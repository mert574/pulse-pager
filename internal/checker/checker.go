// Package checker runs a single HTTP check for a monitor and turns the outcome
// into a domain.CheckResult. It applies the assertions from PRD 4.2 in priority
// order and owns the SSRF guard and the status-code matching. It does not write
// results anywhere: the caller persists them and feeds alerting. That keeps this
// package free of store and alerting imports and easy to test with httptest.
package checker

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pulse/internal/domain"
)

const (
	defaultBodyCapBytes    = 64 * 1024
	defaultMaxErrorTextLen = 500
)

// Config controls checker behavior. Defaults are filled in by New when a field
// is left at its zero value.
type Config struct {
	BlockPrivateNetworks bool  // PULSE_BLOCK_PRIVATE_NETWORKS
	BodyCapBytes         int64 // 64 * 1024
	MaxErrorTextLen      int   // truncate ErrorText, e.g. 500
	// RootCAs verifies ssl-monitor certificate chains. nil = the system roots
	// (production). Tests set it to trust a self-signed cert (BACKLOG: SSL-expiry).
	RootCAs *x509.CertPool
}

// Checker holds the shared http.Client. The client has no global Timeout: each
// check carries its own context deadline from the monitor's TimeoutSeconds.
type Checker struct {
	cfg    Config
	client *http.Client
}

// New builds a Checker. When BlockPrivateNetworks is on, the client's Transport
// dials through a net.Dialer whose Control func re-checks the actual connected
// IP, so a private target is refused at dial time even if DNS changed after our
// pre-resolve.
func New(cfg Config) *Checker {
	if cfg.BodyCapBytes <= 0 {
		cfg.BodyCapBytes = defaultBodyCapBytes
	}
	if cfg.MaxErrorTextLen <= 0 {
		cfg.MaxErrorTextLen = defaultMaxErrorTextLen
	}

	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if cfg.BlockPrivateNetworks {
		dialer.Control = dialControl
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Checker{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			// No global Timeout: the per-check context is the deadline.
		},
	}
}

// Check runs the monitor's HTTP request and returns a fully populated result.
// CheckedAt is the request start time in UTC. StatusCode and LatencyMs are
// filled whenever known, even on failure, for context.
//
// captureResponse controls whether a response-level failure captures the
// response into result.Snapshot (PRD-002 3.8). When false the failure body is
// never read for a snapshot, so a caller can skip the cost for orgs whose plan
// does not include the feature (RFC-009). It does not change the healthy/unhealthy
// decision; body_contains is still read and asserted regardless.
func (c *Checker) Check(ctx context.Context, m *domain.Monitor, captureResponse bool) *domain.CheckResult {
	// Dispatch on the monitor type. ssl checks a TLS cert's expiry (BACKLOG:
	// SSL-expiry); everything else (http, and an empty type from older rows) runs
	// the HTTP check.
	if m.Type == domain.MonitorSSL {
		return c.checkSSL(ctx, m)
	}
	return c.checkHTTP(ctx, m, captureResponse)
}

func (c *Checker) checkHTTP(ctx context.Context, m *domain.Monitor, captureResponse bool) *domain.CheckResult {
	res := &domain.CheckResult{
		MonitorID: m.ID,
		Healthy:   false,
	}

	// SSRF pre-resolve. Done before anything else so we return blocked_target
	// without sending bytes. We use the URL host here; a parse failure shows up
	// later as a connection error when we build the request.
	if c.cfg.BlockPrivateNetworks {
		if host := hostFromURL(m.URL); host != "" {
			if err := resolveAndCheck(host); err != nil {
				if isBlockedErr(err) {
					res.CheckedAt = time.Now().UTC()
					return withReason(res, domain.ReasonBlockedTarget, "")
				}
				// A resolve failure that is not a block (name does not exist)
				// is a connection error: we could not reach the target.
				res.CheckedAt = time.Now().UTC()
				return withReason(res, domain.ReasonConnectionError, c.truncate(err.Error()))
			}
		}
	}

	// Parse the status matcher up front. expected_status_codes is optional: an empty
	// spec means "do not assert the status" (the monitor relies on its other
	// assertions instead), so we skip the status check entirely. A non-empty but
	// invalid spec means the monitor was stored with a bad value; treat it as a
	// status mismatch since we cannot decide what counts as healthy.
	checkStatus := strings.TrimSpace(m.ExpectedStatusCodes) != ""
	var (
		matcher    StatusMatcher
		matcherErr error
	)
	if checkStatus {
		matcher, matcherErr = ParseStatusCodes(m.ExpectedStatusCodes)
	}

	// Per-check timeout. This deadline covers the dial, the request, and the
	// body read, so a slow body still counts against TimeoutSeconds.
	reqCtx := ctx
	var cancel context.CancelFunc
	if m.TimeoutSeconds > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, string(m.Method), m.URL, bodyReader(m))
	if err != nil {
		res.CheckedAt = time.Now().UTC()
		return withReason(res, domain.ReasonConnectionError, c.truncate(err.Error()))
	}
	for _, h := range m.Headers {
		req.Header.Set(h.Key, h.Value)
	}

	start := time.Now()
	res.CheckedAt = start.UTC()

	resp, err := c.client.Do(req)
	if err != nil {
		// Distinguish a timeout from a generic connection error. A deadline on
		// our per-check context is a timeout; everything else is a connection
		// error (refused, reset, DNS, dial-time block, and so on).
		reason := domain.ReasonConnectionError
		if isTimeoutErr(reqCtx, err) {
			reason = domain.ReasonTimeout
		}
		return withReason(res, reason, c.truncate(err.Error()))
	}
	defer resp.Body.Close()

	status := resp.StatusCode
	res.StatusCode = &status

	// Read the body when there is a body assertion. For body_contains the read
	// time counts toward latency (PRD-002 3.5), so it happens before latency is
	// recorded. Otherwise defer: a healthy check drains and discards (fast path),
	// a failing check reads it lazily for the snapshot (PRD-002 3.8).
	var (
		bodyText      string
		bodyTruncated bool
		bodyRead      bool
	)
	if m.BodyContains != nil {
		text, truncated, readErr := readBodyCapped(resp.Body, c.cfg.BodyCapBytes)
		if readErr != nil {
			// A read error after the response arrived is still a failure to
			// complete the request: timeout if the deadline tripped, else
			// connection error. We have the status code, so keep it for context.
			latency := int(time.Since(start).Milliseconds())
			res.LatencyMs = &latency
			reason := domain.ReasonConnectionError
			if isTimeoutErr(reqCtx, readErr) {
				reason = domain.ReasonTimeout
			}
			return withReason(res, reason, c.truncate(readErr.Error()))
		}
		bodyText, bodyTruncated, bodyRead = text, truncated, true
	}

	latency := int(time.Since(start).Milliseconds())
	res.LatencyMs = &latency

	// snapshot captures the response for a debug record on a response-level
	// failure (PRD-002 3.8). It reads the body lazily if we have not already
	// (e.g. a status_mismatch on a monitor with no body_contains), best-effort.
	snapshot := func() *domain.ResponseSnapshot {
		if !bodyRead {
			bodyText, bodyTruncated, _ = readBodyCapped(resp.Body, c.cfg.BodyCapBytes)
			bodyRead = true
		}
		return &domain.ResponseSnapshot{
			StatusCode: &status,
			Headers:    map[string][]string(resp.Header),
			Body:       bodyText,
			Truncated:  bodyTruncated,
		}
	}

	// Assertions in PRD 4.2 priority order: status, then latency, then body.
	// blocked_target and connection/timeout are already handled above.
	if checkStatus && (matcherErr != nil || matcher == nil || !matcher.Matches(status)) {
		if captureResponse {
			res.Snapshot = snapshot()
		}
		return withReason(res, domain.ReasonStatusMismatch, "")
	}
	if m.MaxLatencyMs != nil && latency > *m.MaxLatencyMs {
		if captureResponse {
			res.Snapshot = snapshot()
		}
		return withReason(res, domain.ReasonLatencyExceeded, "")
	}
	if m.BodyContains != nil && !strings.Contains(bodyText, *m.BodyContains) {
		if captureResponse {
			res.Snapshot = snapshot()
		}
		return withReason(res, domain.ReasonBodyAssertion, "")
	}

	// Healthy. If we never read the body, drain a little so keep-alive can reuse
	// the connection, then close.
	if !bodyRead {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	}

	res.Healthy = true
	res.FailureReason = nil
	return res
}

// readBodyCapped reads up to cap bytes of the body. It reads one extra byte to
// detect a body longer than the cap (truncated), and returns it trimmed to cap.
func readBodyCapped(r io.Reader, capBytes int64) (string, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, capBytes+1))
	if err != nil {
		return "", false, err
	}
	if int64(len(data)) > capBytes {
		return string(data[:capBytes]), true, nil
	}
	return string(data), false, nil
}

// withReason sets the failure reason and optional error text on a result and
// returns it. It leaves Healthy false. errText is stored only when non-empty.
func withReason(res *domain.CheckResult, reason domain.FailureReason, errText string) *domain.CheckResult {
	res.Healthy = false
	r := reason
	res.FailureReason = &r
	if errText != "" {
		t := errText
		res.ErrorText = &t
	}
	return res
}

// truncate shortens a string to MaxErrorTextLen runes so ErrorText stays small.
func (c *Checker) truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= c.cfg.MaxErrorTextLen {
		return s
	}
	runes := []rune(s)
	if len(runes) <= c.cfg.MaxErrorTextLen {
		return s
	}
	return string(runes[:c.cfg.MaxErrorTextLen])
}

// bodyReader returns the request body for methods that take one. GET, HEAD, and
// DELETE send no body even if Body is set (validation rejects that on write).
func bodyReader(m *domain.Monitor) io.Reader {
	if m.Body == "" {
		return nil
	}
	switch m.Method {
	case "POST", "PUT", "PATCH":
		return strings.NewReader(m.Body)
	default:
		return nil
	}
}

// hostFromURL pulls the hostname out of a URL string. A parse failure or a URL
// without a host returns an empty string, which tells the caller to skip the
// pre-resolve and let request building surface the error.
func hostFromURL(raw string) string {
	// Same parser http.NewRequest uses, so the host we check matches the host
	// the request will dial.
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return ""
	}
	return u.Hostname()
}

// isBlockedErr reports whether an error came from our SSRF block rather than a
// plain resolve failure. We tag block errors with a known substring so we do
// not depend on error wrapping across the two callers.
func isBlockedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "blocked address")
}

// isTimeoutErr reports whether an error is due to the per-check deadline. We
// check the context first (most reliable), then fall back to the net timeout
// interface for cases the transport reports without setting ctx.Err.
func isTimeoutErr(ctx context.Context, err error) bool {
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

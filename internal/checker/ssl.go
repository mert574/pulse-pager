package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"pulse/internal/domain"
)

// defaultTLSPort is dialed when the monitor URL carries no explicit port.
const defaultTLSPort = "443"

// checkSSL connects to the monitor's host, reads its TLS leaf certificate, and
// decides health from the certificate's expiry (BACKLOG: SSL-expiry). We notify
// at fixed thresholds (domain.SSLWarnThresholds) and once expired, so this only
// produces the verdict + the cert detail; the escalation lives in alerting.
//
// We dial with InsecureSkipVerify so we still obtain the cert when it is expired
// or otherwise invalid (a verifying handshake would fail before we could read
// NotAfter), then verify the chain ourselves to tell cert_invalid from a healthy
// or expiring cert.
func (c *Checker) checkSSL(ctx context.Context, m *domain.Monitor) *domain.CheckResult {
	now := time.Now()
	res := &domain.CheckResult{
		MonitorID: m.ID,
		Healthy:   false,
		CheckedAt: now.UTC(),
	}

	host, port := sslHostPort(m.URL)
	if host == "" {
		return withReason(res, domain.ReasonConnectionError, "ssl monitor url has no host")
	}

	// SSRF pre-resolve, same guard as the HTTP path: refuse a private target
	// without sending a byte.
	if c.cfg.BlockPrivateNetworks {
		if err := resolveAndCheck(host); err != nil {
			if isBlockedErr(err) {
				return withReason(res, domain.ReasonBlockedTarget, "")
			}
			return withReason(res, domain.ReasonConnectionError, c.truncate(err.Error()))
		}
	}

	reqCtx := ctx
	if m.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	netDialer := &net.Dialer{Timeout: 30 * time.Second}
	if c.cfg.BlockPrivateNetworks {
		netDialer.Control = dialControl
	}
	// InsecureSkipVerify: we verify the chain manually below so an expired or
	// invalid cert is still readable. ServerName drives SNI so we get the right
	// cert from hosts that serve several.
	tlsDialer := &tls.Dialer{
		NetDialer: netDialer,
		Config:    &tls.Config{InsecureSkipVerify: true, ServerName: host}, //nolint:gosec // verified manually
	}

	start := time.Now()
	conn, err := tlsDialer.DialContext(reqCtx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		reason := domain.ReasonConnectionError
		if isTimeoutErr(reqCtx, err) {
			reason = domain.ReasonTimeout
		}
		return withReason(res, reason, c.truncate(err.Error()))
	}
	defer conn.Close()

	latency := int(time.Since(start).Milliseconds())
	res.LatencyMs = &latency

	state := conn.(*tls.Conn).ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return withReason(res, domain.ReasonConnectionError, "no peer certificate")
	}
	leaf := state.PeerCertificates[0]

	// We have the leaf: record the expiry and the detail for the card, even when
	// the cert turns out to be invalid below.
	notAfter := leaf.NotAfter
	res.CertExpiresAt = &notAfter
	res.CertInfo = certInfo(leaf)

	// Verify the chain and hostname as of now. An expired cert is the common
	// "down" case and gets its own reason; any other verify failure is invalid.
	intermediates := x509.NewCertPool()
	for _, ci := range state.PeerCertificates[1:] {
		intermediates.AddCert(ci)
	}
	if _, verr := leaf.Verify(x509.VerifyOptions{
		DNSName:       host,
		Intermediates: intermediates,
		Roots:         c.cfg.RootCAs, // nil = system roots
		CurrentTime:   now,
	}); verr != nil {
		var invalid x509.CertificateInvalidError
		if errors.As(verr, &invalid) && invalid.Reason == x509.Expired {
			return withReason(res, domain.ReasonCertExpired, expiryText(notAfter, now))
		}
		return withReason(res, domain.ReasonCertInvalid, c.truncate(verr.Error()))
	}

	// Valid right now. Warn when inside a threshold window (<= 7 days).
	if domain.SSLWarnLevel(notAfter, now) >= 1 {
		return withReason(res, domain.ReasonCertExpiringSoon, expiryText(notAfter, now))
	}

	res.Healthy = true
	res.FailureReason = nil
	return res
}

// sslHostPort pulls the host and port to dial from an ssl monitor's url. It
// accepts a full URL ("https://example.com[:port]") or a bare "host[:port]";
// the port defaults to 443.
func sslHostPort(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		port := u.Port()
		if port == "" {
			port = defaultTLSPort
		}
		return u.Hostname(), port
	}
	// Bare host or host:port (no scheme).
	if host, port, err := net.SplitHostPort(raw); err == nil {
		return host, port
	}
	if raw == "" {
		return "", ""
	}
	return raw, defaultTLSPort
}

// certInfo builds the persisted cert detail from a leaf certificate.
func certInfo(leaf *x509.Certificate) *domain.CertInfo {
	return &domain.CertInfo{
		Subject:   leaf.Subject.CommonName,
		Issuer:    leaf.Issuer.CommonName,
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		DNSNames:  leaf.DNSNames,
		Serial:    leaf.SerialNumber.String(),
	}
}

// expiryText is the short human message stored on a warning/expired result.
func expiryText(notAfter, now time.Time) string {
	if !now.Before(notAfter) {
		return fmt.Sprintf("certificate expired on %s", notAfter.UTC().Format("2006-01-02"))
	}
	days := int(notAfter.Sub(now).Hours() / 24)
	return fmt.Sprintf("certificate expires in %d days (%s)", days, notAfter.UTC().Format("2006-01-02"))
}

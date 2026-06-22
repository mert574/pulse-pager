// Package notify renders uptime events into channel-specific messages and sends
// them. The Manager fans an event out to every attached channel concurrently and
// retries each channel a few times with backoff. Providers themselves are a
// single attempt with no retry, so they stay small and easy to test.
//
// Channel types are descriptor-driven: each type declares a Descriptor (config
// schema, which fields are secret, plan-gating capability) and a Provider (the
// delivery call). The Manager looks providers up through a Registry by channel
// type, so adding a channel type is one Provider plus one Descriptor plus a
// Register call, with no per-type branch anywhere else. See RFC-007a.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"pulse/internal/domain"
)

// Event is one thing that happened to a monitor that we want to tell people about.
type Event struct {
	EventType       string // "down" | "recovery"
	Monitor         domain.Monitor
	Incident        domain.Incident
	Check           domain.CheckResult
	DurationSeconds *int // set on recovery only
	SentAt          time.Time
	// Test marks a "send test message" delivery. Providers render a clearly
	// labelled test instead of a real down/recovery message.
	Test bool
	// ChannelName is the channel's display name, used in the test message text.
	ChannelName string
}

const (
	EventDown     = "down"
	EventRecovery = "recovery"
)

// defaultRegistry is the package-level registry every provider file registers
// into via init. Default() returns it.
var defaultRegistry = NewRegistry()

// Register adds a descriptor to the package-level default registry. Provider
// files call this from init so Default() is fully populated.
func Register(d Descriptor) { defaultRegistry.Register(d) }

// Default returns the package-level registry populated with all built-in
// providers (slack, discord, webhook, smtp, pagerduty, opsgenie, telegram,
// teams, twilio).
func Default() *Registry { return defaultRegistry }

// httpClientOrDefault returns c, or a sane default client if c is nil, so a
// provider built by a Factory without an injected client still works.
func httpClientOrDefault(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// clientAware is implemented by providers that need the Manager's http.Client.
// The Manager injects its client when it builds a provider so tests can pass an
// httptest client through.
type clientAware interface {
	setClient(*http.Client)
}

// Manager owns the registry and runs the dispatch loop.
type Manager struct {
	registry   *Registry
	client     *http.Client
	logger     *slog.Logger
	maxRetries int
	backoff    func(attempt int) time.Duration
}

// defaultBackoff sleeps 1s, 4s, 9s ... (attempt squared seconds), capped at 30s.
func defaultBackoff(attempt int) time.Duration {
	d := time.Duration(attempt*attempt) * time.Second
	const cap = 30 * time.Second
	if d > cap {
		return cap
	}
	return d
}

// NewManager builds a Manager backed by the default registry. If client is nil a
// default client with a sane timeout is used. If logger is nil the slog default
// is used.
func NewManager(client *http.Client, logger *slog.Logger) *Manager {
	return NewManagerWithRegistry(Default(), client, logger)
}

// NewManagerWithRegistry is NewManager with an explicit registry, for tests or a
// trimmed-down provider set.
func NewManagerWithRegistry(reg *Registry, client *http.Client, logger *slog.Logger) *Manager {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		registry:   reg,
		client:     client,
		logger:     logger,
		maxRetries: 3,
		backoff:    defaultBackoff,
	}
}

// SetRetryPolicy overrides the per-channel attempt count and the backoff between
// attempts. It is for callers that need different timing than the defaults (3
// attempts, 1s/4s/9s backoff), mainly tests that must stay fast. attempts < 1 is
// clamped to 1; a nil backoff keeps the current one.
func (mgr *Manager) SetRetryPolicy(attempts int, backoff func(attempt int) time.Duration) {
	if attempts < 1 {
		attempts = 1
	}
	mgr.maxRetries = attempts
	if backoff != nil {
		mgr.backoff = backoff
	}
}

// provider builds a fresh Provider for a channel type and injects the Manager's
// http client if the provider wants one.
func (mgr *Manager) provider(t domain.ChannelType) (Provider, bool) {
	d, ok := mgr.registry.Get(t)
	if !ok {
		return nil, false
	}
	p := d.Factory()
	if ca, ok := p.(clientAware); ok {
		ca.setClient(mgr.client)
	}
	return p, true
}

// Dispatch sends the event to every channel. Each channel runs in its own
// goroutine and retries up to maxRetries with backoff between failures. One
// channel failing does not block the others. Dispatch blocks until all channel
// goroutines finish so the caller knows the work is done.
func (mgr *Manager) Dispatch(ctx context.Context, ev Event, channels []*domain.Channel) {
	var wg sync.WaitGroup
	for _, ch := range channels {
		if ch == nil || !ch.Enabled {
			continue
		}
		wg.Add(1)
		go func(ch *domain.Channel) {
			defer wg.Done()
			mgr.sendWithRetry(ctx, ev, ch)
		}(ch)
	}
	wg.Wait()
}

// sendWithRetry runs the per-channel attempt loop. After the last failure it logs
// at error level so the give-up is visible.
func (mgr *Manager) sendWithRetry(ctx context.Context, ev Event, ch *domain.Channel) {
	p, ok := mgr.provider(ch.Type)
	if !ok {
		mgr.logger.Error("notify: no provider for channel type",
			"channel_id", ch.ID, "type", ch.Type)
		return
	}

	var lastErr error
	for attempt := 1; attempt <= mgr.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			mgr.logger.Error("notify: context canceled before send",
				"channel_id", ch.ID, "type", ch.Type, "err", err)
			return
		}
		lastErr = p.Send(ctx, ch.Config, ev)
		if lastErr == nil {
			return
		}
		mgr.logger.Warn("notify: send attempt failed",
			"channel_id", ch.ID, "type", ch.Type,
			"attempt", attempt, "max", mgr.maxRetries, "err", lastErr)
		if attempt < mgr.maxRetries {
			if !sleepCtx(ctx, mgr.backoff(attempt)) {
				mgr.logger.Error("notify: context canceled during backoff",
					"channel_id", ch.ID, "type", ch.Type)
				return
			}
		}
	}
	mgr.logger.Error("notify: giving up after retries",
		"channel_id", ch.ID, "type", ch.Type,
		"attempts", mgr.maxRetries, "err", lastErr)
}

// Test sends a test message to a single channel. It is the UI "send test" entry.
func (mgr *Manager) Test(ctx context.Context, ch *domain.Channel) error {
	p, ok := mgr.provider(ch.Type)
	if !ok {
		return fmt.Errorf("notify: no provider for channel type %q", ch.Type)
	}
	ev := Event{Test: true, ChannelName: ch.Name, SentAt: time.Now()}
	return p.Send(ctx, ch.Config, ev)
}

// sleepCtx waits for d or until ctx is done. It returns true if it slept the full
// duration, false if ctx was canceled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// cfgString reads a string config value. Numbers are accepted and converted so a
// port given as a JSON number still works.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// JSON numbers decode to float64; render without a trailing .0 for ints.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	case bool:
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

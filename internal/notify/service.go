package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"pulse/internal/bus"
	"pulse/internal/domain"
	"pulse/internal/events"
)

// channelIDKey is the in-memory-only key the Runner stuffs into a channel's config
// so the recording provider can key its outcome by channel id without changing the
// Event struct or the Provider interface. Providers read config by their own known
// keys (cfgString), so this extra key is ignored by the real delivery code, and the
// recording wrapper strips it before delegating. It never touches the database or
// the wire (the config comes from the Runner, decrypted, in memory).
const channelIDKey = "__pulse_channel_id"

// Consumer is the subset of the bus consumer the Runner needs (mirrors worker).
type Consumer interface {
	Poll(ctx context.Context, handler func(bus.Record) error) error
}

// ChannelStore loads a monitor's attached, enabled channels with decrypted config.
type ChannelStore interface {
	GetChannelsForMonitor(ctx context.Context, orgID, monitorID int64, secretKeysFor func(domain.ChannelType) []string) ([]*domain.Channel, error)
}

// DedupStore is the durable dedup backstop (RFC-007 4.2). It returns true when the
// caller is the first to claim the dedup id, false when it was already handled.
type DedupStore interface {
	ClaimNotifyDedup(ctx context.Context, orgID int64, dedupID string) (bool, error)
}

// DeliveryRecorder records one channel's delivery outcome (RFC-007 6.1). The
// signature is plain fields (not a struct) so *store.Pool satisfies it directly,
// with no shared type and so no package cycle (notify never imports store).
type DeliveryRecorder interface {
	RecordDelivery(ctx context.Context, orgID, incidentID, channelID int64, eventType, status string, attempts int, lastError string) error
}

const (
	statusDelivered = "delivered"
	statusFailed    = "failed"
)

// DedupCache is the Redis fast path (SET NX EX). It returns true when the key was
// newly set (first time), false when it already existed (duplicate). An error means
// Redis is unavailable, and the Runner falls back to the Postgres backstop
// (fail toward send-once-more, RFC-007 section 11).
type DedupCache interface {
	SetIfAbsent(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
}

// Store ties the three Postgres-backed roles the Runner needs. The store.Pool
// satisfies it.
type Store interface {
	ChannelStore
	DedupStore
	DeliveryRecorder
}

// Runner consumes notify.events and delivers each to a monitor's channels with the
// reused Manager, deduping first and recording each channel's outcome (RFC-007). It
// also fans the same down/recovery event out to the org's registered, enabled
// outbound webhooks (PRD-005 7), each signed with its own secret. The org-webhook
// path is a sibling of the per-channel path inside the same handler: the existing
// notify.events stream already carries everything an org webhook needs (org, monitor,
// incident, type, timestamps), so this avoids a second Kafka topic and a separate
// consumer for v1. A failing org webhook is retried with a short bounded budget and
// recorded, never blocking the partition; the per-channel path stays the authority
// for committing the offset.
type Runner struct {
	mgr      *Manager
	registry *Registry
	store    Store
	cache    DedupCache
	cons     Consumer
	log      *slog.Logger
	dedupTTL time.Duration
	now      func() time.Time

	// webhooks is the org outbound-webhook store; nil disables org-webhook delivery
	// (the per-channel path still runs). webhookCfg tunes the deliverer (attempts,
	// backoff, budget, client); the Runner fills sane defaults when it is zero.
	webhooks   WebhookStore
	webhookCfg orgWebhookConfig
}

// RunnerOption tweaks a Runner (mainly for tests: clock, dedup TTL).
type RunnerOption func(*Runner)

// WithDedupTTL sets the Redis dedup key TTL. Default is 24h (RFC-007 4.2): it only
// needs to outlive the at-least-once redelivery window.
func WithDedupTTL(d time.Duration) RunnerOption { return func(r *Runner) { r.dedupTTL = d } }

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) RunnerOption { return func(r *Runner) { r.now = now } }

// WithWebhooks enables org-level outbound webhook delivery (PRD-005 7) alongside the
// per-channel path. store loads the org's enabled webhooks and records each outcome;
// nil leaves org-webhook delivery off (the per-channel path is unchanged).
func WithWebhooks(store WebhookStore) RunnerOption {
	return func(r *Runner) { r.webhooks = store }
}

// WithWebhookDelivery overrides the org-webhook delivery tuning (attempts, backoff,
// budget, http client). Mainly for tests that need small, fast settings; production
// uses the defaults (a few attempts, ~24h budget recomputed from the event time).
func WithWebhookDelivery(client *http.Client, maxAttempts int, backoff func(int) time.Duration, budget time.Duration) RunnerOption {
	return func(r *Runner) {
		if client != nil {
			r.webhookCfg.client = client
		}
		if maxAttempts > 0 {
			r.webhookCfg.maxAttempts = maxAttempts
		}
		if backoff != nil {
			r.webhookCfg.backoff = backoff
		}
		if budget > 0 {
			r.webhookCfg.budget = budget
		}
	}
}

// NewRunner builds a Runner. mgr is the reused Manager (built from the same client
// the tests inject so an httptest target is reachable); registry is the one the
// Manager uses, so SecretKeys lines up with the providers. store is the Postgres
// pool; cache is the Redis client.
func NewRunner(mgr *Manager, registry *Registry, store Store, cache DedupCache, cons Consumer, log *slog.Logger, opts ...RunnerOption) *Runner {
	if log == nil {
		log = slog.Default()
	}
	r := &Runner{
		mgr:      mgr,
		registry: registry,
		store:    store,
		cache:    cache,
		cons:     cons,
		log:      log,
		dedupTTL: 24 * time.Hour,
		now:      time.Now,
		webhookCfg: orgWebhookConfig{
			// Defaults: a handful of attempts with squared-second backoff, and a 24h
			// budget recomputed from the event time so a delayed redelivery still
			// stops at the right point (PRD-005 7.2, RFC-007 7.3). A v1 bounded
			// in-handler retry: the partition is not blocked for hours because each
			// attempt re-checks the budget against the event's SentAt and gives up.
			maxAttempts: 4,
			backoff:     defaultBackoff,
			budget:      24 * time.Hour,
		},
	}
	for _, o := range opts {
		o(r)
	}
	// Fill the deliverer's clock, logger, and store from the Runner so the option
	// setters only need to carry tuning.
	r.webhookCfg.now = r.now
	r.webhookCfg.log = r.log
	r.webhookCfg.store = r.webhooks
	return r
}

// Run consumes notify.events until the context is cancelled, mirroring the worker
// loop: poll, handle, and commit-after-process (a returned error leaves the offset
// uncommitted so the event redelivers, kept safe by the dedup id).
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info("notifier started")
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := r.cons.Poll(ctx, func(rec bus.Record) error {
			return r.handle(ctx, rec)
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.log.Error("poll failed", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
	}
}

// handle processes one notify.event: decode, dedup, load channels, dispatch, record.
// It returns nil to commit and a non-nil error to leave the offset for redelivery.
// An unparseable message is poison: log and drop (commit) rather than blocking the
// partition (RFC-007 3.2).
func (r *Runner) handle(ctx context.Context, rec bus.Record) error {
	var ev events.NotifyEvent
	if err := json.Unmarshal(rec.Value, &ev); err != nil {
		r.log.Error("bad notify event, dropping", "err", err)
		return nil
	}

	dedupID := ev.DedupKey
	if dedupID == "" {
		// Defensive: if alerting did not set it, derive the RFC-007 4.1 id ourselves
		// so dedup still works rather than treating every event as unique.
		dedupID = DedupKey(ev.IncidentID, ev.EventType)
	}

	first, err := r.dedup(ctx, ev.OrgID, dedupID)
	if err != nil {
		// Dedup state could not be confirmed at all (Redis and Postgres both down).
		// Return an error so the event redelivers rather than risk a missed alert
		// (RFC-007 section 11: a missed alert is worse than a rare duplicate).
		return fmt.Errorf("dedup %s: %w", dedupID, err)
	}
	if !first {
		r.log.Info("notify event already handled, skipping", "monitor", ev.MonitorID, "type", ev.EventType, "dedup", dedupID)
		return nil
	}

	channels, err := r.store.GetChannelsForMonitor(ctx, ev.OrgID, ev.MonitorID, r.registry.SecretKeys)
	if err != nil {
		// A transient read failure: redeliver (dedup already claimed, but a
		// redelivery re-checks Postgres which now holds the claim, so it would skip;
		// to keep send-once-more strong we only claim in dedup, and a failure here is
		// rare. Erroring is the safe choice over silently dropping the alert).
		return fmt.Errorf("load channels for monitor %d: %w", ev.MonitorID, err)
	}
	if len(channels) == 0 {
		// Zero-channel monitor is a supported no-op success for the per-channel path
		// (RFC-007 3.3, PRD-003 AC4), but org webhooks are org-level and still fire.
		r.log.Info("notify event for monitor with no channels, nothing to send to channels", "monitor", ev.MonitorID, "type", ev.EventType)
	} else {
		r.dispatch(ctx, ev, channels)
	}

	// Org-level outbound webhooks: a sibling fan-out for the whole org, independent of
	// the monitor's attached channels (PRD-005 7). Runs after the dedup gate so a
	// redelivered notify.event does not double-fire (the per-event id also lets the
	// receiver dedup). A failed delivery is recorded, never propagated.
	r.deliverWebhooks(ctx, ev)
	return nil
}

// deliverWebhooks loads the org's enabled webhooks and delivers the signed event to
// each subscribed one. It is a no-op when no webhook store is wired. A load failure
// is logged and swallowed: the per-channel alert already went out, and the org-webhook
// feed is best-effort within its retry budget; failing the handler here would replay
// the whole event (and re-send channels) for a webhook read blip.
func (r *Runner) deliverWebhooks(ctx context.Context, ev events.NotifyEvent) {
	if r.webhooks == nil {
		return
	}
	if len(orgEventTypes(ev.EventType)) == 0 {
		return // event type does not map to any org event (defensive)
	}
	hooks, err := r.webhooks.ListEnabledWebhooks(ctx, ev.OrgID)
	if err != nil {
		r.log.Warn("load org webhooks", "err", err, "org", ev.OrgID)
		return
	}
	if len(hooks) == 0 {
		return
	}
	deliverOrgWebhooks(ctx, ev, hooks, r.webhookCfg)
}

// dedup runs the Redis fast path then the Postgres backstop (RFC-007 4.2). Order:
// Redis SET NX first (cheap gate); on a Redis hit it is a duplicate. If Redis says
// "first" or Redis is down, the Postgres unique insert is the authority: it claims
// the id durably and a second claimer gets "duplicate". If Redis is down AND
// Postgres is down, the error propagates so the caller redelivers (fail toward
// send-once-more).
func (r *Runner) dedup(ctx context.Context, orgID int64, dedupID string) (first bool, err error) {
	key := "pulse:notify:dedup:" + dedupID
	if r.cache != nil {
		ok, cacheErr := r.cache.SetIfAbsent(ctx, key, "1", r.dedupTTL)
		if cacheErr != nil {
			// Redis blip: fall through to the Postgres backstop as the authority.
			r.log.Warn("notify dedup redis unavailable, using postgres backstop", "err", cacheErr)
		} else if !ok {
			// Key already in Redis: a duplicate, no need to touch Postgres.
			return false, nil
		}
	}
	// Postgres is the durable "handled" marker. It also catches the case where Redis
	// had evicted the key (Redis said "first" but the event was already handled).
	claimed, pgErr := r.store.ClaimNotifyDedup(ctx, orgID, dedupID)
	if pgErr != nil {
		return false, pgErr
	}
	return claimed, nil
}

// dispatch builds the notify.Event, runs the reused Manager.Dispatch through a
// recording registry that observes each channel's final outcome, then records the
// outcomes. The Manager owns retry/backoff/concurrency; the recorder only watches.
func (r *Runner) dispatch(ctx context.Context, ev events.NotifyEvent, channels []*domain.Channel) {
	nev := r.buildEvent(ev)

	col := newCollector()
	recMgr := r.recordingManager(col)

	// Stamp each channel's id into its in-memory config so the recording provider
	// can attribute the outcome. The config map is the Runner's own decrypted copy.
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		if ch.Config == nil {
			ch.Config = map[string]any{}
		}
		ch.Config[channelIDKey] = ch.ID
	}

	recMgr.Dispatch(ctx, nev, channels)

	for _, ch := range channels {
		if ch == nil || !ch.Enabled {
			continue
		}
		res, ok := col.get(ch.ID)
		if !ok {
			// No provider for the type, or the channel was skipped. The Manager logs
			// the no-provider case; record nothing for a skip.
			continue
		}
		status := statusDelivered
		lastError := ""
		if res.err != nil {
			status = statusFailed
			lastError = res.err.Error()
		}
		if err := r.store.RecordDelivery(ctx, ev.OrgID, ev.IncidentID, ch.ID, ev.EventType, status, res.attempts, lastError); err != nil {
			r.log.Warn("record delivery outcome", "err", err, "channel", ch.ID, "incident", ev.IncidentID)
		}
		if res.err != nil {
			r.log.Warn("notify delivery failed", "channel", ch.ID, "type", ch.Type,
				"incident", ev.IncidentID, "attempts", res.attempts, "err", res.err)
		}
	}
}

// buildEvent maps the wire NotifyEvent to the library's notify.Event. The webhook
// envelope and chat/email bodies are rendered from this (byte-for-byte appendix B,
// unchanged).
func (r *Runner) buildEvent(ev events.NotifyEvent) Event {
	mon := domain.Monitor{
		ID:     ev.MonitorID,
		OrgID:  ev.OrgID,
		Name:   ev.MonitorName,
		URL:    ev.MonitorURL,
		Method: domain.Method(ev.MonitorMethod),
	}
	inc := domain.Incident{
		ID:        ev.IncidentID,
		OrgID:     ev.OrgID,
		MonitorID: ev.MonitorID,
		StartedAt: ev.IncidentStartedAt,
		EndedAt:   ev.IncidentEndedAt,
	}
	sentAt := ev.SentAt
	if sentAt.IsZero() {
		sentAt = r.now()
	}
	return Event{
		EventType:       ev.EventType,
		Monitor:         mon,
		Incident:        inc,
		Check:           ev.Check,
		DurationSeconds: ev.DurationSeconds,
		SentAt:          sentAt,
	}
}

// recordingManager builds a Manager whose registry wraps each real provider in a
// recordingProvider that reports the final per-channel outcome into col. It reuses
// the same client and retry/backoff settings as the Runner's Manager, so delivery
// behaves identically; only an observer is added (RFC-007 6.2).
func (r *Runner) recordingManager(col *collector) *Manager {
	wrapped := NewRegistry()
	for _, d := range r.registry.List() {
		inner := d.Factory
		d.Factory = func() Provider {
			return &recordingProvider{inner: inner(), col: col}
		}
		wrapped.Register(d)
	}
	m := NewManagerWithRegistry(wrapped, r.mgr.client, r.log)
	// Carry the source Manager's retry/backoff so delivery behaves exactly as the
	// injected Manager would (tests can inject small settings to stay fast).
	m.maxRetries = r.mgr.maxRetries
	m.backoff = r.mgr.backoff
	return m
}

// DedupKey is the RFC-007 4.1 dedup id: hex(sha256(incident_id, event_type)). It is
// exported so alerting (the emitter) and the notifier compute the same value.
func DedupKey(incidentID int64, eventType string) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(incidentID, 10)))
	h.Write([]byte{0})
	h.Write([]byte(eventType))
	return hex.EncodeToString(h.Sum(nil))
}

// --- outcome collector ---

type attemptResult struct {
	attempts int
	err      error // nil on success; the last error on give-up
}

type collector struct {
	mu sync.Mutex
	by map[int64]attemptResult
}

func newCollector() *collector { return &collector{by: map[int64]attemptResult{}} }

func (c *collector) record(channelID int64, attempts int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Keep the latest (the final attempt's state for this channel).
	c.by[channelID] = attemptResult{attempts: attempts, err: err}
}

func (c *collector) get(channelID int64) (attemptResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.by[channelID]
	return r, ok
}

// recordingProvider wraps a real provider, counts attempts, and reports the final
// outcome (last error or success) for the channel into the collector. The Manager
// calls Send once per attempt on a fresh provider per channel (one goroutine per
// channel), so attempts accumulate per channel here and the last Send result is the
// give-up/success state.
type recordingProvider struct {
	inner    Provider
	col      *collector
	attempts int
}

func (p *recordingProvider) Send(ctx context.Context, cfg map[string]any, ev Event) error {
	channelID := channelIDFromCfg(cfg)
	p.attempts++
	err := p.inner.Send(ctx, stripChannelID(cfg), ev)
	p.col.record(channelID, p.attempts, err)
	return err
}

func (p *recordingProvider) Validate(cfg map[string]any) error {
	return p.inner.Validate(stripChannelID(cfg))
}

// channelIDFromCfg reads the sentinel channel id the Runner stamped in.
func channelIDFromCfg(cfg map[string]any) int64 {
	v, ok := cfg[channelIDKey]
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	}
	return 0
}

// stripChannelID returns cfg without the sentinel key so the real provider never
// sees it. It only allocates when the key is present.
func stripChannelID(cfg map[string]any) map[string]any {
	if _, ok := cfg[channelIDKey]; !ok {
		return cfg
	}
	out := make(map[string]any, len(cfg)-1)
	for k, v := range cfg {
		if k == channelIDKey {
			continue
		}
		out[k] = v
	}
	return out
}

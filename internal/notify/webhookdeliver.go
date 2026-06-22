package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"pulse/internal/domain"
	"pulse/internal/events"
)

// This file delivers org-level outbound webhooks (PRD-005 section 7, RFC-007 section
// 7). It is distinct from the per-monitor generic-webhook channel (webhook.go): an
// org webhook is a programmatic event feed for the whole org. The Runner calls it
// alongside per-channel delivery for the same notify.event, so a down/recovery that
// alerting emits also fans out to the org's registered, enabled webhooks. Each
// delivery is signed with the per-webhook secret and carries a unique event id so a
// receiver can dedup an at-least-once redelivery.

// signatureHeader is the header carrying the timestamp and the HMAC (PRD-005 7.2,
// RFC-007 7.2): X-Pulse-Signature: t=<unixts>,v1=<hex hmac-sha256(<ts>.<rawbody>)>.
const signatureHeader = "X-Pulse-Signature"

// WebhookStore loads an org's enabled webhooks and records each delivery outcome.
// *store.Pool satisfies it; the signature uses plain types so notify never imports
// store (no package cycle), matching the DeliveryRecorder pattern.
type WebhookStore interface {
	ListEnabledWebhooks(ctx context.Context, orgID int64) ([]*domain.OrgWebhook, error)
	RecordWebhookDelivery(ctx context.Context, orgID, id int64, status, lastError string) error
}

// orgEventEnvelope is the JSON body POSTed to an org webhook (PRD-005 7.3): a stable
// envelope with a unique event id, the event type, the org, an occurred-at time, and
// a data object carrying the monitor + incident snapshot. The receiver dedups on
// event_id and verifies the signature over the raw body.
type orgEventEnvelope struct {
	EventID    string       `json:"event_id"`
	Event      string       `json:"event"`
	OrgID      string       `json:"org_id"`
	OccurredAt string       `json:"occurred_at"`
	Data       orgEventData `json:"data"`
}

type orgEventData struct {
	Monitor  orgEventMonitor  `json:"monitor"`
	Incident orgEventIncident `json:"incident"`
}

type orgEventMonitor struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Method string `json:"method"`
}

type orgEventIncident struct {
	ID              string  `json:"id"`
	StartedAt       string  `json:"started_at"`
	EndedAt         *string `json:"ended_at"`         // null while open, set on recovery
	DurationSeconds *int    `json:"duration_seconds"` // recovery only
}

// orgEventTypes maps a notify event type (down/recovery) to the org event types it
// fans out to. A down emits monitor.down + incident.opened; a recovery emits
// monitor.recovery + incident.closed (PRD-005 7.1). A webhook receives only the
// types it subscribes to.
func orgEventTypes(notifyEventType string) []domain.OrgWebhookEvent {
	switch notifyEventType {
	case EventDown:
		return []domain.OrgWebhookEvent{domain.OrgEventMonitorDown, domain.OrgEventIncidentOpened}
	case EventRecovery:
		return []domain.OrgWebhookEvent{domain.OrgEventMonitorRecover, domain.OrgEventIncidentClosed}
	}
	return nil
}

// orgWebhookConfig is the delivery tuning the Runner passes in. It is small on
// purpose so tests can shrink the retry budget and timeout and stay fast.
type orgWebhookConfig struct {
	store       WebhookStore
	client      *http.Client
	maxAttempts int // total POST attempts per webhook per event
	backoff     func(attempt int) time.Duration
	budget      time.Duration // give up once the event is older than this (recomputed from event time)
	now         func() time.Time
	log         *slog.Logger
}

// deliverOrgWebhooks fans one notify.event out to the org's enabled webhooks. For
// each org event type the notify.event maps to, it builds the signed envelope and
// POSTs it to every webhook subscribed to that type, then records the outcome on the
// webhook row so a broken receiver is visible. A webhook with no provider is never
// involved (this is its own HTTP path, not the channel registry). It does not return
// an error: a failed delivery is recorded, not propagated, so one bad receiver never
// blocks the partition (the per-channel path stays the authority for committing).
func deliverOrgWebhooks(ctx context.Context, ev events.NotifyEvent, hooks []*domain.OrgWebhook, cfg orgWebhookConfig) {
	types := orgEventTypes(ev.EventType)
	if len(types) == 0 {
		return
	}
	for _, hook := range hooks {
		if hook == nil || !hook.Enabled {
			continue
		}
		for _, et := range types {
			if !hook.Subscribes(et) {
				continue
			}
			deliverOne(ctx, ev, hook, et, cfg)
		}
	}
}

// deliverOne builds the envelope for one (webhook, event type), signs it, and POSTs
// with a bounded retry within the budget. The outcome is recorded on the webhook row.
func deliverOne(ctx context.Context, ev events.NotifyEvent, hook *domain.OrgWebhook, et domain.OrgWebhookEvent, cfg orgWebhookConfig) {
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	log := cfg.log
	if log == nil {
		log = slog.Default()
	}

	body, eventID := buildOrgEnvelope(ev, et, now)
	raw, err := json.Marshal(body)
	if err != nil {
		log.Error("marshal org webhook envelope", "err", err, "webhook", hook.ID)
		return
	}

	var lastErr error
	for attempt := 1; attempt <= cfg.maxAttempts; attempt++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		// Stop retrying once the event has aged past the budget. The anchor is the
		// event's SentAt (the triggering time), so any consumer computes the same
		// remaining budget regardless of restarts (RFC-007 7.3).
		if cfg.budget > 0 && !ev.SentAt.IsZero() && now().Sub(ev.SentAt) > cfg.budget {
			lastErr = fmt.Errorf("retry budget of %s elapsed since the event", cfg.budget)
			break
		}
		lastErr = postSigned(ctx, cfg.client, hook, raw, eventID, now)
		if lastErr == nil {
			break
		}
		log.Warn("org webhook delivery attempt failed", "webhook", hook.ID, "event", et,
			"attempt", attempt, "max", cfg.maxAttempts, "err", lastErr)
		if attempt < cfg.maxAttempts && cfg.backoff != nil {
			if !sleepCtx(ctx, cfg.backoff(attempt)) {
				lastErr = ctx.Err()
				break
			}
		}
	}

	status := statusDelivered
	errText := ""
	if lastErr != nil {
		status = statusFailed
		errText = lastErr.Error()
		log.Warn("org webhook delivery gave up", "webhook", hook.ID, "event", et, "err", lastErr)
	}
	if cfg.store != nil {
		if rerr := cfg.store.RecordWebhookDelivery(ctx, hook.OrgID, hook.ID, status, errText); rerr != nil {
			log.Warn("record org webhook outcome", "err", rerr, "webhook", hook.ID)
		}
	}
}

// postSigned signs the raw body with the webhook's secret and POSTs it. The
// timestamp is bound into the signature so a replay with a stale t does not verify
// (PRD-005 7.2). A non-2xx is an error so the retry loop runs.
func postSigned(ctx context.Context, client *http.Client, hook *domain.OrgWebhook, raw []byte, eventID string, now func() time.Time) error {
	ts := strconv.FormatInt(now().Unix(), 10)
	sig := signBody(hook.SigningSecret, ts, raw)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(signatureHeader, fmt.Sprintf("t=%s,v1=%s", ts, sig))
	req.Header.Set("X-Pulse-Event-Id", eventID)

	c := httpClientOrDefault(client)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// signBody computes hex(hmac-sha256(secret, <ts>.<rawbody>)), the v1 signature
// (PRD-005 7.2, RFC-007 7.2). Exported as SignWebhookBody for tests/receivers that
// recompute it.
func signBody(secret, ts string, raw []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignWebhookBody is the exported signer so receivers and tests verify the
// X-Pulse-Signature: it is hex(hmac-sha256(secret, ts + "." + body)).
func SignWebhookBody(secret, ts string, body []byte) string { return signBody(secret, ts, body) }

// buildOrgEnvelope builds the body and the unique event id for one (event, type).
// The event id is stable per (incident, type) so an at-least-once redelivery of the
// same notify.event carries the same id and the receiver dedups it (PRD-005 7.2).
func buildOrgEnvelope(ev events.NotifyEvent, et domain.OrgWebhookEvent, now func() time.Time) (orgEventEnvelope, string) {
	occurred := ev.SentAt
	if occurred.IsZero() {
		occurred = now()
	}
	eventID := orgEventID(ev.IncidentID, et)

	var endedAt *string
	if ev.IncidentEndedAt != nil {
		s := rfc3339UTC(*ev.IncidentEndedAt)
		endedAt = &s
	}

	env := orgEventEnvelope{
		EventID:    eventID,
		Event:      string(et),
		OrgID:      fmt.Sprintf("org_%d", ev.OrgID),
		OccurredAt: rfc3339UTC(occurred),
		Data: orgEventData{
			Monitor: orgEventMonitor{
				ID:     fmt.Sprintf("mon_%d", ev.MonitorID),
				Name:   ev.MonitorName,
				URL:    ev.MonitorURL,
				Method: ev.MonitorMethod,
			},
			Incident: orgEventIncident{
				ID:              fmt.Sprintf("inc_%d", ev.IncidentID),
				StartedAt:       rfc3339UTC(ev.IncidentStartedAt),
				EndedAt:         endedAt,
				DurationSeconds: ev.DurationSeconds,
			},
		},
	}
	return env, eventID
}

// orgEventID is hex(sha256(incident_id, event_type)) prefixed with evt_, stable per
// (incident, org event type) so a redelivery carries the same id.
func orgEventID(incidentID int64, et domain.OrgWebhookEvent) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(incidentID, 10)))
	h.Write([]byte{0})
	h.Write([]byte(string(et)))
	return "evt_" + hex.EncodeToString(h.Sum(nil))[:32]
}

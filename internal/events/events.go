// Package events holds the JSON payloads carried on the Kafka topics (the
// RFC-002 contracts). They are plain data shared by producers and consumers, so
// like domain this package stays dependency-light. Wire format is JSON (RFC-002
// section 5); timestamps are RFC3339 UTC via time.Time's JSON encoding.
package events

import (
	"time"

	"pulse/internal/domain"
)

// CheckJob is one check to run, produced by the scheduler onto
// check.jobs.<region> and consumed by that region's workers. It carries the full
// monitor config so the worker never reads Postgres on the hot path (RFC-002,
// RFC-000 section 2.3). JobID is the idempotency anchor: <monitorID>:<region>:<scheduledUnix>.
type CheckJob struct {
	JobID       string         `json:"job_id"`
	OrgID       int64          `json:"org_id"`
	Region      string         `json:"region"`
	ScheduledAt time.Time      `json:"scheduled_at"`
	Monitor     domain.Monitor `json:"monitor"`
}

// CheckResultEvent is the outcome of one check, produced by the worker onto
// check.results and consumed by alerting (step 2). ScheduledAt is carried
// through so the multi-region aggregation window can group a tick's regions
// (the RFC-006 ask noted in the RFC consistency review).
type CheckResultEvent struct {
	JobID       string             `json:"job_id"`
	ScheduledAt time.Time          `json:"scheduled_at"`
	Result      domain.CheckResult `json:"result"`
}

// NotifyEvent is the notify.events payload (key: monitor_id), produced by alerting
// when an incident opens (down) or closes by recovery (recovery), and consumed by
// the notifier (RFC-007 section 3). It carries everything the notifier needs to
// build the per-channel message without reading the monitor row on the hot path,
// except the secret channel config, which the notifier loads and decrypts by id.
//
// DedupKey is hex(sha256(incident_id, event_type)) set by alerting (RFC-007 4.1):
// stable per (incident, kind), so a redelivery carries the same key and the down
// and the recovery each fire once. The alerting emitter is built in parallel; this
// struct is the wire contract both sides agree on. ChannelIDs is the monitor's
// attached channels at emit time; an empty slice is the supported zero-channel
// monitor (the notifier dedups, loads nothing, and commits).
type NotifyEvent struct {
	OrgID      int64  `json:"org_id"`
	MonitorID  int64  `json:"monitor_id"`
	IncidentID int64  `json:"incident_id"`
	EventType  string `json:"event_type"` // "down" | "recovery"
	DedupKey   string `json:"dedup_key"`

	// Monitor identity for the message body (the notifier does not re-read it).
	MonitorName   string `json:"monitor_name"`
	MonitorURL    string `json:"monitor_url"`
	MonitorMethod string `json:"monitor_method"`

	// Incident timing, used for the webhook envelope and recovery duration.
	IncidentStartedAt time.Time  `json:"incident_started_at"`
	IncidentEndedAt   *time.Time `json:"incident_ended_at,omitempty"` // set on recovery
	DurationSeconds   *int       `json:"duration_seconds,omitempty"`  // set on recovery

	// Check is the triggering result (reason/status/latency for the body).
	Check domain.CheckResult `json:"check"`

	// ChannelIDs is the monitor's attached channels at emit time. Empty is the
	// supported zero-channel monitor (the notifier loads nothing and commits).
	ChannelIDs []int64 `json:"channel_ids"`

	// RegionsObserved is the regions that saw the round fail (RFC-002 4.5). The
	// notifier renders it as a human-readable line, never a locked-envelope field.
	// Single-region today, so this is the one failing region on a down event.
	RegionsObserved []string `json:"regions_observed_unhealthy,omitempty"`

	// SentAt is when alerting emitted the event; the notifier uses delivery time
	// when this is zero.
	SentAt time.Time `json:"sent_at"`
}

// AuditEvent is produced onto audit.events (key: org_id) for an operator action that
// must leave a trail (RFC-018 5/8: every admin billing action is audited). Actor is the
// operator email; Action is a short verb (e.g. "billing.plan_set", "billing.cancel",
// "billing.refund"); Detail carries action-specific fields (plan, amount, payment id)
// as a small flat map. There is no consumer yet; the api emits best-effort so the trail
// exists for when the audit log is built.
type AuditEvent struct {
	OrgID      int64             `json:"org_id"`
	Actor      string            `json:"actor"`
	Action     string            `json:"action"`
	Detail     map[string]string `json:"detail,omitempty"`
	OccurredAt time.Time         `json:"occurred_at"`
}

// MonitorChangedEvent is produced by the api onto monitor.changed (key: org_id)
// when a monitor is created, updated, enabled, disabled, or deleted, so the
// scheduler picks up the live config change instead of waiting for its next full
// scan (RFC-002 5.1, PRD-006 5). MonitorID is 0 for a change that affects the whole
// org. Deleted marks a removal so a consumer can drop its schedule entry.
type MonitorChangedEvent struct {
	OrgID     int64     `json:"org_id"`
	MonitorID int64     `json:"monitor_id"`
	Deleted   bool      `json:"deleted"`
	ChangedAt time.Time `json:"changed_at"`
}

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

// EmailIntentType discriminates an email.events payload (RFC-019). The notifier
// switches on it to pick the template, the recipient, and the From category. No
// usable credential ever rides these payloads: the notifier mints the magic-link /
// invite token at send time (RFC-019 section 5), so the bus log holds nothing worth
// stealing.
type EmailIntentType string

const (
	EmailMagicLink   EmailIntentType = "magic_link"   // passwordless sign-in link
	EmailInvitation  EmailIntentType = "invitation"   // org invite (first send or resend)
	EmailChannelTest EmailIntentType = "channel_test" // one-off "this channel works" test
)

// EmailIntent is the email.events envelope (key: org_id when set, else email),
// carrying a semantic intent the notifier turns into one email (RFC-019 section 3/4).
// Type selects which payload is set; exactly one is non-nil. Locale is the render
// language for whichever email this is (empty falls back to English), kept at the
// envelope level since every email type renders in some locale. The api used to send
// these inline; now it publishes the intent and the notifier is the only sender.
type EmailIntent struct {
	Type        EmailIntentType       `json:"type"`
	Locale      string                `json:"locale,omitempty"`
	OccurredAt  time.Time             `json:"occurred_at"`
	MagicLink   *MagicLinkRequested   `json:"magic_link,omitempty"`
	Invitation  *InvitationRequested  `json:"invitation,omitempty"`
	ChannelTest *ChannelTestRequested `json:"channel_test,omitempty"`
}

// MagicLinkRequested asks the notifier to mint a one-time sign-in token, store its
// hash in the shared magic-link Redis record the api's Verify reads, and email the
// verify link to Email (RFC-019 section 5.1). The api publishes this for any address
// it is handed, so the flow stays enumeration-safe; the notifier just sends the link.
type MagicLinkRequested struct {
	Email string `json:"email"`
	// Country (a CF-IPCountry code) and UserAgent describe the request that asked for
	// the link, captured by the api handler. The sign-in email turns them into a
	// "requested from" / "device" section so the recipient can spot a request they did
	// not start. Country-level only: Cloudflare gives the country free, not the city,
	// and the origin never sees a meaningful client IP behind the proxy. Both optional;
	// empty when not captured (older intents, no Cloudflare) and the email omits them.
	Country   string `json:"country,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

// InvitationRequested asks the notifier to mint a fresh invite token, write its hash
// to the still-pending invitation row (under WithOrg), and email the accept link
// (RFC-019 section 5.2). One intent covers both the first send and a resend: the
// notifier action is identical (mint, set-hash-where-pending, send). The render
// fields (OrgName, Inviter, Role) ride along because the api already holds them and
// none is secret; the notifier reads the row only to set the token and confirm the
// invite is still pending. Inviter is a display string like "Jane Doe (jane@acme.com)"
// and may be empty, in which case the copy falls back to its passive phrasing.
type InvitationRequested struct {
	InvitationID int64  `json:"invitation_id"`
	OrgID        int64  `json:"org_id"`
	OrgName      string `json:"org_name"`
	Inviter      string `json:"inviter"`
	Role         string `json:"role"`
	Email        string `json:"email"`
}

// ChannelTestRequested asks the notifier to send a one-off "this channel works" test
// to the person who clicked Test only (RequestedByEmail), never the whole channel
// (RFC-019 section 3). It is published only for the Team-email channel, the one test
// that uses the platform mailer; other channel types test synchronously in the api
// against their own destination. ChannelName is carried for the copy so the notifier
// renders without a channel read; ChannelID and OrgID are for logs and scoping.
type ChannelTestRequested struct {
	ChannelID        int64  `json:"channel_id"`
	ChannelName      string `json:"channel_name"`
	OrgID            int64  `json:"org_id"`
	RequestedByEmail string `json:"requested_by_email"`
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

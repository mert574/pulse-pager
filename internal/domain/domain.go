// Package domain holds the plain structs and enums shared across packages.
// It has no behavior and imports nothing from other internal packages, so it is
// the stable vocabulary every other package speaks.
package domain

import "time"

type Method string // "GET","POST","PUT","PATCH","DELETE","HEAD"

// MonitorType is the kind of check. v1 ships http only; the field exists from
// day one so adding types later is additive (PRD-002).
type MonitorType string

const (
	MonitorHTTP MonitorType = "http"
)

// DownPolicy reduces per-region results to one monitor verdict (PRD-007, master 6.7).
type DownPolicy string

const (
	DownPolicyAny    DownPolicy = "any"
	DownPolicyQuorum DownPolicy = "quorum"
	DownPolicyAll    DownPolicy = "all"
)

type Status string // derived, see status.go

const (
	StatusDisabled Status = "disabled"
	StatusPending  Status = "pending"
	StatusDown     Status = "down"
	StatusUp       Status = "up"
)

type FailureReason string

const (
	ReasonConnectionError FailureReason = "connection_error"
	ReasonTimeout         FailureReason = "timeout"
	ReasonStatusMismatch  FailureReason = "status_mismatch"
	ReasonLatencyExceeded FailureReason = "latency_exceeded"
	ReasonBodyAssertion   FailureReason = "body_assertion_failed"
	ReasonBlockedTarget   FailureReason = "blocked_target"
)

type Header struct {
	Key    string `json:"key"`
	Value  string `json:"value"` // decrypted in memory; redacted when serialized to API if Secret
	Secret bool   `json:"secret"`
}

type Monitor struct {
	ID                  int64
	OrgID               int64       // tenant owner; every org-owned entity carries it (RFC-001)
	Type                MonitorType // http in v1
	Name                string
	URL                 string
	Method              Method
	Headers             []Header
	Body                string
	ExpectedStatusCodes string // stored raw, e.g. "200,204" or "2xx"; parsed by checker
	TimeoutSeconds      int
	IntervalSeconds     int
	Enabled             bool
	MaxLatencyMs        *int    // nil = no assertion
	BodyContains        *string // nil = no assertion
	FailureThreshold    int
	Regions             []string   // region codes the monitor is checked from (PRD-007)
	DownPolicy          DownPolicy // how per-region results reduce to one verdict
	ChannelIDs          []int64    // attached channels
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type ChannelType string

const (
	ChannelSlack   ChannelType = "slack"
	ChannelDiscord ChannelType = "discord"
	ChannelWebhook ChannelType = "webhook"
	ChannelSMTP    ChannelType = "smtp"
	// Phased types (PRD-003 section 8), plan-gated. Additive to the attach model.
	ChannelPagerDuty ChannelType = "pagerduty"
	ChannelOpsgenie  ChannelType = "opsgenie"
	ChannelTelegram  ChannelType = "telegram"
	ChannelTeams     ChannelType = "teams"
	ChannelTwilio    ChannelType = "twilio"
)

// Channel.Config is the decrypted type-specific config as a map.
// store encrypts the secret keys on write and decrypts on read; the rest of the
// app sees plaintext values in memory.
type Channel struct {
	ID      int64
	OrgID   int64 // tenant owner
	Name    string
	Type    ChannelType
	Config  map[string]any // typed per Type; secret fields decrypted in memory
	Enabled bool
}

type CheckResult struct {
	ID            int64
	OrgID         int64 // tenant owner
	MonitorID     int64
	Region        string    // the region this check ran from (PRD-007); present from day 0
	ScheduledAt   time.Time // the scheduler tick this check belongs to; same across a run's regions
	CheckedAt     time.Time
	Healthy       bool
	FailureReason *FailureReason // nil when healthy
	StatusCode    *int           // nil on connection error / timeout / blocked
	LatencyMs     *int           // nil when no latency (conn error, blocked)
	ErrorText     *string        // short, truncated
	// Snapshot is the captured response on a response-level failure (PRD-002 3.8).
	// In-memory only (json:"-"): it never rides the check.results event or the
	// result row; the worker persists it to the per-monitor last-failure record.
	Snapshot *ResponseSnapshot `json:"-"`
}

// ResponseSnapshot is the response captured from the last failed check, for
// debugging (PRD-002 3.8). Operational data, not secret. Body is capped.
type ResponseSnapshot struct {
	StatusCode *int
	Headers    map[string][]string
	Body       string
	Truncated  bool // the body was longer than the cap
}

type CloseReason string

const (
	CloseRecovered CloseReason = "recovered"
	CloseDisabled  CloseReason = "disabled"
	CloseManual    CloseReason = "manual" // owner/admin hand-close (PRD-002, RBAC matrix)
)

type Incident struct {
	ID            int64
	OrgID         int64 // tenant owner
	MonitorID     int64
	StartedAt     time.Time  // first failing check in the run that opened it
	EndedAt       *time.Time // nil while open
	CauseReason   FailureReason
	CloseReason   *CloseReason // nil while open
	ClosedBy      *int64       // user id for a manual close, nil otherwise
	FirstResultID *int64       // link to the failing check
}

// IncidentAnnotation is one note an org member added to an incident's timeline
// (PRD-002 4). AuthorUserID is nil when the writer was later removed from the org.
type IncidentAnnotation struct {
	ID           int64
	OrgID        int64
	IncidentID   int64
	AuthorUserID *int64
	Note         string
	CreatedAt    time.Time
}

// AlertState is the per-monitor alerting state, derived from stored data.
type AlertState struct {
	ConsecutiveFails int
	FirstFailAt      *time.Time // first failing check of the current run, nil when count is 0
	OpenIncident     *Incident  // nil if none open
	// LastAppliedResultID is the redelivery watermark (RFC-006 5.3): the largest
	// check_results.id whose round the alerting consumer has applied to this
	// monitor. nil = never applied. The pure Apply does not read it; the alerting
	// service uses it to drop a redelivered or stale result.
	LastAppliedResultID *int64
}

// --- status pages (PRD-004) ---

// StatusPageState is draft or published. Only published pages are publicly
// reachable; a draft's public URL 404s like an unknown slug (PRD-004 6).
type StatusPageState string

const (
	StatusPageDraft     StatusPageState = "draft"
	StatusPagePublished StatusPageState = "published"
)

// StatusPageTheme is the light/dark page theme (PRD-004 2.2).
type StatusPageTheme string

const (
	ThemeLight StatusPageTheme = "light"
	ThemeDark  StatusPageTheme = "dark"
)

// PublicStatus is the reduced, public-safe status of a displayed monitor on a page
// (PRD-004 3.2). The four internal monitor statuses collapse to two public ones;
// a disabled monitor is hidden from the page entirely (PublicHidden), never shown.
type PublicStatus string

const (
	PublicOperational PublicStatus = "operational"
	PublicDown        PublicStatus = "down"
	PublicHidden      PublicStatus = "hidden" // disabled monitor: not rendered on the page
)

// PublicBanner is the overall page banner derived from the mix of visible displayed
// monitor statuses (PRD-004 4).
type PublicBanner string

const (
	BannerOperational PublicBanner = "operational"    // all visible up (or none visible)
	BannerPartial     PublicBanner = "partial_outage" // some but not all visible down
	BannerMajor       PublicBanner = "major_outage"   // every visible monitor down
)

// StatusPage is a public, shareable page scoped to one org (PRD-004 2). Branding is
// name/logo/theme/accent only. State is draft|published. CustomDomain is the phased
// paid feature (PRD-004 9.1), unused in v1.
type StatusPage struct {
	ID           int64
	OrgID        int64
	Name         string
	Slug         string
	LogoURL      string
	AccentColor  string
	Theme        StatusPageTheme
	Published    bool
	CustomDomain *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// State returns the page state derived from the published flag.
func (s *StatusPage) State() StatusPageState {
	if s.Published {
		return StatusPagePublished
	}
	return StatusPageDraft
}

// StatusPageMonitor is one displayed-monitor entry on a page (PRD-004 3.1): a
// reference to a monitor plus the friendly public display name and the display order.
// The display_name is the ONLY label the public sees; the raw monitor url is never
// carried here.
type StatusPageMonitor struct {
	ID          int64
	OrgID       int64
	PageID      int64
	MonitorID   int64
	DisplayName string
	SortOrder   int
}

// UptimeSummary is a displayed monitor's uptime over the three public windows
// (PRD-004 3.3): the percentage 0..100 over 24h, 7d, and 90d, computed from check
// results. A window with no data reports 100 (no known problem) and Has* false so the
// caller can label "no data" rather than imply a real measurement.
type UptimeSummary struct {
	Uptime24h float64
	Uptime7d  float64
	Uptime90d float64
	Has24h    bool
	Has7d     bool
	Has90d    bool
}

// PublicDisplayedMonitor is the public-safe projection of one displayed monitor
// (PRD-004 3.6 left column only): the friendly name, the reduced public status, the
// uptime summary, and a recent up/down history strip. It carries NO raw url, method,
// headers, body, assertions, failure reason, status code, latency, channels, or
// region detail. The internal monitor id is kept only for server-side ordering and
// is not part of the wire DTO.
type PublicDisplayedMonitor struct {
	DisplayName string
	Status      PublicStatus
	Uptime      UptimeSummary
	History     []PublicHistoryPoint // recent up/down per period, oldest..newest
}

// PublicHistoryPoint is one cell of the recent history bar (PRD-004 3.4): up/down for
// a period only. It never carries latency, status code, or a failure reason.
type PublicHistoryPoint struct {
	At Time // period start
	Up bool
}

// PublicIncident is a public incident entry on a page (PRD-004 5.1): the affected
// monitor's friendly name, the start time, and (once closed) the duration. The
// failure reason, status code, latency, and raw url are never shown.
type PublicIncident struct {
	DisplayName     string
	StartedAt       time.Time
	EndedAt         *time.Time
	DurationSeconds *int
	Resolved        bool
}

// PublicStatusPage is the full public-safe projection returned for a published page
// (PRD-004 3.6): branding, the overall banner, the visible displayed monitors, and
// recent public incidents. It is assembled only from left-column fields; no internal
// monitor detail can be present because the store never selects those columns into it.
type PublicStatusPage struct {
	Name            string
	Slug            string
	LogoURL         string
	AccentColor     string
	Theme           StatusPageTheme
	Banner          PublicBanner
	Monitors        []PublicDisplayedMonitor
	Incidents       []PublicIncident
	UptimeMaxWindow string // the longest window with data: "24h" / "7d" / "90d" (PRD-004 3.3)
}

// Time is a type alias so the history-point timestamp reads as time.Time without an
// extra import in callers that already use the domain package.
type Time = time.Time

// DerivePublicStatus reduces a monitor's internal status to its public display status
// (PRD-004 3.2): up -> operational, down -> down, pending -> operational (no known
// problem yet), disabled -> hidden.
func DerivePublicStatus(s Status) PublicStatus {
	switch s {
	case StatusDown:
		return PublicDown
	case StatusDisabled:
		return PublicHidden
	default: // up, pending
		return PublicOperational
	}
}

// DeriveBanner computes the overall page banner from the visible displayed monitors
// (PRD-004 4): D = number down, N = number visible. D=0 -> operational; 0<D<N ->
// partial; D=N (N>0) -> major; N=0 -> operational (empty state). Hidden monitors are
// not counted (the caller passes only visible ones).
func DeriveBanner(statuses []PublicStatus) PublicBanner {
	var n, down int
	for _, s := range statuses {
		if s == PublicHidden {
			continue
		}
		n++
		if s == PublicDown {
			down++
		}
	}
	switch {
	case n == 0 || down == 0:
		return BannerOperational
	case down == n:
		return BannerMajor
	default:
		return BannerPartial
	}
}

// --- identity and tenancy (PRD-001) ---

// User is a person, global to Pulse (not org-scoped). Created on first sign-in.
// Locale/Timezone are the i18n preferences (RFC-014 9), default en/UTC.
type User struct {
	ID                int64
	Email             string // verified identity anchor; account linking matches on it
	EmailVerified     bool
	Name              string
	AvatarURL         string
	Locale            string // BCP-47 tag, e.g. "en", "de", "pt-BR"
	Timezone          string // IANA name, e.g. "UTC", "Europe/Berlin"
	Status            string // active / deletion-pending / deleted
	DeletionPendingAt *time.Time
	CreatedAt         time.Time
	LastLoginAt       *time.Time
}

// IdentityProvider is the social provider an identity comes from. 'sso' is added
// later (RFC-016); the field exists from day one so that is additive.
type IdentityProvider string

const (
	ProviderGoogle IdentityProvider = "google"
	ProviderGitHub IdentityProvider = "github"
	// ProviderDev is the identity created by the guarded local dev-login (no OAuth).
	// Dev/local only; the route that uses it is off in production.
	ProviderDev IdentityProvider = "dev"
)

// UserIdentity links a user to one social provider account (RFC-003 2.4).
type UserIdentity struct {
	ID             int64
	UserID         int64
	Provider       IdentityProvider
	ProviderUserID string // the provider's stable subject id
	CreatedAt      time.Time
}

// Organization is the tenant root. PlanID is nil until the billing catalog lands.
// DeletedAt non-nil means the org is in its 14-day soft-delete grace (RFC-015).
type Organization struct {
	ID              int64
	Name            string
	Slug            string // unique globally, shapes {slug}.pulse.app
	PlanID          *int64 // nil until plans/subscriptions exist (RFC-001 4.2)
	Plan            string // billing tier (free/starter/team/business); operator-set until Stripe lands
	DefaultLocale   string
	DefaultTimezone string
	CreatedAt       time.Time
	DeletedAt       *time.Time
}

// Role is a user's role inside one org (PRD-001 7).
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

// Membership is the link of a user to an org with a role. Org-scoped.
type Membership struct {
	ID        int64
	OrgID     int64
	UserID    int64
	Role      Role
	CreatedAt time.Time
}

// InvitationState is the invitation state machine (PRD-001 6).
type InvitationState string

const (
	InvitePending  InvitationState = "pending"
	InviteAccepted InvitationState = "accepted"
	InviteRevoked  InvitationState = "revoked"
	InviteExpired  InvitationState = "expired"
)

// Invitation is a pending offer to join an org. TokenHash is the SHA-256 of the
// raw token; the raw token lives only in the email link. Org-scoped.
type Invitation struct {
	ID         int64
	OrgID      int64
	Email      string
	Role       Role
	State      InvitationState
	TokenHash  string
	Locale     string // invite-email locale (RFC-014 7/9)
	CreatedBy  *int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	AcceptedAt *time.Time
}

// APIKey is a per-org key for the public REST surface (RFC-003 5, PRD-005 2).
// Org-scoped. TokenHash is the SHA-256 of the full presented pulse_sk_ key; the
// secret is shown once and never stored in clear. Role is member or admin only
// (no owner-equivalent keys, PRD-001 App A). The key fixes its org and role, so
// verify resolves the request principal with no JWT (RFC-003 5.4).
type APIKey struct {
	ID         int64
	OrgID      int64
	Name       string
	Prefix     string // non-secret leading chars, safe to list/log
	TokenHash  string
	Role       Role
	CreatedBy  *int64
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// OrgWebhookEvent is one event type an org webhook can subscribe to (PRD-005 7.1).
// These are the org-level event stream, distinct from the per-monitor channel
// payloads. v1 ships the four down/recovery + incident open/close events.
type OrgWebhookEvent string

const (
	OrgEventMonitorDown    OrgWebhookEvent = "monitor.down"
	OrgEventMonitorRecover OrgWebhookEvent = "monitor.recovery"
	OrgEventIncidentOpened OrgWebhookEvent = "incident.opened"
	OrgEventIncidentClosed OrgWebhookEvent = "incident.closed"
)

// OrgWebhook is a registered org-level outbound webhook (PRD-005 7). Pulse POSTs a
// signed event envelope to URL when one of Events fires for the org. The signing
// secret is stored encrypted at rest (crypto cipher) and shown to the user exactly
// once at create/rotate, never returned again. An empty Events means "all event
// types". LastStatus is "delivered" or "failed", set after each delivery attempt so
// an owner/admin can spot a broken receiver.
type OrgWebhook struct {
	ID             int64
	OrgID          int64
	URL            string
	SigningSecret  string // decrypted in memory; never serialized to the API
	Enabled        bool
	Events         []OrgWebhookEvent // empty = all
	CreatedBy      *int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastDeliveryAt *time.Time
	LastStatus     string // "" until first delivery, then delivered/failed
	LastError      *string
}

// Subscribes reports whether the webhook wants the given event type. An empty
// Events list means it wants every type.
func (w *OrgWebhook) Subscribes(ev OrgWebhookEvent) bool {
	if len(w.Events) == 0 {
		return true
	}
	for _, e := range w.Events {
		if e == ev {
			return true
		}
	}
	return false
}

// RefreshToken is one opaque, DB-backed refresh token (RFC-003 4). Global (keyed
// by user). TokenHash is the SHA-256 of the raw token. ReplacedBy non-nil means
// the token was already rotated, so re-presenting it is the reuse signal.
type RefreshToken struct {
	ID         int64
	UserID     int64
	FamilyID   int64
	ReplacedBy *int64
	TokenHash  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
}

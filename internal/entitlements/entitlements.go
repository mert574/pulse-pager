// Package entitlements decides what an org's plan allows. It is the seam RFC-009
// will flesh out: a Redis-cached per-org lookup backed by the plan/subscription
// tables (PRD-006). Billing is not built yet, so the default resolver (AllOn)
// turns every feature on for every org. The Resolver interface exists so callers
// gate on entitlements today and the real resolver drops in without changing them.
package entitlements

import "pulse/internal/region"

// Set is the resolved feature access for one org. It grows as more of the plan
// model becomes enforced (RFC-009 2.2: monitor cap, interval floor, regions, ...).
type Set struct {
	// FailureSnapshot allows storing the last failed check's response (PRD-002 3.8).
	FailureSnapshot bool
}

// Resolver returns the entitlement set for an org.
type Resolver interface {
	For(orgID int64) Set
}

// AllOn grants every feature to every org. This is the current resolver, used
// until per-plan gating ships (RFC-009). Swapping in the real resolver is a
// one-line wiring change at each call site.
type AllOn struct{}

// For returns an all-features-on set.
func (AllOn) For(int64) Set {
	return Set{FailureSnapshot: true}
}

// --- seats (PRD-001 5.2, master 11 seat meter) ---

// Plan is the billing tier a seat cap comes from. The numbers live here, not in a
// handler, so the api just asks "is a seat free?" (PRD-001 5.2). When PRD-006 lands
// the real per-org cap drops in behind SeatResolver with no handler change.
type Plan string

// Plan codes are deliberately marketing-name-agnostic (tier1..3 plus tierCustom) so
// the public display names (Free / Hobby / Professional / Custom on pricing.html)
// can change without the stored code drifting. tier1 is the free tier; tierCustom
// is the contract-negotiated tier.
const (
	PlanTier1      Plan = "tier1"
	PlanTier2      Plan = "tier2"
	PlanTier3      Plan = "tier3"
	PlanTierCustom Plan = "tierCustom"
)

// ParsePlan turns a stored plan string into a known Plan, falling back to Free for an
// empty or unrecognized value so a bad row can never grant more than the free tier.
func ParsePlan(s string) Plan {
	switch Plan(s) {
	case PlanTier2:
		return PlanTier2
	case PlanTier3:
		return PlanTier3
	case PlanTierCustom:
		return PlanTierCustom
	default:
		return PlanTier1
	}
}

// seatCaps is the per-plan seat capacity from PRD-001 5.2 (master 11 table):
// Free 1, Hobby 3, Professional 10, Custom unlimited (pricing.html; Custom is
// contract-negotiated, represented here as a very large cap).
var seatCaps = map[Plan]int{
	PlanTier1:      1,
	PlanTier2:      3,
	PlanTier3:      10,
	PlanTierCustom: 1_000_000, // Custom: "unlimited" seats (contract-negotiated)
}

// SeatResolver returns the seat cap for an org. PRD-006 will back this with the
// real subscription; until then DefaultSeats reads it off the org's plan. The
// interface exists so callers gate on seats today and the real one drops in later.
type SeatResolver interface {
	SeatCap(orgID int64, plan Plan) int
}

// DefaultSeats is the current seat resolver: it maps the org's plan to the PRD-001
// 5.2 cap, ignoring the org id (no per-org overrides until PRD-006). An unknown
// plan falls back to the Free cap, which fails safe (the tightest limit).
type DefaultSeats struct{}

// SeatCap returns the plan's seat capacity.
func (DefaultSeats) SeatCap(_ int64, plan Plan) int {
	if cap, ok := seatCaps[plan]; ok {
		return cap
	}
	return seatCaps[PlanTier1]
}

// FixedSeats is a seat resolver that returns the same cap for every org and plan.
// It is the test/dev override so the seat path can be exercised above the Free cap
// of 1 (where the owner already takes the only seat). Production wires DefaultSeats.
type FixedSeats struct{ Cap int }

// SeatCap returns the fixed cap.
func (f FixedSeats) SeatCap(int64, Plan) int { return f.Cap }

// SeatUsage is the seat meter for an org: accepted members plus reserved pending
// invitations, against the plan cap (PRD-001 5.1). Used is what occupies a seat;
// Cap is what the plan allows.
type SeatUsage struct {
	Used int
	Cap  int
}

// HasFreeSeat reports whether one more seat can be taken (an invite can be sent).
func (u SeatUsage) HasFreeSeat() bool { return u.Used < u.Cap }

// --- monitors (PRD-006 section 3 matrix, master 6.2 hard floor) ---

// HardIntervalFloorSeconds is the absolute minimum interval, never crossed on any
// plan (master 6.2). The per-tier floor is the max of this and the tier value.
const HardIntervalFloorSeconds = 30

// MonitorLimits is the per-plan monitor allowance: the enabled-monitor cap, the
// min check interval floor in seconds, the regions the plan allows, and how many
// regions one monitor may use (PRD-006 3). Numbers are the locked Free/Business
// anchors and the GTM-tunable Starter/Team values from PRD-006 appendix A.
type MonitorLimits struct {
	MonitorsCap          int      // enabled-monitor cap (0 means unset -> Free)
	MinIntervalSeconds   int      // tier frequency floor (effective floor = max(hard, this))
	RegionsAllowed       []string // region codes the plan includes
	RegionsPerMonitorCap int      // max regions one monitor may run from
}

// monitorLimits is the per-plan monitor matrix, matching pricing.html: Free
// 10 monitors / 15-min floor / single region, Hobby 25 / 5-min / single region,
// Professional 50 / 1-min / up to 4 regions, Custom high cap / 30s floor / all
// regions (contract-negotiated). Region codes come from internal/region so this
// catalog can't drift from the worker region or the dispatch default.
var monitorLimits = map[Plan]MonitorLimits{
	PlanTier1:      {MonitorsCap: 10, MinIntervalSeconds: 900, RegionsAllowed: []string{region.EUCentral}, RegionsPerMonitorCap: 1},
	PlanTier2:      {MonitorsCap: 25, MinIntervalSeconds: 300, RegionsAllowed: []string{region.EUCentral}, RegionsPerMonitorCap: 1},
	PlanTier3:      {MonitorsCap: 50, MinIntervalSeconds: 60, RegionsAllowed: []string{region.EUCentral, region.USEast, region.USWest, region.SAEast}, RegionsPerMonitorCap: 4},
	PlanTierCustom: {MonitorsCap: 1000, MinIntervalSeconds: 30, RegionsAllowed: []string{region.EUCentral, region.USEast, region.USWest, region.SAEast}, RegionsPerMonitorCap: 4},
}

// MonitorResolver returns the monitor limits for an org. PRD-006 will back this with
// the real per-org entitlement (with support grandfather overrides); until then
// DefaultMonitors reads it off the org's plan. The interface mirrors SeatResolver so
// the handler gates on limits today and the real resolver drops in with no change.
type MonitorResolver interface {
	MonitorLimits(orgID int64, plan Plan) MonitorLimits
}

// DefaultMonitors is the current monitor resolver: it maps the org's plan to the
// PRD-006 matrix, ignoring the org id (no per-org overrides until PRD-006). An
// unknown plan falls back to Free, which fails safe (the tightest limits).
type DefaultMonitors struct{}

// MonitorLimits returns the plan's monitor limits.
func (DefaultMonitors) MonitorLimits(_ int64, plan Plan) MonitorLimits {
	if l, ok := monitorLimits[plan]; ok {
		return l
	}
	return monitorLimits[PlanTier1]
}

// FixedMonitors is a monitor resolver that returns the same limits for every org and
// plan. It is the test/dev override so the create path can be exercised above the
// Free cap of 2 and below custom floors. Production wires DefaultMonitors.
type FixedMonitors struct{ Limits MonitorLimits }

// MonitorLimits returns the fixed limits.
func (f FixedMonitors) MonitorLimits(int64, Plan) MonitorLimits { return f.Limits }

// EffectiveIntervalFloor is the floor a monitor's interval may not go below: the
// max of the absolute hard floor and the plan's tier floor (PRD-002 2.3 note,
// master 6.2). The scheduler also clamps to this on dispatch (PRD-006 5.2).
func (l MonitorLimits) EffectiveIntervalFloor() int {
	if l.MinIntervalSeconds > HardIntervalFloorSeconds {
		return l.MinIntervalSeconds
	}
	return HardIntervalFloorSeconds
}

// AllowsRegion reports whether region is in the plan's allowed set.
func (l MonitorLimits) AllowsRegion(region string) bool {
	for _, r := range l.RegionsAllowed {
		if r == region {
			return true
		}
	}
	return false
}

// --- status pages (PRD-004 2.3, master 11 plan table) ---

// statusPageCaps is the per-plan status-page count cap (PRD-004 2.3, master 11):
// Free 1, Hobby 3, Professional 10, Custom unlimited. Creating a page past the cap is rejected
// on write with the upsell (PRD-004 2.3, PRD-006).
var statusPageCaps = map[Plan]int{
	PlanTier1:      1,
	PlanTier2:      3,
	PlanTier3:      10,
	PlanTierCustom: 1000, // Custom: effectively unlimited (contract-negotiated)
}

// StatusPageResolver returns the status-page count cap for an org. PRD-006 will back
// this with the real per-org subscription; until then DefaultStatusPages reads it off
// the org's plan. The interface mirrors SeatResolver / MonitorResolver so the handler
// gates on the cap today and the real resolver drops in with no change.
type StatusPageResolver interface {
	StatusPageCap(orgID int64, plan Plan) int
}

// DefaultStatusPages is the current status-page resolver: it maps the org's plan to
// the PRD-004 cap, ignoring the org id (no per-org overrides until PRD-006). An
// unknown plan falls back to Free, which fails safe (the tightest cap).
type DefaultStatusPages struct{}

// StatusPageCap returns the plan's status-page count cap.
func (DefaultStatusPages) StatusPageCap(_ int64, plan Plan) int {
	if cap, ok := statusPageCaps[plan]; ok {
		return cap
	}
	return statusPageCaps[PlanTier1]
}

// FixedStatusPages is a status-page resolver that returns the same cap for every org
// and plan. It is the test/dev override so the create path can be exercised above the
// Free cap of 1. Production wires DefaultStatusPages.
type FixedStatusPages struct{ Cap int }

// StatusPageCap returns the fixed cap.
func (f FixedStatusPages) StatusPageCap(int64, Plan) int { return f.Cap }

// --- retention + feature flags + the plan catalog (PRD-006 3) ---

// retentionDays is the per-plan raw-results retention window in days (PRD-006 3,
// master 12: Free 7, Starter 30, Team 90, Business 180). The cleanup job ages out
// results beyond this; the billing/usage screen shows it (PRD-006 7.1).
var retentionDays = map[Plan]int{
	PlanTier1:      7,
	PlanTier2:      30,
	PlanTier3:      90,
	PlanTierCustom: 180,
}

// customDomainAllowed is whether a plan may put a status page on a custom domain
// (PRD-006 3: Free/Starter no, Team/Business yes).
var customDomainAllowed = map[Plan]bool{
	PlanTier1:      false,
	PlanTier2:      false,
	PlanTier3:      true,
	PlanTierCustom: true,
}

// apiAccessAllowed is whether the plan gets API keys at all (pricing.html: Free has
// no API access; Hobby/Pro/Custom do). Free is offered the upgrade instead of the
// key-management UI, and key creation is rejected server-side.
var apiAccessAllowed = map[Plan]bool{
	PlanTier1:      false,
	PlanTier2:      true,
	PlanTier3:      true,
	PlanTierCustom: true,
}

// apiWriteAllowed is whether the plan's API keys can write (pricing.html: Hobby is
// read-only; Professional and Custom get full read+write). A read-only key's write
// call is rejected with an upsell. Free has no keys at all (see apiAccessAllowed).
var apiWriteAllowed = map[Plan]bool{
	PlanTier1:      false,
	PlanTier2:      false,
	PlanTier3:      true,
	PlanTierCustom: true,
}

// apiRatePerMin is the per-key request rate ceiling per plan (PRD-006 3: 30/120/
// 300/600 req/min). Illustrative GTM-tunable defaults built against the master's
// "read-only-low / standard / higher / highest" tiers (master 9).
var apiRatePerMin = map[Plan]int{
	PlanTier1:      30,
	PlanTier2:      120,
	PlanTier3:      300,
	PlanTierCustom: 600,
}

// channelTypesAllowed is the notification channel types a plan may use (PRD-006 3).
// All v1 channels (Slack/Discord/webhook/email-over-SMTP) are on every tier; the
// phased PagerDuty/Opsgenie/Telegram/Teams/Twilio types are not modeled here yet
// (they arrive with the channel roadmap, master 7 / 15). The type strings are the
// notify descriptor types and the OpenAPI ChannelType enum, so "smtp" (not "email").
var channelTypesAllowed = map[Plan][]string{
	// The four basics (chat, generic webhook, email) on every plan; the integrations
	// unlock up the ladder. Starter adds Telegram, Team adds the on-call/incident tools
	// plus Microsoft Teams, Business adds Twilio SMS/voice and so includes them all.
	PlanTier1:      {"slack", "discord", "webhook", "smtp"},
	PlanTier2:      {"slack", "discord", "webhook", "smtp", "telegram"},
	PlanTier3:      {"slack", "discord", "webhook", "smtp", "telegram", "pagerduty", "opsgenie", "teams"},
	PlanTierCustom: {"slack", "discord", "webhook", "smtp", "telegram", "pagerduty", "opsgenie", "teams", "twilio"},
}

// ChannelTypesAllowed returns the notification channel types a plan may use (the
// notify descriptor types), falling back to Free (the tightest) for an unknown
// plan. The channel CRUD reads this to plan-gate create/update and to mark each
// catalog entry available or not.
func ChannelTypesAllowed(plan Plan) []string {
	if t, ok := channelTypesAllowed[plan]; ok {
		return t
	}
	return channelTypesAllowed[PlanTier1]
}

// CheckNowLimits is the two-layer rate limit on manual "check now" for a single
// monitor, decoupled from the scheduled interval floor. A manual check is a human
// "did my fix work?" action, so we want it to feel instant for a few retries but not
// double as free high-frequency monitoring:
//   - CooldownSeconds is the burst layer: the minimum gap between two manual checks.
//   - MaxPerWindow within WindowSeconds is the sustained layer: it stops someone
//     holding the burst gap forever to get, say, 1-minute monitoring for free.
//
// Both must pass for a manual check to run. Free is the tightest; paid tiers loosen
// both. Numbers are GTM-tunable (PRD-006 appendix A).
type CheckNowLimits struct {
	CooldownSeconds int // burst: one manual check per this many seconds
	MaxPerWindow    int // sustained: at most this many manual checks per window
	WindowSeconds   int // the sustained window length
}

var checkNowLimits = map[Plan]CheckNowLimits{
	PlanTier1:      {CooldownSeconds: 60, MaxPerWindow: 10, WindowSeconds: 1800},
	PlanTier2:      {CooldownSeconds: 30, MaxPerWindow: 30, WindowSeconds: 1800},
	PlanTier3:      {CooldownSeconds: 20, MaxPerWindow: 60, WindowSeconds: 1800},
	PlanTierCustom: {CooldownSeconds: 10, MaxPerWindow: 200, WindowSeconds: 1800},
}

// CheckNowLimitsFor returns the per-monitor manual-check limits for a plan, falling
// back to Free (the tightest) for an unknown plan.
func CheckNowLimitsFor(plan Plan) CheckNowLimits {
	if l, ok := checkNowLimits[plan]; ok {
		return l
	}
	return checkNowLimits[PlanTier1]
}

// Retention returns the raw-results retention window in days for a plan, falling
// back to Free (the tightest) for an unknown plan.
func Retention(plan Plan) int {
	if d, ok := retentionDays[plan]; ok {
		return d
	}
	return retentionDays[PlanTier1]
}

// CustomDomainAllowed reports whether a plan may use a custom-domain status page.
func CustomDomainAllowed(plan Plan) bool { return customDomainAllowed[plan] }

// APIAccessAllowed reports whether a plan gets API keys at all (Free does not).
func APIAccessAllowed(plan Plan) bool { return apiAccessAllowed[plan] }

// APIWriteAllowed reports whether a plan's API keys can write (read-only otherwise).
func APIWriteAllowed(plan Plan) bool { return apiWriteAllowed[plan] }

// APIRatePerMin returns the per-key request-rate ceiling for a plan, falling back
// to Free for an unknown plan.
func APIRatePerMin(plan Plan) int {
	if r, ok := apiRatePerMin[plan]; ok {
		return r
	}
	return apiRatePerMin[PlanTier1]
}

// FailureSnapshotAllowed reports whether a plan may store the last failed check's
// response (PRD-002 3.8). On for every tier now; gateable per plan later (PRD-006 3).
func FailureSnapshotAllowed(Plan) bool { return true }

// PlanEntry is one tier in the public plan catalog: the plan id plus every gated/
// metered limit for that tier (PRD-006 3 matrix). It is read-only reference config
// the FE renders as a plan-comparison/upgrade table, so it carries no per-org usage.
type PlanEntry struct {
	Plan                 Plan
	MonitorsCap          int
	MinIntervalSeconds   int
	SeatsCap             int
	StatusPagesCap       int
	RetentionDays        int
	RegionsAllowed       []string
	RegionsPerMonitorCap int
	CustomDomainAllowed  bool
	APIAccessAllowed     bool
	APIWriteAllowed      bool
	APIRatePerMin        int
	ChannelTypes         []string
}

// catalogOrder is the tier order the catalog returns, cheapest to richest, so the
// FE renders the comparison table left to right without sorting.
var catalogOrder = []Plan{PlanTier1, PlanTier2, PlanTier3, PlanTierCustom}

// Catalog returns the public plan tiers and their limits (PRD-006 3), sourced from
// the per-plan maps in this package so the FE renders the upgrade table without
// hardcoding. It is store-free reference config; usage vs caps for one org is the
// per-org entitlements endpoint, not this.
func Catalog() []PlanEntry {
	out := make([]PlanEntry, 0, len(catalogOrder))
	for _, p := range catalogOrder {
		ml := monitorLimits[p]
		out = append(out, PlanEntry{
			Plan:                 p,
			MonitorsCap:          ml.MonitorsCap,
			MinIntervalSeconds:   ml.EffectiveIntervalFloor(),
			SeatsCap:             seatCaps[p],
			StatusPagesCap:       statusPageCaps[p],
			RetentionDays:        retentionDays[p],
			RegionsAllowed:       ml.RegionsAllowed,
			RegionsPerMonitorCap: ml.RegionsPerMonitorCap,
			CustomDomainAllowed:  customDomainAllowed[p],
			APIAccessAllowed:     apiAccessAllowed[p],
			APIWriteAllowed:      apiWriteAllowed[p],
			APIRatePerMin:        apiRatePerMin[p],
			ChannelTypes:         channelTypesAllowed[p],
		})
	}
	return out
}

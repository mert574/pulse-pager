# RFC-009 - Entitlements Enforcement

Status: DRAFT for review
Author: Principal Engineer / Platform
Audience: api authors (RFC-012), scheduler authors (RFC-004), anyone enforcing a plan limit on a write or a dispatch
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 2.6 entitlements-as-library, section 12 cross-cutting entitlement enforcement, ADR-0008)
Product source of truth: `docs/prd/PRD-006-billing-and-entitlements.md` (plan matrix, the two enforcement points, downgrade behavior, fail-closed on write), master PRD sections 11 and 12
Depends on: RFC-000 (library-not-service, cache contract), RFC-001 (`plans` / `subscriptions` / `entitlements` schema), RFC-002 (`billing.events` for invalidation)
Depended on by: RFC-004 (scheduler dispatch clamp), RFC-012 (api write enforcement + error envelope)
Reuses: nothing from v1; `internal/entitlements` is new (RFC-000 section 9 package table)

House style: no em-dashes. Tables and diagrams over prose. Every load-bearing choice states the decision, the reasoning, and the rejected alternatives.

---

## 1. Overview, scope, and owned contracts

### 1.1 What this RFC is

Entitlements is the thing that turns "this org is on Free" into "this org may have 10 monitors, no faster than every 15 minutes, in 1 region." It is a shared library, `internal/entitlements`, not a service (RFC-000 section 2.6, ADR-0008). Two callers link it: api enforces on write, scheduler enforces on dispatch. Neither calls the other and neither calls a network endpoint for the lookup; the only I/O is a Redis read with Postgres as the source of truth (RFC-000 section 12).

This RFC fixes three things: the resolved-entitlements model an org carries, the library API the two callers use, and the cache plus invalidation contract including the fail-open-versus-closed decision at each point.

### 1.2 Scope

In scope:

- The resolved-entitlements struct: how plan plus subscription plus per-org overrides become one concrete set of limits and flags (section 2).
- The library API: `Get`, the pure check helpers (`CheckMonitorCreate`, `ClampInterval`, `RegionAllowed`, and the rest), and how callers link rather than call a service (section 3).
- The Redis cache: key, TTL, what is stored, invalidation on `billing.events`, and the cold-cache and Redis-down behavior, resolved per enforcement point (section 4).
- Enforcement point 1, api on write: which writes are checked, the error codes, the fail-closed stance (section 5).
- Enforcement point 2, scheduler on dispatch: the interval clamp and the region filter, and why both points exist (section 6).
- Downgrade behavior: bring-usage-under-the-limit, the over-limit state, what clamps versus what blocks (section 7).
- Plan and subscription changes: how a change reaches both enforcement points quickly (section 8).
- The per-plan API rate limit (section 9).
- Testing and acceptance criteria (section 10).

Out of scope (named, owned elsewhere):

| Out of scope | Owner |
|--------------|-------|
| The `plans` / `subscriptions` / `entitlements` table DDL and seed matrix | RFC-001 section 4.2 |
| The `billing.events` event schema and the `entitlement-invalidator` consumer wiring | RFC-002 section 4.7 |
| The standard error envelope shape (`code` / `message` / `fields`) and HTTP status mapping | RFC-012 |
| The role gate (`authz.Can`) that runs alongside the entitlement gate | RFC-003 section 7.4 |
| The scheduler's schedule rebuild and per-(monitor, region) fan-out that this RFC clamps | RFC-004 |
| Paddle webhook handling that produces `billing.events` | api billing handler (PRD-006 section 8) |
| The billing/usage UI, upgrade prompts, the downgrade checklist | RFC-013, PRD-006 section 7 |

### 1.3 Contracts this RFC owns

| Contract | Decision in this RFC |
|----------|----------------------|
| Resolved entitlements | one immutable struct per org, derived from plan + subscription + override, cached whole (section 2) |
| Library API | `Get(ctx, orgID)` is the only I/O; every check is a pure function over the struct (section 3) |
| Cache key | `ent:v1:{org_id}` in Redis, value is the JSON-encoded struct, TTL 10 minutes as a backstop (section 4) |
| Invalidation | `entitlement-invalidator` (RFC-002) deletes the key on every `billing.events`; next `Get` repopulates from Postgres (section 4, 8) |
| Fail-closed on write | if entitlements cannot be confirmed, the api write is rejected (section 4.4, 5) |
| Fail-stale-permissive on dispatch | the scheduler clamps using its last-known snapshot, never wide-open (section 4.4, 6) |

---

## 2. The entitlement model

### 2.1 From plan to a concrete set

A plan is a row in the global `plans` catalog (RFC-001 section 4.2): the four tiers `tier1` / `tier2` / `tier3` / `tierCustom` with the default values for every gated dimension. A plan is not what we enforce against directly. What we enforce against is the per-org `entitlements` row, which is normally a copy of the plan defaults but can hold an override (an enterprise custom limit, a comped grant, a support grandfather) without inventing a fake plan tier. This is RFC-001's design: "stored so an override does not need a fake plan" (RFC-001 section 4.2).

The resolution is a small merge, computed when the `entitlements` row is written (on signup, on plan change), not on every read:

```
plan defaults  (plans row for the subscription's plan_id)
    +  subscription capacity  (seats_purchased; status drives drop-to-Free)
    +  per-org override        (any non-default value set by an operator/enterprise deal)
    =  the entitlements row    (RFC-001 section 4.2, one row per org)
```

Decision: resolve once into the `entitlements` row, cache that row whole, and never recompute the merge on the hot path. Reasoning: the merge needs a plan read, a subscription read, and override logic; doing it per check at 10k/sec or per api request is the database read per request that RFC-000 section 12 forbids. The merge runs only when something changes (section 8). Rejected alternative, resolve on read from plan + subscription + override every time: simpler to reason about but reintroduces the multi-row read the cache exists to avoid, and spreads the override precedence logic across both callers.

Override precedence, when an override is present: the override value wins for that one field; every unset field falls back to the plan default. Overrides are per field, not per row, so an enterprise org can get 2000 monitors while still inheriting the standard 30s hard floor. Overrides are phased: in Phase 1 they are set by an operator writing the `entitlements` row directly; the model already supports them so no schema change is needed when enterprise deals arrive.

### 2.2 The resolved struct

The cached struct mirrors the `entitlements` columns one-to-one (RFC-001 section 4.2). It is the single value `Get` returns and every helper reads.

```go
// internal/entitlements
type Entitlements struct {
    OrgID                 int64

    MonitorsCap           int      // enabled-monitor cap
    MinIntervalSeconds    int      // the plan floor (or override)
    HardFloorSeconds      int      // 30 across all tiers, never undercut
    SeatsIncluded         int      // plan-included seats
    SeatsPurchased        int      // paid add-on seats (subscription)
    RegionsAllowed        []string // the set of region codes the org may use
    RegionsPerMonitorCap  int      // max regions on one monitor
    RetentionDays         int      // raw-result retention
    StatusPagesCap        int
    CustomDomain          bool
    APIAccess             APIAccess // none / read / full
    APIRatePerMin         int       // per-key token-bucket rate
    OutboundWebhooks      bool
    ChannelTypesAllowed   []string  // notification channel types this tier may use
    AuditLogRetentionDays *int      // nil when the tier has no audit retention
    FailureSnapshot       bool      // store last failed check's response; enforced by the worker before persisting (PRD-002 3.8, RFC-005 5b)

    ResolvedAt time.Time // when the merge ran; for staleness logging only
}

type APIAccess string // "none" | "read" | "full"

// SeatsTotal is included + purchased; the seat gate counts accepted + pending against it.
func (e Entitlements) SeatsTotal() int { return e.SeatsIncluded + e.SeatsPurchased }
```

`ChannelTypesAllowed` is not a column in RFC-001 section 4.2, and v1 does not add one. RFC-001 stores the metered numeric and boolean limits; the channel-type set is a small tier-derived list. Decision (final): the resolver derives `ChannelTypesAllowed` from the plan tier (the matrix in PRD-006 section 3: all v1 channels on every tier, PagerDuty/Opsgenie phased onto Professional). There is no `entitlements` column and no per-org channel-type override in v1, so a reader should not look for a missing column. It is carried in the cached struct so the notification write path reads it the same way as every other limit. A `channel_types_allowed TEXT[]` column is only worth adding later if phased channels ever need a per-org grant.

### 2.3 The four tiers, resolved

These are the seeded plan defaults (RFC-001 section 4.2 seed table, which is PRD-006 section 3). An org with no override carries exactly these.

Codes tier1 / tier2 / tier3 / tierCustom map to the public names Free / Hobby /
Professional / Custom (pricing.html is the source of truth). Custom carries large
defaults a contract overrides.

| Field | tier1 | tier2 | tier3 | tierCustom |
|-------|-----:|--------:|-----:|---------:|
| MonitorsCap | 10 | 25 | 50 | 1000 |
| MinIntervalSeconds | 900 | 300 | 60 | 30 |
| HardFloorSeconds | 30 | 30 | 30 | 30 |
| RegionsPerMonitorCap | 1 | 1 | 4 | 4 |
| premium regions (in RegionsAllowed) | no | no | no | yes |
| SeatsIncluded | 1 | 3 | 10 | 1000000 |
| RetentionDays | 7 | 30 | 90 | 180 |
| StatusPagesCap | 1 | 3 | 10 | 1000 |
| CustomDomain | false | false | true | true |
| APIAccess | none | read | full | full |
| APIRatePerMin | n/a | 120 | 300 | 600 |
| OutboundWebhooks | false | false | true | true |
| AuditLogRetentionDays | nil | nil | 30 | 365 |
| FailureSnapshot | true | true | true | true |

`RegionsAllowed` holds the concrete region codes the org may use, drawn from the `regions` catalog (RFC-001) and bounded by `RegionsPerMonitorCap`; premium regions only appear in the set on business (the `premium_regions_allowed` plan flag). The set is the live entitlement both the api and the scheduler filter against (RFC-001 section 4.2 note: the repository validates region codes on write, the scheduler re-checks on dispatch).

---

## 3. The library API (`internal/entitlements`)

### 3.1 Shape: one lookup, pure checks

Decision: exactly one function does I/O (`Get`); every enforcement helper is a pure function of `(Entitlements, args)` with no context and no error. Reasoning: callers on the two hottest paths (api write, scheduler dispatch) should pay the lookup once and then make every decision in memory with no further failure mode. Pure helpers are trivially testable in isolation (section 10) and identical on both callers, which is the no-bypass guarantee in code form. Rejected alternative, a method-on-a-client API where each check hits the cache itself: multiplies the lookup, hides which calls do I/O, and lets the two callers drift.

```go
// The only I/O. Serves from Redis, falls back to Postgres, populates the cache.
// Returns ErrEntitlementsUnavailable when neither source can answer (section 4.4).
func Get(ctx context.Context, orgID int64) (Entitlements, error)

var ErrEntitlementsUnavailable = errors.New("entitlements: cannot confirm limits")
```

### 3.2 The pure check helpers

Each returns enough for the caller to build the right error (api) or the right clamp (scheduler). They never reach the cache.

```go
// Monitor cap. currentEnabled is the count of enabled monitors in the org.
// Allowed reports whether one more enabled monitor fits.
func CheckMonitorCreate(e Entitlements, currentEnabled int) Decision

// Interval. ClampInterval returns the interval that must actually run:
//   max(requested, MinIntervalSeconds), then never below HardFloorSeconds.
// The api uses CheckInterval to reject; the scheduler uses ClampInterval to run.
func ClampInterval(e Entitlements, requested int) int
func CheckInterval(e Entitlements, requested int) Decision // floor vs hard-floor violation

// Regions. RegionAllowed tests one code; FilterRegions keeps only entitled codes,
// capped at RegionsPerMonitorCap, preserving order (home region first).
func RegionAllowed(e Entitlements, region string) bool
func FilterRegions(e Entitlements, requested []string) []string
func CheckRegions(e Entitlements, requested []string) Decision // not-in-plan vs count-exceeded

// Seats. seatsUsed is accepted memberships + live pending invites (PRD-006 section 4.2).
func CheckInvite(e Entitlements, seatsUsed int) Decision

// Status pages, custom domain, API access, channels: same Decision shape.
func CheckStatusPageCreate(e Entitlements, currentPages int) Decision
func CheckCustomDomain(e Entitlements) Decision
func CheckAPIWrite(e Entitlements) Decision        // false when APIAccess != full
func ChannelTypeAllowed(e Entitlements, ct string) bool

// Decision carries the verdict and, on a deny, the error code RFC-012 renders.
type Decision struct {
    Allowed bool
    Code    string // "" when Allowed; e.g. "monitor_limit_reached" otherwise
    Limit   int    // the limit that was hit, for the message and the upsell
}
```

The two interval entrypoints exist on purpose. `CheckInterval` is the write gate (reject below the floor). `ClampInterval` is the dispatch behavior (run no faster than the floor without mutating the stored value). Same floor, two uses, so a downgrade is enforced at both points (section 6).

### 3.3 Why a library, not a service

Restating RFC-000 section 2.6 / ADR-0008 as it lands here: api and scheduler import `internal/entitlements` and link it into their binary (one Go module, RFC-000 section 3). `Get` reads the shared Redis cache the api populates and the scheduler reads. There is no entitlements process to deploy, page, or add a network hop in front of the two most latency-sensitive paths. The data is small and changes only on plan change, the ideal shape for cache-with-invalidation in-process. If a future usage-metering engine ever needs its own process, the `Get` signature stays and a service can sit behind it without touching callers (RFC-000 section 2.6 rejected-alternative note).

---

## 4. Caching and invalidation (RFC-000 section 12)

### 4.1 What is cached, where, and how

| Property | Value | Reasoning |
|----------|-------|-----------|
| Store | Redis (shared by api and scheduler) | RFC-000 section 12: hot paths never pay a Postgres read per request or check |
| Key | `ent:v1:{org_id}` | per-org (RFC-000 section 12: "the cache key is per org"); `v1` lets the struct shape change without a stale-read mismatch |
| Value | JSON-encoded `Entitlements` (the whole resolved struct, section 2.2) | one round trip returns everything every helper needs; no second read |
| TTL | 10 minutes | a backstop only; correctness comes from event invalidation, not expiry (4.2). TTL bounds drift if an invalidation is ever missed |
| Populated by | `Get` on a miss: read Postgres, resolve, `SET` with TTL | read-through; the first reader after a miss pays the Postgres read, everyone else hits Redis |

The TTL is deliberately not the primary correctness mechanism. Invalidation is. The TTL exists so that if the invalidation pipeline ever drops a message (a `billing.events` consumer outage, say), an org's limits self-heal within 10 minutes instead of staying stale until the next plan change. Decision: short-ish TTL plus event invalidation, belt and suspenders. Rejected alternative, no TTL: a single missed invalidation pins stale limits forever; rejected, very short TTL (seconds): turns the cache back into a near-per-request Postgres read on a cold-ish key, defeating the cache.

### 4.2 Invalidation on `billing.events`

Invalidation is event-driven and already wired by RFC-002. The `entitlement-invalidator` consumer reads `billing.events` (keyed by `org_id`, so two changes for one org stay ordered) and deletes `ent:v1:{org_id}` on every record (RFC-002 section 4.7). The next `Get` for that org misses and repopulates from the freshly-written `entitlements` row.

```
plan change (Paddle webhook OR internal admin)
   -> api writes the new subscription + resolves the new entitlements row (Postgres)
   -> api produces billing.events (org_id key)                        [RFC-002 4.7]
   -> entitlement-invalidator consumes -> DEL ent:v1:{org_id}          [RFC-002 4.7]
   -> next Get(org) misses -> reads Postgres -> SET ent:v1:{org_id}
   -> api write gate and scheduler dispatch now see the new limits
```

Invalidation is idempotent: deleting an already-deleted key is a no-op, so redelivery is safe with no extra dedup token (RFC-002 section 4.7). Ordering matters and is guaranteed: the api writes the new `entitlements` row to Postgres before producing `billing.events`, so by the time the invalidator deletes the key, the repopulating read sees the new row, never the old one.

### 4.3 The two failure cases

| Case | What happened | Handled in |
|------|---------------|------------|
| Cold cache | key absent (TTL expired, or just invalidated), Postgres reachable | read-through repopulates; one slow read, then warm. Normal. |
| Redis down | the cache layer is unreachable on the hot path | fail decision differs by caller (4.4) |

A cold cache is not a failure; it is the normal read-through path. The interesting case is Redis being unreachable, because that is where fail-open versus fail-closed actually bites.

### 4.4 Fail-open vs fail-closed, resolved per enforcement point

This is the load-bearing decision of the RFC. The two enforcement points get different answers, on purpose, and RFC-000 section 12 already fixes both.

| Enforcement point | When entitlements cannot be confirmed | Stance | Why |
|-------------------|----------------------------------------|--------|-----|
| api on write | cache miss AND Postgres unavailable | fail-closed: reject the write (`ErrEntitlementsUnavailable` -> 503-class error) | A stale-permissive CREATE is permanent. If we let a monitor be created while we cannot confirm the cap, the over-limit row persists after the outage and a downgrade is bypassed forever. The cost of being wrong is unbounded. |
| scheduler on dispatch | Redis unreachable on the tick | fail-stale-permissive: clamp using the last-known snapshot (rebuilt from Postgres on boot, RFC-004), never dispatch wide-open | A stale-permissive DISPATCH is temporary: it self-corrects on the very next tick once the cache is back, and the worst case is a monitor checked at its old (already-entitled) cadence for a few seconds. Failing closed here would stop monitoring a paying customer mid-incident over a cache blip, which is worse than a few seconds of slightly-stale cadence. |

Spelled out, because the asymmetry is the whole point:

- The write path fails closed because the side effect outlives the outage. Blocking a legitimate create for the duration of a Postgres outage is annoying but recoverable (the user retries when we are healthy); allowing an illegitimate create is not (the row is there forever, under the wrong plan). When in doubt, do not let new over-limit state into the system. This is RFC-000 section 12 verbatim: "if entitlements cannot be determined the api write is rejected rather than allowed, so a downgrade can never be bypassed by knocking over the cache."

- The dispatch path stays permissive against its last-known snapshot because the effect is bounded in time and never wide-open. The scheduler rebuilt its entitlement view from Postgres on boot (RFC-004) and holds it; if Redis is briefly unreachable it keeps clamping against that snapshot rather than dispatching with no clamp at all. The snapshot can be a few seconds stale; it is never absent and never permissive beyond what the org was last known to be entitled to. RFC-000 section 12: "the scheduler, if it cannot read entitlements, holds the last known snapshot rather than dispatching wide-open." Note the precise stance: this is not "fail open" in the dangerous sense (it never ignores the limit), it is "fail to the last confirmed limit."

The principle in one line: a stale-permissive dispatch for a few seconds is acceptable; a stale-permissive monitor CREATE is not, because the create persists and the dispatch does not.

---

## 5. Enforcement point 1: api on write (PRD-006 section 5.1)

### 5.1 The write gate

Every write that touches a metered limit calls `Get(ctx, orgID)` once, then the matching pure helper, before the row is accepted. The entitlement gate runs alongside (not instead of) the `authz.Can` role gate; both must pass (RFC-003 section 7.4, D7). On a deny, the api returns the standard error envelope (`code` / `message` / `fields`, owned by RFC-012) carrying the helper's `Code` and an upsell hint so the SPA renders an inline upgrade prompt.

| Write action | Helper | Deny `code` | Envelope shape |
|--------------|--------|-------------|----------------|
| Create enabled monitor over cap | `CheckMonitorCreate` | `monitor_limit_reached` | resource-level |
| Create/update `interval_seconds` below plan floor | `CheckInterval` | `interval_below_plan_floor` | per-field on `interval_seconds` |
| `interval_seconds` below the 30s hard floor | `CheckInterval` | `interval_below_hard_floor` | per-field on `interval_seconds` |
| `regions` includes a code not in plan | `CheckRegions` | `region_not_in_plan` | per-field on `regions` |
| `regions` exceeds per-monitor cap | `CheckRegions` | `region_count_exceeded` | per-field on `regions` |
| Invite member over seats (accepted + pending) | `CheckInvite` | `seat_limit_reached` | resource-level |
| Create status page over cap | `CheckStatusPageCreate` | `status_page_limit_reached` | resource-level |
| Set custom domain without the flag | `CheckCustomDomain` | `custom_domain_not_in_plan` | per-field on `custom_domain` |
| Write via a read-only (Free) API key | `CheckAPIWrite` | `api_write_not_in_plan` | resource-level, 403 |
| Per-key rate exceeded | rate limiter (section 9) | `api_rate_limited` | resource-level, 429 + `Retry-After` |

These codes are the binding list from RFC-000 section 12 and PRD-006 appendix A. The HTTP status mapping (400 for per-field validation, 403 for the read-only write, 429 for rate, 503 for `ErrEntitlementsUnavailable`) is owned by RFC-012; this RFC owns the `code` each helper emits.

### 5.2 Counting current usage

The helpers take the current count as an argument; the api computes it from the real resource tables, not from a stored tally, so it cannot drift (PRD-006 section 2.3, 4.2/4.3):

| Limit | Count is | Source |
|-------|----------|--------|
| monitors | `COUNT(*) WHERE enabled = true` for the org | `monitors` (RFC-001) |
| seats | accepted memberships + live pending invitations | `memberships` + `invitations` (RFC-001) |
| status pages | `COUNT(*)` for the org | `status_pages` (RFC-001) |

The count read and the entitlement read both happen inside the write transaction so a concurrent create cannot race two monitors past a cap of one (the count is taken under the same transaction that inserts). RFC-012 owns the exact transaction boundary; this RFC requires the count be transactionally consistent with the insert.

### 5.3 Fail-closed on write, concretely

If `Get` returns `ErrEntitlementsUnavailable` (cache miss and Postgres unavailable, section 4.4), the api does not run the helper and does not accept the write. It returns a 503-class error, not a success and not a silent allow. This is the no-bypass guarantee: an attacker (or bad luck) cannot create over-limit state by knocking the cache over.

---

## 6. Enforcement point 2: scheduler on dispatch (PRD-006 section 5.2, RFC-004)

### 6.1 The dispatch clamp

The scheduler owns the schedule for every org's monitors and re-applies the org's interval floor and region entitlement on every dispatch, independent of the api and without trusting the stored monitor config (RFC-000 section 12, RFC-004). It reads `Get(ctx, orgID)` on the hot dispatch path (Redis, sub-ms) and applies two pure helpers:

```
effective_interval = ClampInterval(ent, monitor.interval_seconds)
                   = max(monitor.interval_seconds, ent.MinIntervalSeconds)  // never below HardFloorSeconds

dispatch_regions   = FilterRegions(ent, monitor.regions)
                   = [r for r in monitor.regions if RegionAllowed(ent, r)][:ent.RegionsPerMonitorCap]
```

- Interval: a monitor stored at 60s on an org now on Free (floor 900s) is dispatched no faster than every 15 minutes. The stored value is not mutated, so a future upgrade restores the fast cadence with no re-edit (PRD-006 section 5.2, 6.1).
- Regions: a monitor configured for 4 regions on an org now on a 1-region plan is dispatched only to the home region; the other 3 region jobs are not enqueued. Premium regions stop dispatching the moment the org leaves a premium tier.

The scheduler enqueues one check job per dispatched region (RFC-004 fan-out), so filtering the region set here is the same as not producing those jobs.

### 6.2 Why both points are needed (defense in depth)

| Threat | Caught by api write gate? | Caught by scheduler dispatch clamp? |
|--------|---------------------------|-------------------------------------|
| Create a monitor below the floor right now | yes (`CheckInterval` rejects) | n/a (never created) |
| Monitor created at 60s under Professional, then org downgrades to Free | no (it already exists; no write happens on downgrade) | yes (clamped to 900s every tick) |
| Monitor created in 4 regions under Custom, then downgrade | no (already exists) | yes (filtered to home region every tick) |

The write gate alone is not enough: it only sees writes, and a downgrade is not a write to the monitor. After a downgrade the pre-existing monitor still has a sub-floor interval and extra regions stored. Only the dispatch clamp, applied on every tick against the live entitlement, stops it from running richer than the new plan allows. The two points enforce the same floor from two directions so neither can be the single bypass. This is the master's locked enforcement model (master section 11, RFC-000 section 12: "neither trusting the other").

---

## 7. Downgrade behavior (PRD-006 section 6.2)

### 7.1 Clamp vs block

A downgrade never silently deletes a customer's monitors, members, or regions (master section 11). The limits split into two kinds:

| Limit | On downgrade | Mechanism |
|-------|--------------|-----------|
| interval floor | clampable | scheduler runs `max(stored, floor)`; stored value preserved (section 6) |
| region set | clampable | scheduler filters to `RegionsAllowed`; stored set preserved (section 6) |
| monitor count | blocking | owner must disable/delete monitors to get under the new cap |
| seats | blocking | owner must remove members / revoke invites |
| status-page count | blocking | owner must delete/unpublish pages |
| custom domain | blocking | owner must remove the custom domain |

Clampable limits need no owner action: the scheduler enforces them automatically and the stored config waits intact for a future upgrade. Blocking limits cannot be enforced without choosing what to drop, and we never pick which monitor or which teammate to remove for the customer. So the downgrade is held until the org is within the target plan.

### 7.2 The over-limit (held downgrade) state and what runs meanwhile

The billing UI presents a checklist ("to switch to Hobby, bring usage under these limits") and the downgrade button stays disabled until usage fits (PRD-006 section 6.2, 7). Until the downgrade is actually applied, the org is still on the old plan, so:

- api still enforces the old (higher) plan on writes. The plan has not changed yet; only the request to change it is pending.
- scheduler still clamps to the old plan. Nothing changes at dispatch until the new `entitlements` row is written.

The downgrade applies (new `entitlements` row written, `billing.events` produced, cache invalidated, section 8) only once the org is within the target plan for the blocking limits. At that instant the clampable limits also take effect at the next dispatch. There is no window where the org is half-downgraded.

The exception path is a forced drop-to-Free (trial expiry, or dunning fail-out on `subscription_canceled`, PRD-006 section 10.3). There the plan drops regardless of current usage; clampable limits clamp immediately at the scheduler, and the blocking over-limit resources are surfaced for the owner to resolve but monitoring is not torn down behind their back. While `past_due` (before cancel) nothing degrades; monitoring continues so a payment glitch never blinds a customer mid-incident (PRD-006 section 10.3).

---

## 8. Plan and subscription changes (PRD-006 section 6, 8)

### 8.1 One path, two sources

A plan change comes from one of two sources and both land identically (PRD-006 section 8.3: limits are enforced the same way before and after Paddle):

| Source | Phase | Trigger |
|--------|-------|---------|
| internal admin | Phase 1 (and ongoing for overrides) | operator sets the plan / writes an override |
| Paddle webhook | Phase 2 | `subscription_created/updated/canceled/renewed`, `payment_failed` |

Both flow through the same steps:

```
1. api records the change: write subscriptions, re-resolve and write the entitlements row (Postgres)
2. api produces billing.events keyed by org_id, source = admin | paddle   (RFC-002 4.7)
3. entitlement-invalidator: DEL ent:v1:{org_id}                            (RFC-002 4.7)
4. next Get(org) on either caller repopulates from the new Postgres row    (section 4.2)
5. api write gate and scheduler dispatch clamp use the new limits          (sections 5, 6)
```

Upgrade is immediate: higher caps and faster floor are live as soon as the cache is invalidated, and the scheduler simply stops clamping (effective interval becomes the stored value again, dropped regions resume), no data migration (PRD-006 section 6.1). Downgrade follows section 7.

### 8.2 How fast it takes effect

The invalidation deletes the key; the next read on each caller repopulates. The scheduler picks up the new limit on its next dispatch tick for that monitor (seconds), the api on its next write check for that org. The TTL backstop (section 4.1) bounds the worst case to 10 minutes if the invalidation message were ever lost. `billing.events` is per-org ordered (RFC-002), so an upgrade-then-immediate-downgrade applies to the cache in the order it happened and the final read sees the last state.

---

## 9. Rate limiting (PRD-006 section 5.1, RFC-012)

The per-key API rate limit is the one entitlement enforced continuously rather than on a discrete write. `APIRatePerMin` comes from the resolved entitlements (30 / 120 / 300 / 600 by tier, section 2.3).

Decision: a Redis token bucket keyed by API key id (which fixes the org, RFC-003), refilled at `APIRatePerMin` per minute. Reasoning: the rate ceiling is per key (PRD-006 section 5.1, master section 9), api is stateless so the counter must live in shared Redis (RFC-000 section 4 api failure behavior), and a token bucket gives burst tolerance plus a steady rate from one counter. The api reads `APIRatePerMin` from the same cached `Entitlements` it uses for every other check, so a plan change re-rates the key on the next cache repopulate with no separate config.

```
bucket key:  ratelimit:v1:{api_key_id}
rate:        ent.APIRatePerMin tokens / minute
on request:  take 1 token; if empty -> 429 with Retry-After (RFC-012 owns the header)
on deny:     code = api_rate_limited (section 5.1)
```

Fail-open-vs-closed for the rate limiter specifically differs from the entitlement write gate and matches RFC-000 section 4: if Redis is down, the rate limiter fails open with a conservative default rather than 503-ing every API call (RFC-000 section 4 api failure behavior: "fail-open with a conservative default on rate limit"). This is safe in a way the write gate is not: a few un-throttled requests during a cache outage do not create persistent over-limit state, where an un-checked monitor create would. The conservative default cap (a low fixed rate applied in-process when Redis is unreachable) bounds the blast radius. The exact value and the 429 envelope are RFC-012's; this RFC fixes that the rate comes from `APIRatePerMin` and that the limiter fails open conservatively.

---

## 10. Testing

### 10.1 Pure-helper unit tests (no I/O)

Every helper in section 3.2 is a pure function, so it is tested directly against a hand-built `Entitlements` with no Redis or Postgres:

- `CheckMonitorCreate`: at cap-1 allows, at cap denies with `monitor_limit_reached`.
- `CheckInterval` / `ClampInterval`: below plan floor denies `interval_below_plan_floor`; below 30s denies `interval_below_hard_floor`; clamp returns `max(stored, floor)` and never below 30s.
- `RegionAllowed` / `FilterRegions` / `CheckRegions`: a non-entitled code denies `region_not_in_plan`; over the per-monitor cap denies `region_count_exceeded`; filter drops non-entitled and truncates to the cap, home region first.
- `CheckInvite`: at total seats denies `seat_limit_reached`, counting accepted + pending.
- `CheckAPIWrite`: `read` access denies `api_write_not_in_plan`, `full` allows.

### 10.2 Acceptance criteria (PRD-006 section 10.2, binding)

| # | Criterion | How tested |
|---|-----------|------------|
| AC1 | 3rd enabled monitor on Free is blocked | api integration: Free org with 2 enabled monitors, create a 3rd -> `monitor_limit_reached`, no row inserted |
| AC2 | Interval below plan floor rejected | Free org, `interval_seconds` < 900 -> per-field `interval_below_plan_floor`; any tier < 30 -> `interval_below_hard_floor` |
| AC3 | Invite over seats blocked | Free org (1 seat, owner holds it), any invite -> `seat_limit_reached`; second pending invite on a 1-extra-seat plan also blocked |
| AC4 | Scheduler refuses a sub-floor interval after downgrade | monitor stored at 60s, org downgraded to Free; assert dispatch interval = 7200s, stored value unchanged |
| AC5 | Scheduler refuses a de-entitled region after downgrade | monitor in 4 regions, org downgraded to 1-region plan; assert only the home region job is enqueued |
| AC6 | Downgrade blocks until under the limit | org with 20 enabled monitors cannot complete a downgrade to Free (cap 10); downgrade stays held, no monitor auto-deleted; succeeds once enabled <= 10 |
| AC7 | Entitlement change takes effect promptly | after a plan change, assert `ent:v1:{org}` is deleted and the next write check + next dispatch see the new limits |
| AC8 | Free needs no card; Free API is read-only | Free API key read succeeds, write -> 403 `api_write_not_in_plan` |

### 10.3 Cache invalidation correctness test

A dedicated test because this is the subtle one:

1. Warm the cache: `Get(org)` on the old plan, assert the cached struct.
2. Apply a plan change (write `subscriptions` + new `entitlements` row, produce `billing.events`).
3. Let the `entitlement-invalidator` consume; assert `ent:v1:{org}` is gone from Redis.
4. `Get(org)` again; assert it repopulated from Postgres and returns the new limits.
5. Ordering check: assert the new `entitlements` row was written before the `billing.events` produce, so the repopulating read can never see the old row (section 4.2).
6. Redelivery check: replay the same `billing.events` record; assert the delete is a no-op and the cache stays correct (idempotent invalidation, RFC-002 section 4.7).

### 10.4 Fail-mode tests

- Write fail-closed: make Postgres unavailable on a cold cache, attempt a monitor create, assert `ErrEntitlementsUnavailable` -> 503-class, no row inserted (section 4.4, 5.3).
- Dispatch fail-stale-permissive: make Redis unreachable on a tick, assert the scheduler clamps against its last-known snapshot (not wide-open, not stopped) and self-corrects on the next tick after Redis returns (section 4.4, 6).
- Rate-limit fail-open: make Redis unreachable, assert API requests are not 503'd and fall back to the conservative in-process cap (section 9).

---

## 11. Open questions and dependencies

### 11.1 Open questions

| # | Question | Recommended default |
|---|----------|---------------------|
| Q1 | `ChannelTypesAllowed` has no column in RFC-001 section 4.2 (deviation, section 2.2). Add a `channel_types_allowed TEXT[]` column, or keep it tier-derived in the resolver? | RESOLVED: tier-derived in the resolver, no schema column and no per-org override in v1 (section 2.2). Add the column only if phased channels later need a per-org grant. No RFC-001 change. |
| Q2 | Per-field overrides (section 2.1): which fields may an operator override, and is there an audit trail for an override write? | Allow override on any numeric/flag field; emit `billing.events` with `source = admin` and `event_type = override` (RFC-002 already lists `override`) so the override is audited and invalidates the cache the same way. |
| Q3 | Cache TTL value (10 min, section 4.1). Is the self-heal window acceptable for a missed invalidation? | 10 minutes. Tunable; the primary mechanism is event invalidation, the TTL is only the backstop. |
| Q4 | Rate-limit conservative fail-open default (section 9). What fixed rate applies when Redis is down? | A low per-key rate (for example the Free 30/min) applied in-process; final value owned by RFC-012. |
| Q5 | Should `Get` serve a stale cached value on a Postgres error during repopulate, or only the boot snapshot (scheduler) / fail-closed (api)? | api fail-closed (section 4.4); scheduler uses its boot snapshot. `Get` itself returns the error and lets the caller pick; the asymmetry lives in the callers, not in `Get`. |

### 11.2 Dependencies

| RFC | Direction | What |
|-----|-----------|------|
| RFC-000 | this depends on it | library-not-service decision (section 2.6, ADR-0008), the section 12 cache + fail-closed-on-write contract, the per-org cache-key rule |
| RFC-001 | this depends on it | the `plans` / `subscriptions` / `entitlements` tables and the seed matrix (section 4.2); the `regions` catalog; the count sources (monitors/memberships/invitations/status_pages) |
| RFC-002 | this depends on it | `billing.events` schema and per-org ordering, the `entitlement-invalidator` consumer that deletes the cache key (section 4.7) |
| RFC-003 | this composes with it | the two-gate model (role gate AND entitlement gate, D7); the API-key-to-org resolution the rate limiter keys on |
| RFC-004 | depends on this | the scheduler dispatch clamp (`ClampInterval`, `FilterRegions`), the boot-snapshot fail-stale-permissive stance |
| RFC-012 | depends on this | the per-write error codes and the envelope mapping, the 429 + `Retry-After` rate-limit response, the 503 on `ErrEntitlementsUnavailable` |
| RFC-013 | downstream | the upgrade prompts and the downgrade checklist render the `code` and the held-downgrade state |

---

## 12. Decisions summary

| # | Decision | Rejected alternative |
|---|----------|----------------------|
| D1 | Resolve plan + subscription + override into one `entitlements` row, cache it whole, never recompute on the hot path | resolve on every read (reintroduces the multi-row read the cache exists to avoid) |
| D2 | One library `internal/entitlements`, linked by api and scheduler; `Get` is the only I/O, every check is pure | an entitlements service (extra hop + failure mode on the two hottest paths, ADR-0008) |
| D3 | Redis cache `ent:v1:{org_id}`, value is the whole struct, TTL 10 min as a backstop, correctness from event invalidation | no TTL (a missed invalidation pins stale limits forever); seconds-TTL (near-per-request Postgres read) |
| D4 | api write gate fails CLOSED when entitlements cannot be confirmed | fail open (a stale-permissive create persists past the outage and bypasses a downgrade forever) |
| D5 | scheduler dispatch fails STALE-PERMISSIVE: clamp against the last-known boot snapshot, never wide-open | fail closed (stops monitoring a paying customer mid-incident over a cache blip); fail wide-open (ignores the limit) |
| D6 | Two enforcement points, neither trusting the other: api `CheckInterval` on write, scheduler `ClampInterval` on dispatch | a single gate (the write gate cannot see a downgrade, which is not a write to the monitor) |
| D7 | Downgrade clamps interval + region automatically, blocks on monitor/seat/page count until the owner trims, never auto-deletes | silent delete on downgrade (master section 11 forbids destroying customer data) |
| D8 | Rate limit is a Redis token bucket keyed by API key, rate = `APIRatePerMin` from the same cached struct, fails OPEN conservatively | fail closed on the rate limiter (a cache blip 503s every API call for no persistent-state benefit) |

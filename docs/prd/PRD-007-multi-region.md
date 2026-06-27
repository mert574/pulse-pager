# PRD-007 Multi-Region

Status: Draft
Owner: Product (Principal PM)
Parent: `PRD.md` (master). This sub-PRD fully specifies the multi-region product domain derived from master sections 6.6, 6.7, 11 (region row and region cost), 12 (multi-region posture), and 13 (regional isolation).
Related sub-PRDs: PRD-002 Monitoring Engine, PRD-004 Status Pages, PRD-005 Public API and Webhooks, PRD-006 Billing and Entitlements.

This document is product behavior only. The deep technical design (control-plane and data-plane split, region health probing, failover mechanics, cost accounting) is owned by RFC-008 Multi-Region and Probe Fleet, with scheduler and worker pieces in RFC-004 and RFC-005.

---

## 1. Overview and goals

Pulse owns probe machines in several regions and checks each customer endpoint from more than one of them. Two reasons. First, a real outage looks different from a regional network problem, and some outages only show up in one part of the world (a CDN edge, a regional DNS issue, a region-specific deploy). Checking from one place cannot tell these apart. Second, our own probe regions can be slow, partitioned, or fully down, and a region that is down returns nothing. That silence must never read as the customer being down.

This capability is designed in from day one and delivered in phases (master section 12, multi-region posture). The `region` field is in the check-job and check-result schema from Phase 0 (master 6.3), so every result is attributed to where it ran. The fan-out, region selection, down policy aggregation, and probe-fleet health land at GA (Phase 2, master section 15) with a small set of regions, and the region set grows over later phases.

### Goals

- Detect regional downtime: catch an outage that only one region or one part of the world can see.
- Tell a real outage from our own probe region failing, so we never page a customer because of our own infrastructure.
- Make region a first-class attribution dimension: per-region history, per-region status, and "which region saw it fail" are stored facts, not inferred.
- Reduce per-region results into one monitor verdict with a customer-chosen `down_policy`, then feed the existing per-monitor state machine (PRD-002, master 6.4) unchanged.
- Treat region as both a plan entitlement and a real cost dimension (master 11), with cost-aware scheduling so low tiers do not run on premium regions.

### Non-goals (v1 and GA)

- This is not data-residency-first in v1. We monitor from regions for detection quality, not to keep a customer's data inside a jurisdiction. Regional data residency is designed-for and delivered later (section 10, master 12 and 15, Phase 3).
- Not building active-active multi-region control plane. The control plane stays in one home region at the 99.9% target (master 12 availability). Multi-region here is about where checks run, not about control-plane redundancy.
- Not exposing per-region internals (raw URLs, headers, assertions) anywhere new. Region attribution rides on the same redaction rules already committed (master 13, PRD-002).
- Not adding a new monitor health status. The four monitor statuses stay disabled / pending / down / up (master 6.5). Coverage-degraded is a separate signal about our own regions, not a target health value (master decision 16.7).

---

## 2. Region catalog entity

The region catalog is the set of regions Pulse operates. It is platform-owned data, not customer-owned. Customers select from it (section 3); they never define a region.

### Region attributes

| Attribute | Meaning |
|-----------|---------|
| `id` (region code) | stable machine code, for example `us-east`, `eu-west`, `ap-southeast`. Never reused once retired. This is the value carried in check-job and check-result `region` (master 6.3). |
| `display_name` | human label shown in the UI, for example "US East (Virginia)". |
| `geography` | continent or area grouping for selection and status display, for example North America, Europe, Asia-Pacific. |
| `tier` / `premium` flag | whether the region is a standard region or a premium region. Premium regions are an entitlement available only on higher plans (master 11 region row) and are a paid upsell (section 8). |
| `cost_class` | relative cost class (for example standard / premium) used for cost-aware scheduling and per-region margin tracking (section 8). Internal; not shown to customers as a number. |
| `lifecycle_state` | one of `available` / `deprecated` / `retired`. Controls whether new monitors can select it. |
| `health` | current platform-measured liveness of the region's probe fleet, derived from heartbeats (section 6). This is operational state, not a static attribute, and drives aggregation and coverage decisions. |

The `home_region` is one designated region in the catalog used as the default for new monitors and as the only region a Free plan gets (master 11). It is a flag on a catalog region, not a separate entity.

### Adding and retiring regions

- Adding a region: a new catalog row in `available`, with its tier, cost class, and geography. Because `region` is in the schema from day one and aggregation is additive (master 6.3, 12), adding a region is never a migration of meaning. A new region becomes selectable for plans whose entitlement includes it (section 3, PRD-006).
- Deprecating a region: lifecycle moves to `deprecated`. Existing monitors that already select it keep running, but new monitors and edits cannot add it. The UI and API surface a notice on monitors that still use a deprecated region.
- Retiring a region: lifecycle moves to `retired`. The region stops executing checks. Monitors that still selected it have that region dropped from their effective region set; the platform treats the drop the same as a region going unavailable (section 6 and the edge case in section 12). Retiring must never silently leave a monitor with an empty region set; the platform falls back to the monitor's home region and surfaces the change to the owner.

The full add/retire runbook and how health is measured are in RFC-008. This PRD fixes the product-visible lifecycle states and their effect on customers.

---

## 3. Customer region selection

A monitor carries a `regions` list (master 6.2, appendix A). It is a required, non-empty list of region codes; each must be a region the org's plan includes; no duplicates; the count is limited by the plan; the default is the plan's home region.

### Plan-gated selection (reference PRD-006)

Region availability is a plan entitlement and a cost dimension (master 11). The per-tier allowances are owned by PRD-006 Billing and Entitlements; the master anchors are:

| Plan | Regions per monitor | Premium regions |
|------|---------------------|-----------------|
| Free | 1 (default home region only) | No |
| Starter | up to 2 | No |
| Team | up to 4 | No |
| Business | up to 6 | Yes, included |

Enterprise (Phase 3) adds more and premium regions plus regional data residency (master 11, 15). The exact middle-tier counts stay GTM-tunable in PRD-006; this PRD depends on whatever PRD-006 sets and does not re-decide it.

### Default home region

Every new monitor defaults to the plan's home region as a single-region selection. Free stays single-region by entitlement. A customer on a paid tier opts into more regions explicitly; we do not auto-expand a monitor to all allowed regions, because each added region multiplies check volume and cost (section 11) and changes alerting sensitivity (section 5).

### Enforcement (reference PRD-006)

Region selection is enforced in two independent places, the same cross-cutting pattern as all entitlements (master 11 enforcement, PRD-006):

- At the api on write: selecting a region the plan does not include, or exceeding the region count, is rejected with the standard per-field error shape and an upsell (master appendix A `regions` rule). Selecting a premium region without the premium entitlement is rejected the same way.
- At the scheduler on dispatch: the scheduler respects the org's region entitlement on every tick, so a monitor created under a higher plan cannot keep running in a richer region set after a downgrade. The downgrade flow (PRD-006) prompts the owner to drop extra regions rather than silently dropping them, but if usage still exceeds the lower plan the scheduler will not dispatch the disallowed regions.

### UI and API

- UI: the monitor create/edit form (master 10.6) shows region selection limited to the plan's allowed regions, with premium regions visibly gated behind an upsell when the plan does not include them, plus the `down_policy` picker (section 5). Geography grouping (section 2) organizes the picker.
- API: `GET` regions lists the regions available to the org (its entitlement) so clients pick valid codes (master 9 surface, "Regions: list"). Monitor create and update accept `regions` and `down_policy`, validated against the entitlement (master 9, PRD-005). A monitor read exposes per-region status and any coverage-degraded state (master 9, section 6 here).

---

## 4. Per-region execution and attribution

### Fan-out

For each scheduled tick the scheduler enqueues one check job per selected region (master 6.7). A monitor with three selected regions produces three check jobs per tick, each tagged with its target region. The worker fleet in that region runs the request independently with the monitor's configured timeout and SSRF policy (master 6.3, 13) and writes a result tagged with its `region`.

This means check volume scales with the number of selected regions (section 11). A monitor at a 1-minute interval across four regions runs four checks per minute, not one. Scheduling accuracy and throughput targets (master 12) apply per region-job.

### Attribution

Every check result stores `region` (master 6.3). Because the field is present from Phase 0, per-region history and "which region saw it fail" are first-class:

- Per-region check history: results are filterable by region in the UI (master 10.5) and API (master 9, "results ... filterable by region").
- Which region saw it fail: when a monitor is unhealthy, the per-region verdicts that fed the aggregation are recoverable from the stored results, so the monitor detail and the incident can show which regions saw the target as unhealthy at the time the incident opened.
- Per-region status: a monitor read exposes the current per-region status (master 9, 10.5), separate from the aggregated monitor verdict.

"Check now" (master 6.3) produces a normal result per region the same way and feeds the same aggregation; it does not shift the scheduled cadence and is serialized per monitor.

---

## 5. Down policy and aggregation

The per-region results are reduced to one monitor-level healthy/unhealthy verdict by the monitor's `down_policy`, and that single verdict is handed to the existing per-monitor state machine (PRD-002, master 6.4). The state machine is unchanged: it still owns counting consecutive unhealthy verdicts, the `failure_threshold`, incident open and close, and the one-down/one-up dedup. Multi-region only changes what produces the healthy/unhealthy input.

### The down policy table

| `down_policy` | Monitor counts as unhealthy when | Behavior | Use for |
|---------------|----------------------------------|----------|---------|
| `any` | at least one healthy-region result is unhealthy | most sensitive; one bad path opens the verdict | strict; catch a regional outage fast, accept more single-region noise |
| `quorum` (default) | a majority of the healthy reporting regions return unhealthy | balanced; a single regional blip does not flip the verdict | the default for almost everyone |
| `all` | every healthy reporting region returns unhealthy | most conservative; only a clearly global outage flips the verdict | low-noise monitors where you only want to know about a global outage |

The default is `quorum` (master 6.7, appendix A) so a single regional blip does not page a customer who checks from several regions. For a single-region monitor (Free, or any monitor with one selected region) all three policies are equivalent: that one region's verdict is the monitor verdict.

### Quorum defined precisely

The hard question is the denominator. Quorum is defined as a strict majority of the regions that actually reported a usable result on this tick, after excluding regions whose probe fleet is unhealthy (section 6). It is not a majority of selected regions, because counting a silent degraded region as if it were healthy would let our own outage drag the verdict.

Let:

- `S` = the set of regions selected on the monitor.
- `D` = the subset of `S` whose probe fleet is currently unhealthy (degraded or down by heartbeat, section 6). These returned nothing usable and are excluded.
- `R` = `S` minus `D` = the healthy reporting regions for this tick. `|R|` is the quorum denominator.
- `U` = the regions in `R` that returned an unhealthy result.

Quorum verdict is unhealthy when `|U| > |R| / 2` (strict majority of `R`). With `|R| = 4` that needs 3 of 4. With `|R| = 3` it needs 2. With `|R| = 2` it needs 2 (a tie is not a majority, so 1 of 2 stays healthy). With `|R| = 1` the single region decides.

Using `R` (healthy reporters) as the denominator, not `S` (all selected), is the core decision: it is what keeps a down probe region from counting toward "the target is down." This same exclusion is why coverage can run too thin to decide (section 6).

The `any` and `all` policies use the same `R`: `any` is unhealthy when `|U| >= 1`, `all` is unhealthy when `|U| = |R|` and `|R| >= 1`. All three operate only over regions that actually reported.

### Recommended quorum denominator (open decision, see section 13)

We ship "majority of healthy reporting regions (`R`)" as the default denominator. The alternative (majority of all selected regions `S`) is rejected because it counts our own down region as an implicit healthy vote, which weakens detection exactly when our fleet is degraded. The trade-off: when several regions go silent, `R` shrinks and a small `R` can flip the verdict on fewer absolute regions, which is why we also set a minimum-regions floor for quorum to be meaningful (section 13) and surface coverage-degraded when `R` falls too low to satisfy the policy (section 6).

---

## 6. Probe-fleet health (the core hard problem)

This is the false-positive problem we will not ship: our own regions can be slow, partitioned, or fully down, and a region that is down produces no result. That absence must never be read as the customer being down (master 6.7, 13). This section defines the product behavior; the measurement and failover mechanics are RFC-008.

### Liveness

The platform tracks the health of its own regions with per-region heartbeats and liveness (master 6.7). Each region's probe fleet reports it is alive and able to run checks. From this the platform classifies a region as healthy or unhealthy at any moment. This classification is what produces `D` and `R` in section 5.

### Two situations that look alike from outside

| Situation | What it means | What the platform does |
|-----------|---------------|------------------------|
| Region returns an unhealthy result | the target is down from that region | counts toward `down_policy` (it is part of `U` within `R`) |
| Region returns no result and the region is healthy | the target did not answer this region's request in time (timeout, connection error) | this is a normal unhealthy check result, recorded with its failure reason (master 6.3), counts toward `down_policy` |
| Region returns no result and the region is unhealthy | our probe region is degraded or unavailable; says nothing about the target | excluded from `down_policy` (it is in `D`, removed from `R`), never counted as the target being down |

The distinction that matters is the third row against the others. A timeout that a healthy region reports is real signal about the target. Silence from a region our own heartbeats say is down is our problem and is dropped from the math.

### Coverage-degraded state

When too few healthy regions remain to satisfy a monitor's `down_policy`, the platform does not declare the monitor down on missing data (master 6.7). Instead it surfaces a coverage-degraded signal on the monitor, visible in the UI and API. Coverage-degraded means "we cannot currently judge this monitor with the coverage you asked for, because our own regions are short," not "your target is unhealthy."

Concretely, coverage-degraded is raised when `R` (healthy reporting regions) drops below what the policy needs to render a confident verdict:

- For `any`: at least one healthy region must report. `|R| = 0` is coverage-degraded.
- For `quorum`: a meaningful majority needs enough reporters. `|R| = 0` is coverage-degraded; below the quorum minimum-regions floor (section 13) is coverage-degraded.
- For `all`: at least one healthy region must report. `|R| = 0` is coverage-degraded.

Coverage-degraded is not one of the four monitor statuses (master 6.5, decision 16.7). It is an orthogonal indicator that can sit alongside up, down, or pending. A monitor that was up and then loses coverage shows up with a coverage-degraded indicator, not down.

### Failover of a down region's checks

Plan permitting, when a selected region's fleet is unhealthy, the platform fails that region's checks over to another healthy region so coverage is restored (master 6.7). Failover is plan-gated because it can move a check onto a region the customer did not pay for; the failover-eligibility rule by tier is an open decision (section 13). When failover restores enough healthy reporters, `R` recovers and coverage-degraded clears.

### The committed false-positive guarantee (restated)

We never page a customer because our own probe region went down (master 6.7, 13). A missing result from a region we run is our problem to handle, not a reason to open an incident. Customer incidents come only from regions that actually saw the target unhealthy, aggregated by the down policy. When our own coverage is too thin to judge, the monitor goes coverage-degraded, not down, and no incident opens and no notification fires.

---

## 7. Topology (product level)

The topology splits in two (master 6.6). This is the product-level view; the deep design is RFC-008 and RFC-000.

- Control plane (one home region): api, scheduler, alerting, notifier, PostgreSQL, and the central Kafka. It owns scheduling, the state machine, incidents, and notifications. It is the single source of truth for a monitor's state.
- Regional data planes: the worker fleets that actually reach customer endpoints, one fleet per region we operate. They run the checks and nothing else stateful.

Flow: the scheduler enqueues a check job tagged with its target region; the worker fleet in that region executes it and writes the result, tagged with the region, back to the central store; all cross-region aggregation (down policy, uptime, status pages) happens against that central store (master 6.6, 12). So checks fan out across regions but there is one place that decides a monitor's state.

Regional fleets run least-privilege with their own egress controls, the same SSRF posture as any worker, so a fleet cannot reach internal services or another region's sensitive endpoints (master 6.7, 13). A region going down is a handled failure mode (section 6), not a security or correctness incident.

---

## 8. Cost awareness

Region availability is both an entitlement and a real COGS dimension, because our own regions cost us differently (master 6.7, 11 region cost). This PRD states the product behavior; PRD-006 owns the pricing and metering model.

- Region as COGS: each region has a cost class (section 2). Premium regions cost more to run.
- Cost-aware scheduling: low tiers default to the cheaper home region so we are not paying premium-region cost on free traffic (master 11 region cost). The scheduler picks the cheaper or default region for low tiers; richer and premium regions are an opt-in on plans that include them.
- Per-region check cost tracking: the platform tracks per-region check cost so multi-region margin stays visible (master 11). Fan-out multiplies check volume (section 4, 11), so cost per monitor scales with selected region count, and this must be measurable per region for margin.
- Premium regions as a paid upsell: premium regions are an entitlement on higher plans and a conversion lever (section 3, master 11). Selecting one on a plan that does not include it is rejected with an upsell (section 3).

Failover (section 6) interacts with cost: failing a check over to a more expensive region to keep coverage is a cost the platform absorbs to honor the false-positive guarantee, which is part of why failover is plan-gated (section 13).

---

## 9. Customer-facing surfaces

- Monitor detail (master 10.5): per-region status, a recent check history table filterable by region, and a coverage-degraded indicator when our regions are short (section 6). The detail shows which regions saw the target unhealthy when an incident is open (section 4).
- Monitor create/edit form (master 10.6): region selection limited to the plan's allowed regions, premium regions gated with an upsell, and the `down_policy` picker (section 3, 5).
- Coverage-degraded indicator: shown on the monitor (list and detail) as an orthogonal signal, distinct from up/down (section 6, master decision 16.7). It tells the customer "our coverage is reduced," and it never reads as the target being down.
- API (master 9, PRD-005): list regions available to the org; monitor read exposes per-region status and coverage-degraded; results filterable by region.

### Status pages (reference PRD-004)

Status pages read from the same check and incident data, no separate probing (master 8). For multi-region, the recommendation is that a public status page shows the overall monitor verdict only and hides per-region detail in v1. Reasons:

- The public page already maps monitor status to up / down / degraded (master 8) and shows friendly names, never internals. Per-region detail leaks more about the customer's footprint and our probe geography than the page is meant to expose.
- Coverage-degraded is about our own regions, not the customer's service, so it must not appear as the customer being degraded on their public page. The page reflects the aggregated monitor verdict, which already excludes our-region failures (section 6).

Whether to ever show per-region detail publicly is an open decision (section 13), owned jointly with PRD-004.

---

## 10. Data residency (phased, later)

Regional data residency for compliance-sensitive customers is designed-for and delivered later, alongside enterprise (master 12, 15 Phase 3). In v1 and at GA, regions exist for detection quality, not to keep a customer's data inside a jurisdiction; results flow back to the central store in the home region for aggregation (section 7, master 12).

What "designed-for" means here: the `region` attribution and the control-plane and data-plane split (section 7) are the substrate residency will build on, so adding residency later is additive, not a re-architecture. The residency product (which data stays in-region, which jurisdictions, how it interacts with the central store) is out of scope for this PRD and will be its own specification when Phase 3 lands.

---

## 11. NFR ties (master section 12)

- Designed in from day one, region set phased: `region` is in the schema from Phase 0; the fan-out, selection, quorum policy, probe-fleet health, coverage-degraded, and failover land at GA with a small region set that grows over phases (master 12 multi-region posture, 15). This PRD does not move that line.
- Fan-out multiplies check volume: N selected regions means N check jobs per tick (section 4). The sustained throughput target (~10,000 checks/sec, master 12) is in terms of region-jobs, not monitors, so a multi-region monitor consumes throughput proportional to its region count. Capacity planning and scheduling accuracy (check dispatched within 5 s at p99, master 12) apply per region-job. PRD-006 region counts and cost-aware scheduling (section 8) are what keep fan-out from running away on low tiers.
- One source of truth: results write back centrally so a monitor's state is decided in one place regardless of region count (master 6.6, 12). Aggregation latency (check-result to decision within 5 s at p99, master 12) covers the reduce step in section 5.
- Control-plane availability is separate: 99.9% control plane (master 12) is not changed by multi-region checking; multi-region is a check-execution capability, not control-plane redundancy (section 1 non-goals, master 12).

---

## 12. User stories, acceptance criteria, edge cases

### User stories

- As an SRE whose API is served from multiple regions, I want to detect when only EU users are affected, so I learn about a region-specific outage that a single US probe would miss.
- As a customer, I do not want to be paged at 3am because Pulse's own us-west probes died, so a failure of Pulse's infrastructure never opens my incident.
- As a team lead, I want a single regional blip not to page us, so the default policy needs a majority of regions to agree before we are called down.
- As a developer, I want to see which region saw my service fail, so I can correlate with my own regional deploys.
- As an owner on a higher plan, I want to add a premium region close to my users, and I accept it costs more.

### Acceptance criteria (testable)

- Detect a region-only outage: a monitor with regions us-east, eu-west, ap-southeast under `any` opens an incident when only eu-west reports unhealthy and the others report healthy. Under `quorum` the same single-region failure does not open an incident (1 of 3 is not a majority).
- Quorum prevents a single-region false page: with `|R| = 3` and 1 region unhealthy, the quorum verdict is healthy and no incident opens. With 2 of 3 unhealthy, the quorum verdict is unhealthy and the state machine begins counting (PRD-002).
- Quorum denominator excludes degraded regions: with 3 selected regions where 1 region's fleet is heartbeat-unhealthy (`D`), quorum is computed over the remaining 2 (`R`), and a tie (1 of 2) stays healthy.
- Our-region-down yields coverage-degraded, not an incident: a region that returns nothing while its heartbeat says unhealthy is excluded from the verdict; if exclusions drop `R` below what the policy needs, the monitor shows coverage-degraded, no incident opens, and no notification fires. This is the committed false-positive guarantee (section 6).
- Per-region attribution correct: every stored result carries the region it ran from; results filtered by region return only that region's results; the monitor detail shows which regions saw the target unhealthy when an incident is open.
- Failover restores coverage when allowed: on a plan with failover, a down region's checks move to a healthy region, `R` recovers, and coverage-degraded clears; on a plan without failover, coverage-degraded persists until the region recovers.
- Single-region monitor is policy-agnostic: a one-region monitor produces the same verdict under any / quorum / all (that region decides).

### Edge cases

- All selected regions down (our fleets): `R = 0`. The monitor is coverage-degraded under every policy. No incident opens. No notification fires. The false-positive guarantee holds even at total coverage loss.
- All selected regions report the target unhealthy: every policy (any, quorum, all) is unhealthy; the state machine opens an incident normally once the threshold is crossed (PRD-002).
- Region retired while selected: the retired region is dropped from the monitor's effective set (section 2). If that leaves the set empty, the platform falls back to the home region and surfaces the change to the owner; the monitor is never left with zero regions.
- Premium region after downgrade: a monitor that selected a premium region under Business is downgraded to a plan without premium regions. PRD-006 prompts the owner to drop it; until usage is brought under the limit, the scheduler does not dispatch the disallowed region, and the monitor's effective region set shrinks (section 3 enforcement). If shrinkage drops coverage below the policy, coverage-degraded shows.
- A healthy region reporting a timeout: this is real signal, counted as an unhealthy result toward the policy, not excluded (section 6 table). Only heartbeat-unhealthy regions are excluded.
- Quorum with `|R| = 1` after exclusions: a single surviving healthy region decides, but only if it clears the minimum-regions floor for quorum (section 13); below the floor it is coverage-degraded rather than a one-region quorum.

---

## 13. Open decisions (recommended defaults)

1. Quorum denominator. Recommended: strict majority of healthy reporting regions (`R`), not all selected regions (`S`) (section 5). Trade-off: a shrinking `R` can flip the verdict on fewer absolute regions, mitigated by the minimum-regions floor below and by coverage-degraded. Rejected `S` because it counts our own down region as a healthy vote.

2. Failover default on or off by tier. Recommended: failover on for paid tiers that include more than one region, off for Free (Free is single-region, so there is nowhere to fail over). The cost of failing onto a more expensive region is absorbed to honor the false-positive guarantee (section 8). Trade-off: a customer can briefly be checked from a region they did not pick; acceptable because it only happens to keep coverage during our own outage and never changes the public verdict math beyond restoring reporters.

3. Per-region detail on public status pages. Recommended: overall verdict only in v1, per-region hidden (section 9, with PRD-004). Trade-off: a customer cannot show regional detail to their users yet; revisit when there is demand, since it exposes more of both the customer footprint and our probe geography.

4. Minimum regions for quorum to be meaningful. Recommended: quorum needs at least 2 healthy reporting regions (`|R| >= 2`) to render a quorum verdict; below that it is coverage-degraded rather than a one-region "quorum." Trade-off: a multi-region monitor that loses all but one region stops producing a quorum verdict and goes coverage-degraded instead of silently behaving like a single-region monitor; this is the safer reading of "majority" and keeps the guarantee clean.

---

## 14. Dependencies

- PRD-002 Monitoring Engine: the per-monitor state machine, monitor statuses, incidents, and check execution semantics. Multi-region produces the single healthy/unhealthy verdict that feeds the state machine unchanged (master 6.4, 6.5; section 5 here).
- PRD-006 Billing and Entitlements: per-tier region counts, premium region entitlement, region as a COGS and metering dimension, cost-aware scheduling defaults, and downgrade handling. This PRD depends on PRD-006's tier numbers and does not re-decide them (master 11; sections 3, 8 here).
- PRD-004 Status Pages: how the aggregated verdict and (not) per-region detail render publicly (master 8; section 9 here).
- PRD-005 Public API and Webhooks: the region list endpoint, region and down_policy on monitor create/update, per-region status and coverage-degraded on monitor reads, and results filterable by region (master 9; sections 3, 4, 9 here).

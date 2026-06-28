# RFC-008 - Multi-Region and Probe Fleet

Status: DRAFT for review
Author: Principal Architecture (distributed systems)
Audience: scheduler (RFC-004), worker (RFC-005), alerting (RFC-006), entitlements (RFC-009), infra (RFC-011) authors, and on-call.
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 4 topology, ADR-0006 cross-region messaging, section 8 consistency/ordering, the control-plane/data-plane split).
Source RFCs: `RFC-002-eventing-kafka-contracts.md` (`check.jobs.<region>`, `check.results` mirror, `region.health` compacted+mirrored, the mirror seam it explicitly delegates here), `RFC-001-data-model-and-multitenancy.md` (`regions` catalog, `region_health`), and the check.jobs/results contracts.
Product source: `docs/prd/PRD-007-multi-region.md` (region selection, `down_policy`, the `R`-set quorum denominator, coverage-degraded, failover, region cost, the never-page-on-our-own-region-failure guarantee), master PRD 6.6 / 6.7 / 12 / 13.

House style: RFC3339 UTC timestamps on the wire. No em-dashes. Tables and diagrams over prose.

Note on consumer RFCs: RFC-005 (worker) and RFC-006 (alerting) both exist now. This RFC designs against the RFC-000 / RFC-002 contracts and PRD-007, and where it needs a model from a consumer (the aggregation window in section 6), it specifies the model those consumers adopt and flags it.

---

## 1. Overview, scope, and the committed guarantee

### 1.1 What this RFC owns

| Owned here | Delegated |
|------------|-----------|
| Probe-fleet health detection: heartbeat cadence, what a heartbeat asserts, the healthy / degraded / down classification, thresholds, time windows, hysteresis | RFC-002 owns the `region.health` wire schema; this RFC sets the detection-latency and staleness bounds RFC-002 open question 4 leaves to us |
| The healthy-reporting set `R` definition (heartbeat-healthy AND result arrived within the round window) | PRD-007 owns the `down_policy` semantics over `R`; the distributed mechanism that builds `R` is here |
| The verdict-input contract to alerting (RFC-006): the round window, per-region outcomes, region health state, coverage-degraded signal | RFC-006 runs the reduce and the state machine; this RFC defines exactly what it reads |
| The failover policy and mechanism (scheduler re-targets a down region's checks) | RFC-004 implements the scheduler re-target; PRD-007 / PRD-006 own the per-tier on/off rule |
| The regional Kafka topology, the mirror seam (topic naming across clusters, ordering through the mirror), and the egress cost trade-off | RFC-002 fixes the topic catalog; RFC-011 provisions clusters and runs MirrorMaker 2 |
| Region catalog lifecycle operations (add / deprecate / retire runbook) and cost attribution | RFC-001 owns the `regions` / `region_health` schema; PRD-007 / PRD-006 own the product lifecycle states and pricing |

### 1.2 Scope boundary

In scope: region health detection, coverage-degraded, failover, regional Kafka topology and egress cost, cost-aware scheduling inputs, the verdict-input contract, data-residency substrate.

Out of scope: the per-monitor state machine and incident lifecycle (RFC-006 / `internal/alerting.Apply`, reused unchanged), the check execution and SSRF posture (RFC-005), the entitlement numbers (PRD-006 / RFC-009), the wire schemas (RFC-002), cluster provisioning and MM2 operation (RFC-011).

### 1.3 The committed guarantee

> We never page a customer because our own probe region went down.

A region that is down returns nothing. That silence is our problem, not the customer's outage. The mechanism that makes this true: a heartbeat-unhealthy region is excluded from the down-policy denominator (`R`), so its absence is never read as an implicit "the target is down" vote. When exclusions drop `R` below what the policy needs to judge, the monitor goes coverage-degraded (an orthogonal signal), and no incident opens and no notification fires. This is binding and is the single correctness property the rest of this RFC defends.

The mirror image of the guarantee, equally load-bearing: a timeout or connection error reported BY a healthy region is real signal about the target and IS counted. The distinction is "who returned nothing" (the region's heartbeat), not "what the result said."

---

## 2. Control plane vs regional data planes

This details RFC-000 section 4.1. The split is hard: all state and all decisions are central; a region only executes checks and reports.

### 2.1 What runs where

| Plane | Location | Components | Holds state? | Makes product decisions? |
|-------|----------|------------|--------------|--------------------------|
| Control plane | one home region, k8s namespace `pulse-control` (+ `pulse-system`) | api, scheduler (singleton, leader-elected), alerting, notifier, PostgreSQL (primary + replicas), Redis, control Kafka cluster, observability (Prometheus / Grafana / OTel / logs) | yes, all of it | yes, all of them |
| Regional data plane | one per operated region, k8s namespace `pulse-region-<code>` | stateless worker fleet (HPA) + the regional Kafka cluster | no durable product state (only the regional broker's transient job/result buffer) | no |

The region controller is a small control-plane responsibility, not a regional component. It consumes mirrored `region.health`, applies the detection logic in section 5, and writes `region_health` in Postgres. It runs in the control plane because the decision must be central. (It can be a goroutine inside alerting or scheduler, or a tiny separate deployment; RFC-011 decides placement. This RFC only fixes that the decision is central.)

### 2.2 Diagram

```
                         CONTROL PLANE (home region)
  +----------------------------------------------------------------------+
  |  api      scheduler(singleton)     alerting        notifier          |
  |   |          |  produce              | consume        | consume       |
  |   |          |  check.jobs.<region>  | check.results  | notify.events |
  |   |          |  (to regional cluster)| reduce verdict |               |
  |   v          v                       | over R         |               |
  |  Postgres  control Kafka  <----------+----------------+               |
  |  (regions, (control topics,          ^                                |
  |  region_   + MIRROR TARGET           | mirror in: check.results,      |
  |  health,   for results+health)       | region.health (per region)     |
  |  incidents)                          |                                |
  |  Redis (region-health snapshot for alerting verdict, locks)           |
  |  region controller: reads region.health -> writes region_health (PG)  |
  +-----------+------------------------------------------+----------------+
              | check.jobs.<region>                      ^ MirrorMaker 2
              | (produced INTO regional cluster)         | (results + health
              v                                          |  mirrored home)
  +-----------+-------------------+        +-------------+-----------------+
  | REGIONAL DATA PLANE eu-west   |        | REGIONAL DATA PLANE us-east   |
  |  regional Kafka cluster       |        |  regional Kafka cluster       |
  |   check.jobs.eu-west          |        |   check.jobs.us-east          |
  |   check.results (local) ------+--MM2-->|   check.results (local) --MM2-+--> home
  |   region.health (local) ------+--MM2-->|   region.health (local) -------+--> home
  |  worker fleet (HPA, stateless)|        |  worker fleet (HPA, stateless)|
  |   consume jobs locally        |        |   consume jobs locally        |
  |   run HTTP check + SSRF guard  |        |   run HTTP check + SSRF guard  |
  |   write result row -> control |        |   emit heartbeats             |
  |   PG, emit check.results local|        |                               |
  +-------------------------------+        +-------------------------------+
```

Note on the result row write: the worker writes the durable `check_results` row to the control-plane Postgres (RFC-000 section 2.3, the row is authoritative history) AND emits the `check.results` event into its regional Kafka cluster, which MM2 mirrors home to drive alerting. The Postgres write crosses the region boundary; the result event consume stays local until mirrored. A brief home-region partition stalls the row write but not in-region checking or local result buffering (section 3.5).

### 2.3 Why central decisions, regional execution

If a whole region disappears the control plane keeps every byte of state and simply sees missing results. Probe-fleet health (section 5) turns that absence into coverage-degraded, never a customer incident. There is no scenario where losing a region corrupts or loses a monitor's state, because the region held none.

---

## 3. Regional Kafka and the mirror (details RFC-000 ADR-0006 / RFC-002 section 7)

### 3.1 Topology

| Flow | From -> to | Topic | Transport | Mirrored? |
|------|-----------|-------|-----------|-----------|
| jobs out | scheduler -> regional cluster | `check.jobs.<region>` | scheduler produces directly INTO the region's cluster | no (jobs never mirror) |
| results home | regional cluster -> control cluster | `check.results` | MirrorMaker 2 | yes |
| heartbeats home | regional cluster -> control cluster | `region.health` | MirrorMaker 2 | yes |

The scheduler in the control plane produces a region's jobs straight into that region's Kafka cluster. Workers consume `check.jobs.<region>` locally and low-latency. They emit `check.results` and `region.health` into the regional cluster; MM2 mirrors only those two topics home. Everything else (`monitor.changed`, `notify.events`, `audit.events`, `billing.events`, `webhook.delivery`) is control-plane only and never crosses a boundary (RFC-002 section 3.1).

Why a regional cluster and not one central cluster region-keyed (RFC-000 ADR-0006, restated): a local broker keeps worker consume local and low-latency, contains blast radius (a regional broker incident does not touch the central bus api/alerting/notifier depend on), and a region<->home partition does not stall in-region checking. Workers keep draining their local job queue through a brief home-region partition.

At Phase 0/1 there is exactly one region (home), so there is no mirror and no egress. The topic-per-region naming and the mirror consumer group exist from day one, so GA multi-region rollout is additive, never a migration (RFC-000 section 4.2).

### 3.2 Topic naming across clusters

| Decision | Value |
|----------|-------|
| Mirrored topic name | preserved unprefixed: `check.results` on a regional cluster mirrors to `check.results` on the control cluster (no source-cluster alias prefix) |
| Region-scoped job topic | `check.jobs.<region>` where `<region>` is the region code (`us-east`, `eu-west`, `ap-southeast`), one per operated region, living on that region's cluster |
| How consumers see origin | the `region` field in the `check.results` and `region.health` payload is the authoritative origin, not the topic name |

Reasoning: central `alerting` and `scheduler` consume one logical `check.results` / `region.health` regardless of which region produced it. Default MM2 prefixes mirrored topics with the source alias (`<src>.check.results`); we configure an explicit topic mapping so all regional sources mirror into the single unprefixed central topic. The trade-off (losing the auto source prefix) is acceptable because the `region` payload field already carries origin, and it avoids forcing the consumer to subscribe to N region-prefixed topics and reconstruct one stream. RFC-011 owns the MM2 topic-mapping config.

### 3.3 Ordering preservation through the mirror

| Property | How it holds |
|----------|--------------|
| Per-monitor order survives the mirror | `check.results` is keyed by `monitor_id`. MM2 preserves per-partition order and maps source partition to the same destination partition, so a monitor's results stay on one partition end to end |
| No cross-region interleaving corrupts a monitor's run | a monitor's results for one region are produced in order in that region; across regions, alerting reduces per-region outcomes within the round window (section 6) rather than relying on a global order, so a small mirror delay between regions is tolerated |
| Latest-wins per region for health | `region.health` is compacted by `region` key; a fresh central consumer reads current liveness per region on join after the mirror catches up |

The down-policy reduce tolerates results arriving slightly apart within the round window, so mirror latency under that window is invisible to correctness. The window must therefore be larger than expected mirror lag (section 6.3).

### 3.4 Egress cost trade-off

| Fact | Detail |
|------|--------|
| What costs | mirroring `check.results` + `region.health` home is cross-region egress, billed per region per byte |
| Why it is bounded | one small JSON row per check per region (a few hundred bytes), and we mirror ONLY these two topics, never the job stream. Jobs (the larger payload, carrying check config and a secret header) are produced into the region and never leave it |
| Why low tiers stay cheap | premium regions are a paid entitlement (PRD-006), so we never pay premium-region egress on free traffic. Free is single-region (home), so Free produces zero mirror egress |
| Phase 0/1 | one region, no mirror, no egress |

Egress is a first-class COGS input to the cost model in section 9. It scales linearly with mirrored result volume, which is the fan-out-multiplied check rate (section 10).

### 3.5 A region whose cluster is unreachable from the control plane

This is the partition case. Two directions fail independently:

| Direction broken | Effect | Handling |
|------------------|--------|----------|
| control -> region (scheduler cannot produce `check.jobs.<region>`) | no new jobs reach the region; in-flight buffered jobs still run; new ticks for that region are not dispatched | scheduler treats produce failure to a region's cluster as a dispatch failure for that region; the region stops getting fresh jobs. Missing results -> the region drops out of `R` (no result in the round window, section 5.4) -> coverage-degraded or failover, never a false page |
| region -> control (MM2 cannot mirror `check.results` / `region.health` home) | results and heartbeats pile up locally; the control plane sees the region's last-known `region.health` go stale and result flow stop | staleness detection (section 5.3) flips the region to degraded then down on the staleness bound; the region drops out of `R`. When the link heals, MM2 drains the backlog (regional retention 24h for results, RFC-002 section 3.4) and the late results land in Postgres as upserts (the `(org_id, monitor_id, region, checked_at)` unique key makes a late mirror a no-op if the row already exists). Late results that arrive after their round window has closed do not retroactively flip a past verdict; they are stored for history only |

The core safety property in both directions: a region we cannot reach contributes nothing to `R`, so it can never drag a verdict. The worst case is reduced coverage (coverage-degraded), which is exactly the designed behavior.

---

## 4. Region catalog

RFC-001 section 4.5 owns the schema; this RFC owns the lifecycle runbook and how health feeds it.

### 4.1 The `regions` table (RFC-001 section 4.5, restated)

| Column | Meaning |
|--------|---------|
| `code` (PK) | stable machine code (`us-east`, `eu-west`, `ap-southeast`), never reused once retired; carried in `check.jobs` / `check.results` / `region.health` `region` |
| `display_name` | UI label, e.g. "US East (Virginia)" |
| `geography` | grouping for selection and status display (North America / Europe / Asia-Pacific) |
| `cost_class` | `standard` / `premium`; drives cost-aware scheduling and per-region margin (section 9). Internal, never shown as a number |
| `is_premium` | premium-region entitlement flag (PRD-006 gates selection) |
| `is_home` | the one home region: control plane location, default for new monitors, the only region Free gets |
| `lifecycle` | `available` / `deprecated` / `retired`; controls whether new monitors can select it |

`region_health` (RFC-001 section 4.5) holds current liveness per region (`status`, `last_heartbeat`, `updated_at`), written by the region controller from mirrored `region.health` (section 5). Deviation flag in section 5.2 on the `status` enum width.

### 4.2 Adding a region

| Step | Action |
|------|--------|
| 1. Stand up the data plane | RFC-011 brings up the regional Kafka cluster and the worker fleet in `pulse-region-<code>`, plus the MM2 flow that mirrors that region's `check.results` + `region.health` home |
| 2. Register the catalog row | insert into `regions` with `lifecycle = 'available'`, its `cost_class`, `is_premium`, `geography`. It is now selectable for plans whose entitlement includes it (PRD-006) |
| 3. Prove health before dispatch | the new fleet emits `region.health` heartbeats; the region must reach `healthy` (section 5) before the scheduler starts dispatching customer jobs to it. Until then it is `available` in the catalog but not yet healthy, so `R` excludes it and no monitor depends on it |
| 4. Start scheduling | once healthy, the scheduler includes it in fan-out for monitors that selected it |

Because `region` is in the schema from Phase 0 and aggregation is additive over `R`, adding a region is never a migration of meaning (PRD-007 section 2).

### 4.3 Deprecating a region

`lifecycle = 'deprecated'`. Existing monitors that already selected it keep running; new monitors and edits cannot add it. The UI and API surface a notice on monitors that still use a deprecated region (PRD-007 section 2). No dispatch change yet.

### 4.4 Retiring a region

| Step | Action |
|------|--------|
| 1. Drain | stop accepting new selections (already true if deprecated first); let in-flight jobs finish |
| 2. Stop scheduling | `lifecycle = 'retired'`; the scheduler stops dispatching `check.jobs.<code>` to it. `region.health` carries `lifecycle = 'retired'` so consumers know to stop counting it |
| 3. Drop from effective sets | every monitor that still selected the retired region has it dropped from its effective region set. The drop is treated exactly like a region going unavailable: it leaves `R` |
| 4. Never leave an empty set | if dropping the retired region would leave a monitor with zero regions, the platform falls back to the monitor's home region and surfaces the change to the owner (PRD-007 section 2, edge case section 12) |
| 5. Tear down the data plane | RFC-011 removes the worker fleet, the regional cluster, and the MM2 flow after the drain window |

`code` is never reused after retirement, so stored history that references a retired region stays unambiguous.

---

## 5. Probe-fleet health detection (the core)

This is the mechanism behind the false-positive guarantee. RFC-002 fixes the `region.health` wire schema; this section fixes the cadence, what a heartbeat asserts, the classification, and the thresholds RFC-002 open question 4 leaves to us.

### 5.1 The heartbeat: cadence and what it asserts

| Property | Value | Reasoning |
|----------|-------|-----------|
| Producer | each worker emits a heartbeat; an optional region controller can aggregate, but the per-worker emit is the source | a heartbeat from a worker proves that worker is alive and able to consume jobs and reach its local broker |
| Cadence | every 10 s per worker | short enough to detect a fleet going dark within tens of seconds, long enough that heartbeat traffic is negligible against the check firehose |
| Key | `region` (compacted topic, latest-wins per region) | the control plane reads one current liveness per region |
| What one heartbeat asserts | (a) the worker process is alive, (b) it is consuming `check.jobs.<region>` from the regional broker (Kafka reachable), and (c) it has recently completed at least one dispatch->result flow (a check ran and a result was written) within the heartbeat window | a heartbeat is not just "process up"; it asserts the region can actually do its one job. A worker that is up but cannot consume jobs or cannot reach the broker is not healthy coverage |
| Payload (RFC-002 section 4.6) | `status` (healthy / degraded / unhealthy), `healthy_workers` count, `reason` (e.g. `no_heartbeat_30s`), `lifecycle_state` | `healthy_workers` lets the control plane reason about thinning coverage before a region is fully dark |

The region's aggregate liveness is a roll-up across its workers: a region is backed by `healthy_workers` live workers. The control plane derives the region status (section 5.3) from heartbeat recency and the worker count, not from a single worker's self-report.

### 5.2 Classification: healthy / degraded / down

| State | Meaning | Drives |
|-------|---------|--------|
| `healthy` | heartbeats are fresh AND `healthy_workers` is above the floor AND the dispatch->result flow is recent | region is in `R` (it is a valid reporter) |
| `degraded` | heartbeats are arriving but late, or `healthy_workers` dropped below the floor, or the dispatch->result flow stalled but the region is not fully dark | region is EXCLUDED from `R` (treated like down for the verdict), but the scheduler may keep dispatching while it watches; this is the early-warning / failover-trigger state |
| `unhealthy` (down) | no fresh heartbeat past the staleness bound, or `healthy_workers` = 0 | region is EXCLUDED from `R`; the scheduler may fail its checks over (section 8) |

For the verdict denominator, `degraded` and `unhealthy` are the same: not `healthy` means not in `R` (PRD-007 section 5.2, RFC-000 section 4.1). The distinction between degraded and unhealthy matters for operations and for failover timing (degraded is the warning, unhealthy is the trigger), not for the down-policy math.

Deviation flag (RFC-001 vs RFC-002): `region_health.status` in RFC-001 section 4.5 is a two-value CHECK (`healthy` / `unhealthy`), while the `region.health` event (RFC-002 section 4.6) carries three values (`healthy` / `degraded` / `unhealthy`). This RFC needs the three-value model so degraded can be the failover early-warning state. Recommendation: widen `region_health.status` to `CHECK (status IN ('healthy','degraded','unhealthy'))`. This is a one-line additive migration owned by RFC-001; until then the controller can collapse degraded into unhealthy when persisting, since both exclude from `R`, and keep the three-value distinction in the live `region.health` stream and Redis snapshot. Flagged for RFC-001.

### 5.3 Thresholds, windows, and hysteresis

| Parameter | Value | What it controls |
|-----------|-------|------------------|
| Heartbeat cadence `H` | 10 s | how often a worker asserts liveness |
| Degraded staleness `T_degraded` | 30 s (3 missed heartbeats) | latest `region.health` older than this -> degraded |
| Down staleness `T_down` | 60 s (6 missed heartbeats) | latest `region.health` older than this -> unhealthy. This is the staleness bound RFC-002 section 4.6 / open question 4 leaves to RFC-008 |
| Worker floor | `healthy_workers >= 1` for healthy at all; a per-region minimum (RFC-011 tunable) for full healthy | a region with workers thinning toward zero degrades before it goes fully dark |
| Detection latency target | a region going fully dark is classified down within `T_down` + mirror lag (target under 90 s p99) | the SLO RFC-002 open question 4 asks us to set |

Hysteresis (anti-flap):

| Direction | Rule | Why |
|-----------|------|-----|
| Healthy -> degraded / down | immediate on crossing the staleness bound | we want to stop trusting a silent region fast; the cost of excluding a region early is only reduced coverage, never a false page |
| Degraded / down -> healthy | requires `recovery_window` of 60 s (6 consecutive fresh heartbeats at full worker count) before the region re-enters `R` | a region must prove sustained recovery, not a single lucky heartbeat, before we let it back into the verdict. This prevents a flapping region from oscillating the denominator and churning verdicts |

The asymmetry is deliberate: fast to distrust, slow to re-trust. Excluding a healthy region briefly only narrows coverage; re-admitting a flapping region too eagerly risks letting its noise into the verdict. A flapping region (heartbeats arriving intermittently around the bound) therefore settles into degraded and stays excluded until it is sustainedly fresh, rather than rapidly toggling `R` (failure mode section 12).

### 5.4 The healthy-reporting set `R`, defined precisely

`R` is the denominator for every down-policy. It is built per (monitor, round) at reduce time by alerting, from two facts this RFC supplies.

```
For a monitor M with selected regions S, for a check round at scheduled_at = t:

  health_healthy(r)  := region r's current region_health status is 'healthy'
                        (degraded and unhealthy both fail this)
  reported(r, t)     := a check.results row exists for (M, r, checked_at within
                        the round window of t)  -- i.e. a result actually arrived

  D = { r in S : NOT health_healthy(r) }            -- excluded: our fleet is not healthy
  R = { r in S : health_healthy(r) AND reported(r, t) }
  U = { r in R : that result is unhealthy }

  |R| is the quorum denominator.
```

The two conditions for membership in `R` are both required:

| Condition | Why both are needed |
|-----------|---------------------|
| `health_healthy(r)` | a region whose heartbeat says it is degraded/down is excluded even if a stale result trickled in. Our own degraded region must not vote |
| `reported(r, t)` | a region that is heartbeat-healthy but whose result for this round has not arrived (in flight, or just slow) is not yet counted for this round. It is not in `D` (it is not unhealthy), but it is not yet in `R` either, until its result lands within the window |

The third row of the PRD-007 section 6 table is the one this defends: "region returned nothing AND region is unhealthy" -> the region is in `D`, removed from `R`, never counted. Contrast: "region returned no result and the region is healthy" -> that is a normal unhealthy result (timeout / connection error) the worker recorded with a `failure_reason`, so `reported(r, t)` is true and the region is in `R` with an unhealthy outcome (it counts toward `U`). The difference is entirely whether a `check.results` row exists: a healthy region that times out STILL writes a result row (`healthy=false`, `failure_reason=timeout`); a down region writes nothing.

Edge: a heartbeat-healthy region whose result never arrives within the window (genuinely lost in the mirror, or the worker crashed mid-round without writing) is excluded from `R` for that round (not in `U`, not a healthy vote). It neither pages nor masks; if enough such gaps drop `|R|` below the policy floor, coverage-degraded fires (section 7). This is the safe reading.

---

## 6. Verdict inputs to alerting (the contract to RFC-006)

RFC-006 reduces per-region results to one verdict and runs the reused `internal/alerting.Apply`. The reduce happens BEFORE `Apply`, which still sees a single healthy/unhealthy input (RFC-000 section 14, the pure state machine is untouched). This section fixes exactly what RFC-006 reads from us.

### 6.1 What alerting reads

| Input | Source | Shape |
|-------|--------|-------|
| Per-region region health | `region_health` (Postgres) + a Redis snapshot the region controller keeps fresh from `region.health` | for each region: `status` (healthy / degraded / unhealthy), `last_heartbeat` |
| Per-(monitor, scheduled_at, region) results | `check.results` events keyed by `monitor_id`, consumed within the round window | the RFC-002 section 4.4 payload: `monitor_id`, `region`, `checked_at`, `healthy`, `failure_reason`, ... |
| The monitor's `down_policy` and selected regions `S` | `monitor.changed` snapshot (the scheduler already carries it) / Postgres | `down_policy` enum, `regions` array |
| The round window | this RFC (section 6.3) | a fixed duration alerting buffers results over before reducing |
| Coverage-degraded signal | computed by alerting from `|R|` vs the policy floor (section 7), using the region health this RFC supplies | a boolean per (monitor, round) |

The contract is: this RFC guarantees `region_health` reflects current liveness within the detection-latency bound (section 5.3), and that a healthy region's timeout is a real `check.results` row. Alerting builds `R` from `health_healthy(r) AND reported(r, t)` (section 5.4), reduces `U` over `R` by `down_policy` (PRD-007 section 5), and feeds the single verdict to `Apply`. When `|R|` is below the policy floor, alerting raises coverage-degraded instead of a verdict (section 7).

### 6.2 The reduce, restated (PRD-007 section 5, the semantics alerting implements)

| `down_policy` | Unhealthy when | Coverage-degraded when |
|---------------|----------------|------------------------|
| `any` | `|U| >= 1` | `|R| = 0` |
| `quorum` (default) | `|U| > |R| / 2` (strict majority of `R`) | `|R| = 0`, or `|R| < 2` (the quorum floor, PRD-007 section 13 decision 4) |
| `all` | `|U| = |R|` and `|R| >= 1` | `|R| = 0` |

Single-region monitor: all three policies are equivalent; that one region's result decides (if it is in `R`). The quorum floor (`|R| >= 2`) means a multi-region quorum monitor that loses all but one region goes coverage-degraded rather than silently behaving like a one-region monitor (PRD-007 decision 4).

### 6.3 The round window: one model, reconciled with RFC-006's aggregation window

This is the question RFC-002 section 7.4 and RFC-000 section 8 flag: a monitor's regions report slightly apart, and alerting needs one window to collect them before reducing. This RFC specifies the model, which RFC-006 adopts.

> One model: the round window and RFC-006's aggregation window are the same thing. There is exactly one window, anchored on `scheduled_at`, owned by this RFC, consumed by RFC-006.

| Parameter | Value | Reasoning |
|-----------|-------|-----------|
| Anchor | the job's `scheduled_at` (RFC-002 section 4.3), carried through to `check.results` | all of a round's per-region results share the same `scheduled_at`-derived round key, so alerting groups them deterministically regardless of arrival order |
| Round key | `(monitor_id, scheduled_at)` | the natural grouping; the worker stamps `checked_at` from the actual run but the round is identified by `scheduled_at` |
| Window length `W` | 5 s after the first result of a round arrives, bounded so the reduce still meets the 5 s result-to-decision SLO (RFC-000 section 12) | W must exceed expected mirror lag (section 3.3) so a slightly-late region still lands in its round; 5 s comfortably covers MM2 lag at our scale |
| Close trigger | reduce when all `R`-eligible regions have reported OR `W` elapses, whichever first | early close when every healthy region is in; the timer bounds the wait when a region is silent |
| Late arrivals | a result that arrives after its round closed is stored for history (the Postgres row) but does not reopen or flip the closed verdict | keeps verdicts monotonic; a late result cannot retroactively page or un-page |

Why anchor on `scheduled_at` and not wall-clock arrival: the fan-out produces N jobs for one tick that share `scheduled_at`. Grouping by `scheduled_at` makes the round deterministic and idempotent under redelivery (a redelivered result has the same `scheduled_at` and round key), and it survives mirror reordering because the round key travels in the payload, not in the arrival order.

Binding on RFC-006: RFC-006 MUST use this single window and this round key. It must not introduce a second, separate aggregation window. If RFC-006 needs a different W for a perf reason, it changes W here and this RFC re-checks the SLO and mirror-lag bound; it does not fork the model.

---

## 7. Coverage-degraded

### 7.1 Definition

Coverage-degraded is an orthogonal monitor signal, NOT one of the four monitor statuses (`disabled` / `pending` / `down` / `up`, master 6.5, decision 16.7). It means "we cannot currently judge this monitor with the coverage you asked for, because our own regions are short," not "your target is down." It can sit alongside `up`, `down`, or `pending`.

### 7.2 When it triggers

Per the policy floors in section 6.2: when `|R|` drops below what the `down_policy` needs to render a confident verdict (any/all: `|R| = 0`; quorum: `|R| = 0` or `|R| < 2`). The `|R|` here is built from `health_healthy(r) AND reported(r, t)` (section 5.4), so coverage-degraded is driven by OUR region health, never by the target.

### 7.3 What the customer sees and what does not happen

| Surface | Behavior |
|---------|----------|
| UI (monitor list + detail) | a coverage-degraded indicator, visibly distinct from up/down (PRD-007 section 9). It reads "our coverage is reduced," never "your target is down" |
| API (monitor read) | a `coverage_degraded` boolean / state alongside per-region status (PRD-005, PRD-007 section 9) |
| Public status page | NOT shown. The page reflects the aggregated verdict only, which already excludes our-region failures (PRD-007 section 9, decision 3) |
| Incident | none opens on missing data |
| Notification | none fires on missing data |

The last two rows are the committed guarantee made operational: no incident, no notification when the only thing wrong is our own coverage. When failover (section 8) restores enough healthy reporters or the region recovers, `R` recovers and coverage-degraded clears automatically.

---

## 8. Failover

Plan-permitting, when a selected region's fleet is unhealthy the platform re-dispatches that region's checks to another healthy region so coverage is restored (PRD-007 section 6).

### 8.1 Mechanism

| Step | Action |
|------|--------|
| Trigger | the region controller marks a selected region `degraded` (early) or `unhealthy` (firm). The scheduler reads `region.health` / `region_health` and sees the region is not healthy |
| Re-target | the scheduler, on its next tick for an affected monitor, produces that region's job into a DIFFERENT healthy region's cluster (`check.jobs.<failover_region>`) instead of the down region's. The job still carries the monitor snapshot; only the target region changes |
| Choice of failover region | a healthy region the monitor's plan permits, preferring same `geography` and lower `cost_class` (section 9). RFC-004 picks; this RFC sets the preference order |
| Attribution | the failed-over check runs from a real, healthy region and writes a real `check.results` row tagged with the region it actually ran from. It is a genuine reporter |
| Clear | when the original region returns to `healthy` (after the recovery window, section 5.3), the scheduler stops failing over and resumes dispatching to it |

### 8.2 Policy

| Tier | Failover | Reasoning |
|------|----------|-----------|
| Free | off | Free is single-region (home only); there is nowhere to fail over to. If home is down, the monitor is coverage-degraded |
| Paid tiers with > 1 region (Professional / Custom) | on | failover keeps coverage during OUR outage; the cost of running on a possibly-pricier region is absorbed to honor the false-positive guarantee |

This is PRD-007 section 13 decision 2. The per-tier on/off is owned jointly with PRD-006; this RFC implements the default above and flags any tuning to PRD-006.

### 8.3 Interaction with the down-policy denominator

A failed-over check is still a real region's result, so it is a genuine member of `R`:

```
Monitor selects S = {us-east, eu-west, ap-southeast}, down_policy = quorum.
ap-southeast fleet goes unhealthy -> in D, would leave R = {us-east, eu-west}, |R| = 2.
Failover re-targets ap-southeast's check to eu-central (healthy, plan-permitted).
eu-central returns a real result -> R = {us-east, eu-west, eu-central}, |R| = 3.
The verdict is now computed over 3 healthy reporters again; coverage-degraded clears.
```

The failed-over result counts in `R` and `U` exactly like any healthy region's result, because it IS a healthy region's result. Failover never invents a vote; it restores a real reporter. It does not change the down-policy math beyond restoring `|R|`.

### 8.4 Cost implication

Failing a check onto a more expensive region (e.g. a premium region) is a cost the platform absorbs to keep coverage during our own outage. This is one reason failover is plan-gated (section 9, PRD-007 section 8). The scheduler's failover-region preference favors lower `cost_class` to bound the absorbed cost.

---

## 9. Cost awareness

Region is both an entitlement and a real COGS dimension. PRD-006 owns pricing/metering; this RFC owns how cost is measured, attributed, and fed into scheduling.

### 9.1 Cost dimensions

| Dimension | Source | Attributed to |
|-----------|--------|---------------|
| Per-check compute | one (monitor, region) job per tick; `cost_class` of the region weights it | the org / monitor, multiplied by selected region count (fan-out, section 10) |
| Cross-region egress | mirrored `check.results` + `region.health` bytes per region (section 3.4) | the region (for margin) and, by fan-out, the org |
| Premium-region premium | `cost_class = premium` regions cost more to run and are a paid upsell | the org, via the premium entitlement (PRD-006) |
| Failover absorption | a failed-over check running on a pricier region than selected | the platform (absorbed COGS), tracked separately so it is visible as a guarantee cost |

### 9.2 Cost-aware scheduling

| Rule | Behavior |
|------|----------|
| Low-tier default region | new monitors default to the home region (cheapest, `cost_class = standard`); Free stays single-region home by entitlement (PRD-007 section 3) |
| No premium on low tiers | the scheduler does not dispatch a premium region for a plan that does not include it; the api rejects the selection on write (RFC-000 section 12, `region_not_in_plan`) |
| Failover prefers cheap | the failover-region choice (section 8.1) prefers lower `cost_class` and same geography to bound absorbed cost |
| Downgrade clamps | after a downgrade, the scheduler stops dispatching disallowed regions on the next tick even before the owner drops them (RFC-000 section 12, PRD-006) |

### 9.3 How cost is measured and attributed

| Mechanism | Detail |
|-----------|--------|
| Per-region job counters | the scheduler emits a count of jobs dispatched per (region, cost_class) and per org; these feed the margin view (RFC-002 region.health `healthy_workers` is liveness, not cost; cost counters are separate metrics, RFC-010) |
| Per-region egress | measured from the MM2 mirror byte volume per source region (RFC-011 exposes it), divided by region for margin |
| Result-based attribution | every `check_results` row carries `region`, so per-region check volume per org is recoverable from Postgres for exact attribution if the metric counters are insufficient |
| Margin visibility | per-region check cost + egress is tracked so multi-region margin stays visible (PRD-007 section 8, master 11). Fan-out multiplies volume, so cost per monitor scales with selected region count and must be measurable per region |

This RFC defines the measurement points; PRD-006 / RFC-009 own turning them into pricing and the billing meter.

---

## 10. Capacity and scale

| Quantity | Value | Source |
|----------|-------|--------|
| Sustained checks/sec, single-region baseline | ~10,000 | PRD-012 / master 12 |
| Fan-out multiplier | per-monitor region count (Free 1, Custom up to 4) | PRD-006, PRD-007 |
| `check.jobs` produced/sec, all regions | checks/sec x average regions per monitor | one job per (monitor, region) tick |
| `check.results` into central/sec | same as jobs (one result per job, after mirror) | the firehose, order 10k-60k/sec depending on average region count |
| Home-region mirror bandwidth | sum over regions of (results/sec for that region x ~few hundred bytes) + heartbeats | single-digit to low-tens MB/sec aggregate (RFC-002 section 9) |

How it scales:

| Lever | Behavior |
|-------|----------|
| Per region | `check.jobs.<region>` has 64 partitions and the worker group scales to 64 consumers per region on lag (RFC-002 section 3.3, 9.2). Each region scales independently |
| Adding a region | adds one `check.jobs.<region>` topic, one worker group, one MM2 flow. No central change beyond the new mirror flow; central load grows only by that region's result + heartbeat volume |
| Home-region mirror | the central `check.results` topic (128 partitions) aggregates all regions after mirror; it carries results + heartbeats + control traffic but NOT the job stream, so central load is bounded to results + control. Mirror bandwidth scales linearly with mirrored result volume |
| Reduce step | alerting scales to the `check.results` partition count (128) on lag while preserving per-monitor order (one monitor = one partition), meeting the 5 s result-to-decision SLO within the round window W |

The fan-out is the dominant scale fact: N selected regions means N jobs and N results per tick. Cost-aware scheduling (section 9) and the PRD-006 region counts are what keep fan-out from running away on low tiers.

---

## 11. Data residency (phased, later)

| Aspect | Position |
|--------|----------|
| v1 / GA | regions exist for detection quality, not to keep a customer's data in a jurisdiction. Results flow home to the central Postgres for aggregation (PRD-007 section 10, master 12) |
| What the topology already supports | the `region` attribution on every result and the control-plane / regional-data-plane split are the substrate residency builds on. Adding residency later is additive (which data stays in-region, which jurisdictions) rather than a re-architecture |
| What residency would add later | a regional store and a constrained mirror (some result detail stays in-region, only aggregates flow home), built on the same `region` field and the same regional-cluster seam this RFC defines. Out of scope here; its own spec at Phase 3 (master 12, 15) |

The seam that makes residency additive: results are already region-tagged and already flow through a per-region cluster before mirroring. A residency variant changes WHAT mirrors home, not the topology.

---

## 12. Failure modes

| Mode | What happens | Outcome |
|------|--------------|---------|
| One region fully down (fleet dark) | no heartbeats; staleness bound flips it to unhealthy within `T_down`; it leaves `R`. If failover is on, its checks re-target to a healthy region and `R` recovers; if off, `|R|` shrinks | coverage-degraded if `R` falls below the policy floor; NO false page. Failover restores coverage when allowed |
| Region partitioned from control plane | MM2 cannot mirror home; `region.health` goes stale; staleness detection treats it as degraded then down (section 3.5, 5.3); it leaves `R`. Late results, when the link heals, upsert by the unique key and do not flip past verdicts | treated as degraded/down -> excluded; never a false page. Mirror lag alone (under W) is invisible |
| All selected regions down | `R = 0` under every policy | coverage-degraded under any/quorum/all; NO incident, NO notification. The guarantee holds even at total coverage loss (PRD-007 section 12) |
| All selected regions report the target unhealthy | every healthy region is in `U`; every policy is unhealthy | the state machine opens an incident normally once `failure_threshold` is crossed (RFC-006, PRD-002). This is a real outage, correctly paged |
| Region retired while selected by monitors | the retired region is dropped from each monitor's effective set; if that empties the set, fall back to home and surface to the owner (section 4.4) | never an empty region set; coverage-degraded if the shrink drops `R` below the floor |
| Heartbeat flapping | heartbeats arrive intermittently around the staleness bound | the region settles into degraded and stays excluded from `R` until it is sustainedly fresh for the recovery window (section 5.3 hysteresis). It does NOT oscillate the denominator or churn verdicts |
| Healthy region reports a timeout | the worker writes a real `check.results` row (`healthy=false`, `failure_reason=timeout`); the region is in `R` with an unhealthy outcome | counted toward the policy (real signal), NOT excluded. Only heartbeat-unhealthy regions are excluded (section 5.4) |
| Slow mirror (lag under W) | results land within the round window slightly apart | invisible to correctness; the reduce tolerates it (section 3.3, 6.3) |
| Slow mirror (lag over W) | a region's result lands after its round closed | stored for history; does not flip the closed verdict; if it happens repeatedly the region is effectively not reporting and may drop from `R` -> coverage-degraded, never a false page |

Every row resolves to the same invariant: a region we cannot trust or cannot hear from contributes nothing to `R`, so the worst case is reduced coverage (coverage-degraded), never a false page.

---

## 13. Open questions and dependencies

### 13.1 Open questions

| # | Question | Owner |
|---|----------|-------|
| 1 | `region_health.status` enum width: RFC-001 has two values, this RFC needs three (`degraded`). Recommendation: widen RFC-001's CHECK to three values (section 5.2). Until then the controller collapses degraded->unhealthy when persisting | RFC-001 |
| 2 | Heartbeat thresholds (`H`=10s, `T_degraded`=30s, `T_down`=60s, recovery window 60s) and the round window `W`=5s are set here as defaults; confirm against real mirror lag and check cadence in staging | RFC-011 / RFC-010 |
| 3 | Worker floor per region (the `healthy_workers` minimum for full healthy vs degraded): set per region by capacity | RFC-011 |
| 4 | Failover region preference detail (strictly same-geography first, or cheapest-healthy first): this RFC sets "same geography, then lower cost_class"; confirm with PRD-006 cost model | RFC-004 / PRD-006 |
| 5 | Whether degraded (not just unhealthy) should trigger failover, or only unhealthy: this RFC allows the scheduler to fail over on degraded as early-warning; confirm the cost/aggressiveness trade-off | RFC-004 / PRD-007 |
| 6 | Cost-meter granularity: per-(region, cost_class) job counters vs per-org egress attribution from MM2 bytes; which is the billing source of truth | PRD-006 / RFC-009 |

### 13.2 Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-000 | the control/data-plane split, ADR-0006 messaging shape, the locked `R`-denominator rule (section 4.1), the topology |
| RFC-002 | the `region.health` / `check.results` / `check.jobs` schemas, the mirror seam it delegates here, the idempotency tokens |
| RFC-001 | the `regions` and `region_health` tables, the `(org_id, monitor_id, region, checked_at)` unique key the late-mirror upsert leans on |
| PRD-007 | the down-policy semantics over `R`, coverage-degraded, failover tiers, the false-positive guarantee |

| Depends on this RFC | For |
|---------------------|-----|
| RFC-004 (scheduler) | the failover re-target mechanism, the failover-region preference, the per-region cost counters |
| RFC-005 (worker) | the heartbeat cadence and what a heartbeat asserts (alive + Kafka reachable + recent dispatch->result) |
| RFC-006 (alerting) | the verdict-input contract: `R` definition, the single round window `W` and round key, the region-health read, the coverage-degraded computation |
| RFC-009 (entitlements) | the region entitlement and premium-region gating that cost-aware scheduling enforces |
| RFC-011 (infra) | regional cluster provisioning, MM2 topic-mapping config, the threshold/floor tuning, the egress byte metric |
```

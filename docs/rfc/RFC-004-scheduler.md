# RFC-004 - Scheduler

Status: DRAFT for review
Author: Principal Distributed Systems
Audience: scheduler service authors, worker authors (RFC-005), api authors (check-now path), entitlements authors (RFC-009), infra (RFC-011), observability (RFC-010)
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 2.2 scheduler, section 4 topology/multi-region, section 11.2 leader election via k8s Lease / ADR-0004, section 12 entitlement enforcement on dispatch)
Depends on: RFC-000 (leader election, topology, entitlement contract), RFC-002 (the `check.jobs.<region>` and `monitor.changed` contracts, idempotency), RFC-001 (monitors / entitlements / plans schema), RFC-009 (entitlement lookup library; contract is RFC-000 section 12)
Out of scope: down-policy / probe-fleet health / verdict reduction (RFC-008 and RFC-006). The scheduler fans out one job per selected region and never reduces a verdict.
Reuses: the v1 min-heap dispatch loop and per-monitor in-flight rule from the v1 single-binary architecture.
Product source: `docs/prd/PRD-002` (check-now, intervals, no pile-ups), `docs/prd/PRD-006` (interval floor + region entitlement on dispatch), `docs/prd/PRD-007` (per-region fan-out, region selection), `docs/PRD.md` section 12 (scheduling-accuracy SLO, scale).

House style: no em-dashes. Tables and diagrams over prose. Every load-bearing choice states the decision, the reasoning, and the rejected alternatives.

---

## 1. Overview, scope, and owned contracts

The scheduler decides which checks are due, fans out one check job per (monitor, selected region), enforces the org's interval floor and region entitlement on dispatch independently of api, and publishes jobs to `check.jobs.<region>` keyed by `monitor_id`. It holds an in-memory schedule derived entirely from Postgres, runs as a leader-elected singleton, and consumes `monitor.changed` to track live edits without a restart.

### 1.1 Owned contracts

| Contract | What this RFC fixes |
|----------|---------------------|
| Scheduling-accuracy SLO mechanism | how a check is dispatched within 5s of its scheduled time at p99 (PRD.md section 12) and how that lateness is measured for RFC-010 |
| Dispatch idempotency | the `job_id = <monitor_id>:<region>:<scheduled_at_unix>` stamp and the `scheduled_at` it carries, which make a redelivered job write the same `check_results` row downstream (RFC-002 section 6.2) |
| Entitlement clamp on dispatch | `effective_interval = max(stored_interval, entitlement.min_interval, hard_floor)` and the region-set filter to `entitlement.regions_allowed` capped at `regions_per_monitor_cap`, applied every tick (RFC-000 section 12, PRD-006 section 5.2) |
| No-pile-up rule in the distributed runtime | what "in-flight" means when execution is on a separate worker fleet, and why `interval >= timeout` plus idempotent results make it a non-issue (section 8) |
| Check-now serialization | how a manual check-now is dispatched without shifting the scheduled cadence and what returns 409 (PRD-002 section 7) |

### 1.2 What this RFC delegates

| Delegated | To |
|-----------|----|
| `check.jobs` / `monitor.changed` wire schemas, partition keys, the `internal/bus` API | RFC-002 |
| The entitlement lookup, its Redis cache, and invalidation | RFC-009 (contract in RFC-000 section 12) |
| Region health detection, the staleness bound for "effectively unhealthy", failover | RFC-008 |
| Verdict reduction (down_policy over healthy regions) | RFC-006 / RFC-008 |
| Leader-election runtime (the Lease object, RBAC, pod spec) | RFC-011 |
| Monitor / entitlement / region schema | RFC-001 |
| Partition counts, retention, MM2 | RFC-002 / RFC-011 |

### 1.3 Scale this RFC is designed against (PRD.md section 12, PRD-007 section 11)

| Quantity | Value |
|----------|-------|
| Active monitors | design for 500k (50k orgs, ~10 monitors avg) |
| Sustained throughput | ~10,000 checks/sec, 2x burst, expressed in region-jobs not monitors |
| Region fan-out | N selected regions means N jobs per tick; throughput target is already in region-jobs |
| Scheduling accuracy | dispatch within 5s of scheduled time, p99, under normal load |

---

## 2. Leader election and singleton

The scheduler must be exactly one active dispatcher at a time. Two active leaders would double-dispatch every tick (the `job_id` dedup downstream would absorb genuine duplicates, but it doubles broker load and risks two leaders racing on the same `scheduled_at`); zero active leaders delays every check past the 5s SLO.

**Current state:** the scheduler runs as a single instance today (`cmd/scheduler/main.go`). Leader election via the k8s Lease and client-go (the design below, ADR-0004) is planned, not built. The single-instance setup gives the singleton guarantee for now; the Lease work is what makes it safe to run more than one replica.

### 2.1 Decision: k8s Lease via client-go leaderelection (RFC-000 ADR-0004)

Leadership is a `coordination.k8s.io/Lease` object acquired through `client-go`'s `leaderelection` package. This is fixed by RFC-000 section 11.2 / ADR-0004 and is not re-litigated here. The reasoning carried from there: the singleton guarantee is tied to the same control plane that schedules the pods, not to Redis (which we treat as fail-open everywhere else). A Redis `SET NX PX` lock can split-brain or vanish on a cache blip; the Lease does not.

### 2.2 Replica roles

| Role | Behavior |
|------|----------|
| Active leader | holds the Lease, runs the dispatch loop (section 5), consumes `monitor.changed`, produces `check.jobs.<region>` |
| Standby (warm) | runs, keeps the leaderelection loop trying to acquire, does NOT build the heap, does NOT consume `monitor.changed`, does NOT dispatch. It is idle except for the election loop |

Run 2 or 3 replicas (RFC-011 sizes). Standbys are warm only in the sense that the pod, the franz-go client connections, and the Postgres pool are up; the heap is built on promotion, not pre-built, because a heap built on a standby would drift from the leader's view and would have to be discarded anyway.

### 2.3 Lease timing and failover

| Parameter | Value (guidance; RFC-011 sets final) | Why |
|-----------|--------------------------------------|-----|
| `LeaseDuration` | 15s | how long a leader's claim is valid without renewal |
| `RenewDeadline` | 10s | the leader must renew within this or it stops acting as leader |
| `RetryPeriod` | 2s | how often acquire/renew is attempted |

Failover budget: worst case a standby observes the Lease as expired after `LeaseDuration` and acquires it, then rebuilds the heap from Postgres (section 4). Heap rebuild for 500k monitors is one indexed query plus an in-memory heapify, measured in low single-digit seconds (section 3.4). So the total gap is roughly `LeaseDuration` plus rebuild, on the order of 15-20s. That gap delays some checks but does not lose any: every monitor's next run is recomputed from `interval_seconds` and the last dispatch, so a leader change re-seeds the schedule rather than skipping ticks. The 5s p99 SLO is measured "under normal load" (PRD.md section 12); a failover is an exceptional event whose recovery is bounded and whose backlog drains within seconds after promotion.

### 2.4 In-flight dispatch during a leader change (no duplicate, no drop)

The dispatch loop is cheap: pop due monitors, clamp, produce one Kafka record per (monitor, region), reschedule `nextRun += interval`. "In-flight" on the scheduler means "between popping a monitor and the franz-go produce returning." The hand-off is made safe by these rules:

| Hazard | Mechanism |
|--------|-----------|
| Old leader keeps dispatching after losing the Lease | `client-go` leaderelection calls `OnStoppedLeading`; the dispatch loop runs under a context that is cancelled there, and `RenewDeadline < LeaseDuration` guarantees the old leader stops acting before the new one can acquire. The loop checks "am I still leader" via that context before each produce batch |
| New leader re-dispatches the same tick | every job carries `job_id = <monitor_id>:<region>:<scheduled_at_unix>`. If the old leader produced tick T for a monitor and the new leader rebuilds and also produces tick T (because rebuild floored `nextRun` to the same boundary, section 4.2), the two jobs share a `job_id` and `scheduled_at`. The worker writes `check_results` keyed `(monitor_id, region, checked_at)` with `checked_at` anchored to the job, so the second is an upsert no-op and re-emits the same result (RFC-002 section 6.2, 6.3). A leader change therefore cannot double-count a check |
| A tick is dropped in the gap | rebuild seeds `nextRun` from `now` floored to the cadence boundary (section 4.2). A monitor whose tick fell entirely inside the gap simply runs at the next boundary; at most one tick is delayed, never permanently lost, and the cadence does not drift because rescheduling is boundary-anchored |

The franz-go idempotent producer also means a produce retry across a transient broker error during the hand-off does not double-append at the broker (RFC-002 section 2.2).

---

## 3. Schedule representation and the sharding decision

### 3.1 Carry forward the v1 min-heap (now across all orgs)

The proven v1 design (archive sections 3.4, 5) is a single goroutine owning a `container/heap` min-heap keyed by `nextRun`, sleeping on one `time.Timer` set to the earliest due time, waking, popping all due items, rescheduling each as `nextRun += interval` (cadence-stable, not `now + interval`), and dispatching. We carry this forward verbatim in shape. The only changes are: the heap now spans every org's monitors, the dispatch action is "produce Kafka jobs" instead of "run the check in a local pool", and each pop fans out per region with an entitlement clamp.

Heap item:

```go
type schedItem struct {
    monitorID   int64
    orgID       int64
    nextRun     time.Time   // boundary-anchored, see 4.2
    intervalSec int         // the STORED interval; the clamp is applied at dispatch, not here
    // the monitor config snapshot needed to build the job payload, kept in a side map
    // keyed by monitorID so the heap item stays small and re-prioritization is cheap
}
```

The config snapshot (url, method, headers, assertions, regions, etc.) lives in a `map[int64]*domain.Monitor` side table, not inside the heap item, so the heap stays small and `monitor.changed` can update a monitor's config without touching the heap unless its interval changed.

### 3.2 The load-bearing question: single leader heap vs sharded scheduling

This is the central scaling decision of this RFC. The two candidates:

| Option | Shape |
|--------|-------|
| A (chosen) | one leader-elected scheduler holding one heap of all 500k monitors |
| B | N scheduler shards, each leader-elected, partitioning monitors by `hash(monitor_id) % N`, each shard owning ~500k/N monitors |

### 3.3 Decision: single leader heap (Option A), with a defined trigger to shard (Option B) later

We run one leader with one heap. RFC-000 section 2.2 already states this stance ("Does not horizontally scale for throughput; it is a single active leader with warm standbys... If one scheduler ever cannot keep up, we shard by org-hash across a small fixed set of leaders before we make it stateless. Not needed at 500k monitors / 10k checks per second because publishing is light"). This RFC confirms it with the concrete numbers and fixes the shard trigger.

### 3.4 Why a single heap holds at 500k monitors

The work the leader does is the load-bearing question, so here are the actual numbers.

| Resource | Estimate at 500k monitors | Reasoning |
|----------|---------------------------|-----------|
| Heap memory | ~24 MB for heap items + ~a few hundred MB for the config side map | a `schedItem` is ~40 bytes; 500k of them is ~20 MB. The config snapshot map dominates: a `domain.Monitor` with headers/regions is ~1-2 KB, so ~0.5-1 GB. This is the real memory cost and it is comfortable for a single pod sized at a few GB (RFC-011) |
| Heap ops/sec | ~10k pop+push pairs/sec | at ~10k region-jobs/sec the leader pops and reschedules on that order. Each `heap.Pop` + `heap.Push` is O(log n); log2(500k) is ~19, so ~19 comparisons per op, ~190k comparisons/sec. Trivial for one core |
| Produce/sec | ~10k Kafka records/sec | franz-go batches produces; 10k small JSON records/sec is single-digit MB/sec, well within one producer (RFC-002 section 9) |
| Clamp/sec | ~10k entitlement lookups/sec | each is a sub-millisecond Redis-cached read through `internal/entitlements` (section 6); batched and cache-hot it is not the bottleneck |
| Rebuild on boot/failover | one `idx_monitors_enabled` scan + heapify | a single indexed query streaming 500k rows via pgx, then O(n) `heap.Init`. Low single-digit seconds |

The job is dominated by Kafka produce and the entitlement cache read, both of which are light per item. The dispatch loop is one goroutine doing cheap work; the heavy work (the HTTP checks) is on the worker fleet, which scales horizontally on Kafka lag (RFC-000 section 2.3). There is no per-org or per-monitor serial bottleneck in the leader because a pop-and-produce does not wait on anything except the (cached, batched) clamp.

### 3.5 Why we do NOT shard now (Option B rejected for v1)

| Cost of sharding now | Detail |
|----------------------|--------|
| N Leases, N rebuild paths, N failover windows | each shard is its own leader-elected singleton with its own gap on failover; operationally N times the moving parts for a problem we do not have |
| Rebalancing on N change | growing N rehashes `hash(monitor_id) % N`, moving monitors between shards mid-flight, which is the same painful repartition Kafka has and we would own it ourselves |
| `monitor.changed` routing | api would have to route each change to the owning shard (or every shard filters), adding a consumer-group-per-shard or a shared topic with per-shard filtering |
| No throughput need | the single leader is not CPU- or memory-bound at 500k (section 3.4); sharding solves a bottleneck that does not exist |

Sharding buys nothing at the target scale and adds real operational weight. It is the wrong trade now.

### 3.6 The shard trigger (record the "later, when X")

Shard to Option B when any of these holds, measured by the section 11 metrics:

| Trigger | Threshold |
|---------|-----------|
| Dispatch-loop CPU | one core sustained > ~70% on the leader (the loop is single-goroutine, so one core is the ceiling) |
| Lateness | the scheduling-lateness p99 histogram (section 11) breaches 5s under normal (non-failover) load |
| Memory | the config side map approaches the pod's memory ceiling at the next monitor-count milestone (e.g. > 2M monitors) |

When triggered, shard by `hash(monitor_id) % N` (not org-hash) so a single large org does not land entirely on one shard. `monitor_id` partitioning matches the `check.jobs` partition key (`monitor_id`, RFC-002 section 3.2), so a shard owns a clean slice with no cross-shard coordination. Each shard is an independent leader-elected heap, identical in code to the single-heap design; the only addition is a shard-index filter on the boot query (`WHERE hash(id) % N = shard_index`) and on `monitor.changed` consumption.

### 3.7 Rejected alternatives summary

| Alternative | Why rejected |
|-------------|--------------|
| One ticker goroutine per monitor (v1 Option A) | 500k parked timers is heavy and every edit must find and stop the right ticker; live edits and the no-pile-up rule are messier than a single heap with a control channel (archive section 5 already rejected this at small scale, more so here) |
| Stateless scheduler reading "due monitors" from Postgres each tick (poll the DB) | a `WHERE next_run <= now` query every second against 500k rows is a constant scan-and-update load on the primary and reintroduces the hot-path DB read RFC-000 designed out. The in-memory heap is the whole point of keeping next-run state derived-but-resident |
| Sharded from day one (Option B now) | section 3.5: operational weight for no throughput need |
| Redis-backed schedule (sorted set of next-run) | makes Redis a correctness dependency for the schedule and adds a network round trip per pop; the heap is in-process and rebuildable from Postgres, which is both faster and safer |

---

## 4. Startup rebuild

### 4.1 Load enabled monitors from Postgres

On acquiring the Lease, the leader rebuilds the heap from the source of truth:

```
on becoming leader:
  monitors := store.Monitors().ListEnabledMonitors(ctx)   // uses idx_monitors_enabled (RFC-001 4.3)
  load the org entitlement snapshot for each distinct org_id (entitlement cache warm, section 6)
  for each monitor:
     put config snapshot into the side map
     nextRun := seedNextRun(monitor, now)                 // jittered + boundary-anchored, 4.2
     heap.Push(schedItem{monitorID, orgID, nextRun, monitor.IntervalSeconds})
  heap.Init
  start the dispatch loop and the monitor.changed consumer
```

`ListEnabledMonitors` is the v1 method carried forward (RFC-001 section 5 keeps the v1 method shapes; the boot read goes to the primary per RFC-001 section "Scheduler boot rebuild -> primary" because it needs the authoritative enabled set). At 500k rows this streams via pgx in one query.

### 4.2 Seeding next-run: jitter to avoid a thundering herd, anchor to avoid drift

Two requirements pull in different directions: spread the first runs so 500k monitors do not all fire at once (thundering herd), and keep the cadence stable across restarts so a monitor does not drift or skip.

Decision: seed `nextRun` to the next cadence boundary derived from a stable per-monitor phase, not from boot time.

```
phase(monitorID, interval) := monitorID's stable offset within [0, interval),
                              e.g. (monitorID mod interval) seconds
seedNextRun(monitor, now):
   interval := monitor.IntervalSeconds        // stored; clamp is applied at dispatch
   // the next instant >= now whose (unix_seconds mod interval) == phase
   return nextBoundaryAtOrAfter(now, interval, phase(monitorID, interval))
```

Why this beats `now + rand(interval)`:

| Property | Stable-phase boundary | `now + rand(interval)` |
|----------|-----------------------|------------------------|
| Thundering herd | spread: monitors with the same interval are distributed across the interval by their id-derived phase | also spread, but by random, which changes every boot |
| Drift across restart | none: the phase is a pure function of `monitorID`, so a restart re-lands the monitor on the same boundary it would have hit, not a fresh random offset | drifts: every restart re-randomizes the offset, so frequent restarts shift a monitor's run times around |
| Double-dispatch safety on failover | the new leader computes the same boundary the old leader used, so a tick in the gap collapses on `job_id` (section 2.4) | a re-randomized offset on the new leader produces a different `scheduled_at`, so the dedup `job_id` would not match and a leader change could double-run a check |

The stable-phase boundary is what makes the failover dedup in section 2.4 actually work. The id-derived phase is the jitter (it spreads load) and the determinism (it survives restart) at the same time.

After the first run, rescheduling is the v1 rule: `nextRun += interval` (boundary-anchored), never `now + interval`, so a slow dispatch or a brief leader gap cannot drift the cadence (archive section 5).

### 4.3 Resume correctness after restart / failover

| Concern | Handling |
|---------|----------|
| Alert state | not the scheduler's; it lives in Postgres (`consecutive_fails`, `first_fail_at`) and is read by alerting (RFC-006). The scheduler holds no alert state, so a restart cannot corrupt it |
| Missed ticks during downtime | not replayed. A check that should have run during a 15s gap simply runs at its next boundary. We do not backfill, because a stale check result is not useful (RFC-002 section 3.4 keeps `check.jobs` at 1h retention for the same reason) and the durable history tolerates a gap |
| `monitor.changed` events during downtime | the topic retains 7 days (RFC-002 section 3.4) but the rebuild reads the full current state from Postgres, which is already post-every-edit, so the scheduler does not need to replay the change log to be correct. On resume it seeks the `monitor.changed` group to latest and relies on the Postgres snapshot for the baseline (section 7.3) |

---

## 5. Per-(monitor, region) fan-out and dispatch idempotency

### 5.1 The dispatch loop

```
loop:
  select:
    timer fires (earliest nextRun reached):
       due := pop all items with nextRun <= now
       for each item in due:
          m := configSnapshot[item.monitorID]
          ent := entitlements.Get(ctx, item.orgID)          // cached, section 6
          eff := clamp(m, ent)                               // section 6: interval + region set
          // reschedule on the STORED interval boundary so an upgrade restores cadence
          item.nextRun = nextBoundary(item.nextRun, m.IntervalSeconds)
          // but skip THIS dispatch if the effective (clamped) interval has not elapsed
          if not dueUnderEffectiveInterval(item, eff): heap.Push(item); continue
          for each region in eff.regions:
             job := buildJob(m, region, scheduledAt = item.firedBoundary)
             bus.Produce(ctx, bus.CheckJobs(region), job, ProduceOpts{
                Key:      bus.KeyMonitor(m.ID),
                DedupKey: job.JobID,                          // <id>:<region>:<scheduled_at_unix>
                TraceCtx: ctx,
             })
          heap.Push(item)
       reset timer to new earliest
    control message (monitor.changed handler, section 7):
       apply create/update/enable/disable/delete to heap + side map
    ctx.Done(): stop dispatching, drain, exit
```

The effective-interval handling deserves care. The heap ticks on the STORED interval so that an entitlement upgrade restores the faster cadence on the next tick without re-editing the monitor (PRD-006 section 5.2). But the clamp can make the effective interval larger than the stored one (a downgraded Free org with a stored 60s monitor must dispatch no faster than 7200s). So each item also tracks `lastDispatchedAt`, and a tick is only produced if `now - lastDispatchedAt >= eff.interval`. A tick that fires on the stored cadence but is inside the clamped window reschedules without producing. This keeps the stored cadence resident (cheap upgrade restore) while honoring the live floor.

### 5.2 The job payload (RFC-002 section 4.3)

For each due (monitor, region) the leader produces one `check.jobs.<region>` record carrying the config snapshot the worker needs so the worker never reads Postgres on the hot path:

| Field | Value | Purpose |
|-------|-------|---------|
| `job_id` | `<monitor_id>:<region>:<scheduled_at_unix>` | the idempotency anchor and dedup key |
| `monitor_id` | the id | partition key (per-monitor ordering, RFC-002 section 3.2) |
| `region` | the selected region code | equals the topic suffix |
| `scheduled_at` | the cadence boundary this tick was due | anchors `job_id`; the worker stamps result `checked_at` from the boundary so a redelivery reuses the same `checked_at` |
| `check.*` | url, method, headers (secret values decrypted here, RFC-002 section 4.3), body, expected_status_codes, timeout_seconds, max_latency_ms, body_contains | the PRD-002 section 2.2 config the worker executes |

The partition key is `monitor_id` (via `bus.KeyMonitor`), so all of a monitor's jobs across ticks land on one partition in order, which is what per-monitor result ordering downstream depends on.

### 5.3 How the job id makes downstream writes idempotent (RFC-002 section 6)

The chain:

```
scheduler stamps job_id = <monitor_id>:<region>:<scheduled_at_unix>, scheduled_at = boundary
   -> worker anchors checked_at to scheduled_at (RFC-002 6.3)
   -> worker INSERT check_results ON CONFLICT (monitor_id, region, checked_at) DO NOTHING
   -> a redelivered or leader-change-duplicated job writes the SAME row (no-op)
   -> worker re-emits check.results with the same (monitor_id, region, checked_at) dedup token
   -> alerting dedups (RFC-002 6.4)
```

The scheduler's only responsibility in this chain is to stamp a stable `job_id` and a stable `scheduled_at` (the cadence boundary, not wall-clock-at-produce). Section 4.2's stable-phase boundary is what guarantees that two independent produces of the same tick (old + new leader, or a produce retry) carry the identical `scheduled_at`, so the downstream unique key collapses them. This is the dispatch-idempotency contract this RFC owns.

---

## 6. Entitlement enforcement on dispatch (RFC-000 section 12, PRD-006 section 5.2)

The scheduler enforces entitlements independently of api, so a monitor created under a higher plan cannot keep running faster or in richer regions after a downgrade. This is the second of the two enforcement points (api on write is the first); neither trusts the other (RFC-000 section 12).

### 6.1 The clamp

```
clamp(monitor, ent) := EffectivePlan{
   interval: max(monitor.IntervalSeconds, ent.min_interval_seconds, ent.hard_floor_seconds),
   regions:  take(intersect(monitor.regions, ent.regions_allowed), ent.regions_per_monitor_cap),
}
```

| Clamp | Rule | Source |
|-------|------|--------|
| Interval floor | `max(stored, entitlement.min_interval_seconds, hard_floor 30s)` | PRD-006 section 5.2: "the scheduler computes the effective interval as `max(monitor.interval_seconds, entitlement.min_interval_seconds)`"; hard floor 30s never goes below (PRD-006 section 3) |
| Region filter | keep only regions in `entitlement.regions_allowed`, capped at `regions_per_monitor_cap`; premium regions drop when the org leaves a premium tier | PRD-006 section 5.2: "only for regions in the current `regions_allowed` set, and no more than `regions_per_monitor_cap`" |

The stored monitor row is never mutated by the clamp. The floor is applied at dispatch so an upgrade restores the faster cadence and the richer region set on the next tick without re-editing the monitor (PRD-006 section 5.2). A downgrade therefore does not need to rewrite every monitor; the scheduler clamps to the live entitlement every tick.

### 6.2 Region-set edge cases

| Case | Behavior |
|------|----------|
| Intersection is empty (downgrade dropped every selected region) | fall back to the org's home region so a monitor is never left with zero dispatch targets. PRD-007 section 2 requires this for retired regions ("must never silently leave a monitor with an empty region set; the platform falls back to the monitor's home region"); the scheduler applies the same fallback for the entitlement-driven empty case |
| A selected region is `retired` in the catalog | dropped from the effective set (PRD-007 section 2: a retired region stops executing checks). The scheduler reads region lifecycle from its `region.health` / region-catalog view; a `retired` or stale-unhealthy region is excluded from fan-out. Health-based exclusion detail and the staleness bound are RFC-008 |
| More selected regions than `regions_per_monitor_cap` | take the first `cap` by a stable order (e.g. region code sort) so the choice is deterministic across ticks and leaders |

Note the scheduler only filters which regions get a job. It does NOT decide the verdict; reducing per-region results under `down_policy` over healthy-reporting regions is alerting's job (RFC-006 / RFC-008, RFC-000 section 4.1). The scheduler fans out one job per surviving region and stops there.

### 6.3 Use the cached library; behavior when the cache is cold or down

Entitlements are read through `internal/entitlements`, which serves from Redis with Postgres as source of truth (RFC-000 section 2.6, 12). On the dispatch path:

| Cache state | Scheduler behavior | Why |
|-------------|--------------------|-----|
| Hit | use the cached entitlement | sub-millisecond, the normal path |
| Miss, Postgres reachable | the library repopulates from Postgres, scheduler uses the fresh value | RFC-009 owns the fill |
| Miss AND Redis/Postgres unavailable | hold the last known snapshot the scheduler loaded (boot rebuild loaded every org's entitlement, section 4.1; `monitor.changed` and `billing.events`-driven invalidation keep it fresh) rather than dispatching wide-open | RFC-000 section 12: "The scheduler, if it cannot read entitlements, holds the last known snapshot (it rebuilt from Postgres on boot) rather than dispatching wide-open." This is fail-safe: a cache outage cannot let a downgraded org keep running faster, because the worst case is the last-known (already-clamped) snapshot |

This is the deliberate asymmetry with api: api fails closed (rejects the write) on an indeterminate entitlement, the scheduler fails to the last known snapshot (keeps dispatching at the last-known limits). Both prevent a downgrade from being bypassed by knocking over the cache. The scheduler cannot fail closed by "stop dispatching" because that would silently stop monitoring on a cache blip, which is worse than dispatching at slightly-stale-but-already-clamped limits.

---

## 7. Live schedule changes (`monitor.changed`)

### 7.1 Consume the change stream

The leader consumes `monitor.changed` (producer api, key `org_id`, RFC-002 section 4.2) so the schedule tracks create/edit/enable/disable/delete without a restart. Each event carries the full monitor snapshot so the scheduler does not re-read Postgres.

| `change` | Heap + side-map action |
|----------|------------------------|
| `created` | put snapshot in side map; `heap.Push` with a seeded `nextRun` (section 4.2) |
| `updated` | replace snapshot in side map; if `interval_seconds` changed, re-anchor `nextRun` to the new cadence boundary; otherwise leave `nextRun` untouched (a url/header edit takes effect on the next already-scheduled tick) |
| `enabled` | treat as `created` if absent from the heap |
| `disabled` | remove from the heap and side map (no-op if absent) |
| `deleted` | remove from the heap and side map (only `monitor.id` is guaranteed on delete, RFC-002 section 4.2) |

The handler runs on the dispatch goroutine via the control-channel pattern (archive section 5): the `monitor.changed` consumer pushes a typed change onto a channel the dispatch loop selects on, so only the dispatch goroutine mutates the heap and there is no lock on the schedule. This is the v1 control-channel design carried forward to a Kafka source.

### 7.2 Ordering and consistency with the DB as source of truth

| Concern | Handling |
|---------|----------|
| Per-org ordering | `monitor.changed` is keyed by `org_id` (RFC-002 section 3.2), so a create-then-edit for one org arrives in order; the scheduler applies them in order and a stale edit cannot overwrite a newer one within the partition |
| Idempotent apply | the dedup token is `event_id`; re-applying the same snapshot is last-writer-wins per monitor and a no-op on redelivery (RFC-002 section 6.2). The handler does not need its own dedup table because applying a full snapshot is naturally idempotent |
| Postgres is the source of truth | the event is an optimization to avoid a reload, not the system of record. The boot rebuild (section 4) reads Postgres, which is already post-every-edit, so a missed or out-of-order event self-heals on the next leader rebuild. If the snapshot in an event ever looks inconsistent (e.g. references a monitor not in the side map on `updated`), the handler treats it as a `created` |
| Entitlement changes | `monitor.changed` does not carry entitlements; a plan change flows through `billing.events` -> the entitlement-invalidator (RFC-002 section 4.7), which invalidates the org's cached entitlement so the scheduler's next clamp reads the new value. The scheduler does not consume `billing.events` directly; it sees the change through the cache on the next tick |

---

## 8. No pile-ups in the distributed runtime

The v1 rule was a per-monitor in-flight lock: a scheduled tick for a monitor still running its previous check was skipped (archive section 3.4). In v1 the scheduler ran the check, so it knew "in-flight." Here execution is on a separate worker fleet and the scheduler does not wait for results, so "in-flight" has to be redefined.

### 8.1 Reasoning: why at-least-once + idempotent results makes scheduled-tick pile-up a non-issue

| Step | Fact |
|------|------|
| Validation rule | `interval_seconds >= timeout_seconds` is a hard rule enforced at api (PRD-002 section 2.3) |
| Consequence | "a check can always finish or time out before the next one for that monitor is due, so a slow endpoint cannot cause checks to overlap for the same monitor in the same region" (PRD-002 section 3.6) |
| Worker scaling | a slow endpoint occupies one worker's request slot, not the fleet; the fleet scales on Kafka lag (PRD-002 section 9, RFC-000 section 2.3) |
| Idempotent result | even if two jobs for the same tick exist (redelivery, leader-change duplicate), the `(monitor_id, region, checked_at)` unique key collapses them to one row (section 5.3) |

So for SCHEDULED ticks the scheduler does not need a per-monitor in-flight token at all. The cadence guarantees the previous tick is finished-or-timed-out before the next is due, and the idempotent write absorbs any duplicate. The scheduler does not track whether a worker is still running a check, and it should not, because that would require the scheduler to consume results just to gate dispatch, coupling it to the worker fleet it deliberately does not wait on.

### 8.2 Where a per-monitor token IS still needed: check-now vs the scheduled tick

The one place a token is required is the interaction between a manual check-now and a scheduled tick (or two concurrent check-nows). A check-now can land at any instant, including while a scheduled check for the same monitor is mid-flight, which would violate "one check per monitor at a time" (PRD-002 section 7.2). This is solved with a short Redis per-monitor lock, owned by the check-now path (section 9), not by the scheduled loop. The scheduled loop does not take the lock for every tick (that would add a Redis round trip per dispatch for a guarantee the cadence already gives); the worker takes the lock when it picks up a job (RFC-005, RFC-000 section 2.3 lists the worker writing the per-monitor "check now" lock), so a scheduled job and a check-now job cannot run concurrently for the same monitor.

### 8.3 Decision and rejected alternatives

| Option | Verdict |
|--------|---------|
| No scheduler-side in-flight tracking for scheduled ticks; rely on `interval >= timeout` + idempotent results; short Redis per-monitor lock taken by the WORKER to serialize a scheduled check against a check-now (chosen) | chosen. The cadence rule already prevents scheduled-tick overlap; the lock is only paid where it is actually needed (check-now vs scheduled, two check-nows) and is taken by the executor, not the dispatcher |
| Scheduler holds a max-in-flight guard per monitor and consumes `check.results` to release it | rejected. Couples the scheduler to the result stream it is designed not to wait on, adds a consumer and per-monitor state, and duplicates the guarantee `interval >= timeout` already gives |
| Redis per-monitor token acquired by the scheduler on every dispatch | rejected. A Redis round trip per region-job at 10k/sec for a guarantee the cadence already provides; it also makes Redis a per-tick correctness dependency, which RFC-000 avoids |

---

## 9. Check-now path

### 9.1 Decision: api enqueues directly to `check.jobs`, the scheduler is not on the request path

A manual check-now (PRD-002 section 7) is a one-off job. Routing it through the scheduler (api -> scheduler -> Kafka) would put the singleton leader on a user-facing request path and add a hop. Instead api produces the check-now job directly to `check.jobs.<region>` for the monitor's effective regions, exactly the same payload shape the scheduler produces, with a `job_id` that marks it as a manual run.

| Decision | api produces the check-now job directly to Kafka |
|----------|--------------------------------------------------|
| Reasoning | the scheduler owns the cadence, not one-off runs; keeping it off the request path means a check-now does not depend on the leader being healthy and does not contend with the dispatch loop. api already holds the entitlement library to clamp the region set on write |
| Rejected | api -> scheduler control channel: puts the leader-elected singleton on a synchronous user request path and adds a failure mode (leader down = no check-now) for no benefit |

### 9.2 Serialization and 409 semantics (PRD-002 section 7.2)

Per-monitor exclusion is a short self-expiring Redis lock, TTL bounded by `timeout_seconds` plus headroom (PRD-002 section 7.2, RFC-000 section 2.2 "per-monitor 'check now' coordination lock"):

```
on POST /api/monitors/{id}/check (api):
  ent := entitlements.Get(orgID)                 // clamp regions, fail-closed on write (RFC-000 12)
  if Redis SET pulse:checknow:<monitor_id> NX EX <timeout+headroom> == 0:   // already locked
     return 409 with the standard error envelope + the latest stored result (PRD-002 7.2)
  job_id := <monitor_id>:<region>:checknow:<now_unix>     // distinct from a scheduled job_id
  for region in clamp(monitor, ent).regions:
     bus.Produce(CheckJobs(region), job, Key=KeyMonitor(id), DedupKey=job_id)
  return 202 (accepted) or the in-flight/just-finished result per PRD-002 7.2
```

The worker, when it picks up either a scheduled job or a check-now job, takes the same per-monitor Redis lock before running and releases it after (RFC-005), so a scheduled tick and a check-now cannot run concurrently for the same monitor. The lock is self-expiring so a crashed worker cannot hold a monitor's check hostage (PRD-002 section 7.2).

### 9.3 Does not shift the scheduled cadence (PRD-002 section 7.1)

The check-now `job_id` carries a `checknow` marker and a wall-clock timestamp, NOT a cadence boundary, so it does not collide with any scheduled `job_id` and the scheduler's heap is untouched. The next scheduled check still fires at its originally planned boundary. A check-now produces a normal check result and feeds the alerting machine exactly like a scheduled check (PRD-002 section 7.1).

---

## 10. Retention and rollup triggers

The scheduler owns no data-plane maintenance. This is a clarification, not a new responsibility.

| Job | Owner |
|-----|-------|
| `check_results` partition create/drop (retention) | RFC-001 partition maintenance, run as a data-plane job (not the scheduler). v1's `DeleteResultsBefore` is removed; retention is a partition drop (RFC-001) |
| Hourly rollups (`check_rollups`) | computed in the alerting data plane (RFC-001 section 6.2 / RFC-000 section 2.4 area), not the scheduler |
| Expired-session cleanup, audit retention | api / data-plane jobs (RFC-003 / RFC-001) |

The v1 monolith folded a retention ticker into the scheduler because everything was one process (archive section 5). In the SaaS the scheduler is a focused singleton; mixing a retention ticker into the leader would make a single-instance component own a data-plane batch job, which belongs on a horizontally-scaled or cron path instead. The scheduler does one thing: decide what is due and fan it out.

---

## 11. SLO mechanism (scheduling accuracy)

Target: a check is dispatched within 5s of its scheduled time at p99 under normal load (PRD.md section 12).

### 11.1 How it is achieved

| Mechanism | Effect on lateness |
|-----------|--------------------|
| In-memory heap + single `time.Timer` to the earliest due time | the loop wakes exactly when the next monitor is due, not on a fixed poll interval, so there is no built-in poll-granularity lateness |
| Boundary-anchored rescheduling (`nextRun += interval`) | a slow produce or a brief gap does not drift the cadence, so lateness does not accumulate |
| Cheap dispatch (produce + cached clamp) | each pop-to-produce is sub-millisecond on the hot path, so even a burst of simultaneously-due monitors drains fast (heap ops are O(log n), produce is batched) |
| Stable-phase jitter (section 4.2) | spreads 500k monitors across their intervals so no single instant has a 500k-deep due batch |
| franz-go batched idempotent producer | keeps produce throughput high so the loop is not blocked on the broker |

The lateness budget is consumed by: time to drain a due batch (bounded by heap-op + produce cost times the batch size), entitlement-cache read time (sub-ms on hit), and the cross-region transport (the job is produced into the regional cluster, RFC-002 section 7; produce-side latency is what the scheduler controls, the worker pickup is a separate worker-lag SLI). Under normal load the dominant term is produce time for the due batch, which the jitter keeps small.

### 11.2 How it is measured (for RFC-010)

| Metric | Definition |
|--------|------------|
| `scheduler_dispatch_lateness_seconds` (histogram) | `dispatched_at - scheduled_at`, where `dispatched_at` is the instant the produce call returns and `scheduled_at` is the cadence boundary. This is the direct SLI for the 5s p99 target (RFC-000 section 9.1 names "scheduling lateness (dispatched_at - scheduled_at) histogram") |
| `scheduler_jobs_published_total{region}` (counter) | jobs produced per region; rate is the fan-out-multiplied check rate |
| `scheduler_leader_state` (gauge) | 1 if this replica is the active leader, else 0; sums to exactly 1 across replicas (an alert fires if it sums to 0 or > 1) |
| `scheduler_schedule_size` (gauge) | heap size; tracks the active enabled-monitor count and is the signal for the section 3.6 memory shard trigger |
| `scheduler_rebuild_duration_seconds` (histogram) | boot/failover rebuild time; the failover-gap component of section 2.3 |
| `scheduler_dispatch_loop_busy_ratio` (gauge) | fraction of wall time the single dispatch goroutine is busy; the CPU shard trigger in section 3.6 |
| `scheduler_entitlement_clamp_total{result}` (counter) | clamps that changed the interval or dropped a region, so a downgrade actually taking effect on dispatch is observable |

RFC-010 owns the dashboards, the error budget, and the alert on a sustained p99 breach.

---

## 12. Failure modes

| Failure | Effect | Handling |
|---------|--------|----------|
| Leader crash | dispatch stops | a standby acquires the Lease within `LeaseDuration` and rebuilds the heap from Postgres; gap ~15-20s, no checks lost (section 2.3). Re-dispatched ticks collapse on `job_id` (section 2.4) |
| Kafka unavailable on dispatch | produce fails | franz-go retries the idempotent produce; the dispatch loop returns the item to the heap and retries on the next tick rather than dropping it. Sustained outage means missed ticks for the duration, which surface downstream as missing results and coverage-degraded (RFC-008), never a false page. A produce that eventually succeeds after a retry does not double-append (idempotent producer) |
| Regional Kafka cluster down (one region) | jobs for that region cannot be produced | the leader still produces to healthy regions; the failed region's jobs are missed for the duration. The scheduler also reads `region.health` (RFC-002 section 4.6); a region that is `unhealthy`/`retired` or stale-beyond-the-bound is dropped from fan-out (section 6.2) so the leader stops trying to dispatch into a dead region. The missing region surfaces as coverage-degraded in alerting (RFC-008), not as a down verdict (this is exactly why RFC-000 section 4.1 makes alerting's denominator the healthy-reporting regions `R`) |
| Postgres unavailable on rebuild | leader cannot build the heap on promotion | the leader retries the boot read with backoff and does not start dispatching until it has the authoritative enabled set (it must not dispatch a partial heap). The boot read is on the primary (RFC-001); if the primary is down, dispatch is delayed until it returns. A running leader already has its heap resident and keeps dispatching from memory during a transient Postgres outage, since the schedule is derived-but-resident |
| Entitlement cache cold/down | clamp cannot read fresh entitlements | hold the last-known snapshot rather than dispatching wide-open (section 6.3, RFC-000 section 12); fail-safe, a downgrade cannot be bypassed |
| Clock skew between leader replicas | `scheduled_at` boundary computed differently across a failover | mitigated by the stable-phase boundary being a pure function of `monitorID` and `interval` (section 4.2), so two replicas compute the same boundary for the same tick regardless of small wall-clock differences; the `job_id` therefore matches and dedups. Larger skew (pods on badly-skewed nodes) is an infra concern (NTP, RFC-011); the dedup tolerates sub-interval skew because the boundary snaps to the interval grid |
| `monitor.changed` consumer lag or gap | schedule briefly stale vs Postgres | self-heals: the next leader rebuild reads Postgres (source of truth, section 7.2); within a leader's life, lag means an edit takes effect a few seconds late, which is acceptable for a config change |
| Duplicate leader (split brain) | two leaders dispatch | prevented by the k8s Lease (only one holder); if it ever happened, `job_id` dedup absorbs the duplicate jobs at the cost of doubled broker load, and the `scheduler_leader_state` sum > 1 alert fires (section 11.2) |

---

## 13. Open questions and dependencies

### 13.1 Open questions

| # | Question | Owner |
|---|----------|-------|
| 1 | Exact Lease timings (`LeaseDuration`/`RenewDeadline`/`RetryPeriod`) and the standby count. Section 2.3 gives guidance; the failover-gap budget vs SLO trade is finalized with real rebuild measurements | RFC-011 + this RFC |
| 2 | The staleness bound for treating a region as "effectively unhealthy" so the scheduler drops it from fan-out. PRD-007 does not fix a heartbeat detection-latency SLO; RFC-008 must set it (also flagged in RFC-002 open question 4) | RFC-008 |
| 3 | The check-now `job_id` format that guarantees no collision with a scheduled `job_id` while staying dedup-safe for a redelivered check-now. Section 9.2 proposes a `checknow` marker; RFC-005 (worker) and RFC-002 (the dedup token) confirm | RFC-005 / RFC-002 |
| 4 | Whether api or the scheduler is the single producer of check-now jobs is decided here as api (section 9.1); RFC-012 (API) wires the endpoint and must use the same `internal/bus` producer and clamp | RFC-012 |
| 5 | The shard trigger thresholds (section 3.6) are first estimates; the CPU-busy-ratio and lateness numbers are confirmed once the leader runs against production-scale load | RFC-010 + this RFC |
| 6 | Empty-region-set fallback to "home region": which region is a monitor's home when the org has multiple allowed regions and the original set is fully dropped. PRD-007 references a home region; the exact home-region resolution rule is RFC-008 / PRD-007 | RFC-008 |

### 13.2 Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-000 | leader-election decision (ADR-0004), topology, the two-enforcement-point entitlement contract (section 12), the fail-to-last-snapshot stance |
| RFC-001 | the `monitors` table and `idx_monitors_enabled` boot index, the `entitlements`/`plans` columns and tier matrix, `ListEnabledMonitors`, the boot-read-on-primary routing |
| RFC-002 | the `check.jobs.<region>` and `monitor.changed` schemas, partition keys, the `job_id` dedup token, the `internal/bus` producer/consumer API, the regional cluster placement of `check.jobs` |
| RFC-009 | the `internal/entitlements` cached lookup used on the clamp path and its invalidation via `billing.events` |
| RFC-008 | region health / lifecycle so the scheduler drops dead or retired regions from fan-out, and the staleness bound |

| Depends on this RFC | For |
|---------------------|-----|
| RFC-005 (worker) | the `job_id` / `scheduled_at` stamping that anchors the idempotent result write; the per-monitor lock the worker takes |
| RFC-012 (API) | the check-now direct-produce path and the shared clamp |
| RFC-010 (observability) | the scheduling-lateness SLI and the leader-state / schedule-size / shard-trigger metrics |
| RFC-011 (infra) | the Lease object, RBAC, replica count, and pod sizing for the heap + config side map |

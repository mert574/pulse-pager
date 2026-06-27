# RFC-006 - Alerting

Status: DRAFT for review
Author: Principal Distributed Systems
Audience: anyone building, operating, or reviewing the alerting service
Owns (per RFC-000 section 13): the distributed wrapper around `internal/alerting`, the redelivery-safety contract (RFC-000 section 8), notify-event emission with dedup ids, hourly rollup production.
Parent: `docs/rfc/RFC-000-architecture-overview.md` (sections 2.4 alerting, 4 topology, 5 eventing, 8 consistency/ordering/idempotency, 11.2 leader election).
Depends on: RFC-000 (ordering/idempotency), RFC-001 (incidents/alert-state/rollups schema), RFC-002 (`check.results` consume, `notify.events` produce, dedup tokens), RFC-008 (down-policy + probe-fleet `region.health`). Depended on by: RFC-007 (notifier consumes `notify.events`).
Product source: PRD-002 (state machine), PRD-003 (notify payloads + region line), PRD-007 (multi-region verdict + coverage-degraded).
Reuses unchanged: `internal/alerting` (the pure `Apply` state machine).

House style: all timestamps RFC3339 UTC on the wire. No em-dashes. Tables and diagrams over prose.

Note on a dependency typo: RFC-000 section 13's RFC-006 box lists "RFC-007 (down-policy + probe health)". That is the RFC-000 index typo flagged in this RFC's brief. Probe-fleet health and the down-policy are owned by RFC-008 (Multi-Region and Probe Fleet); RFC-007 is the notifier, which consumes what this RFC produces. This RFC treats RFC-008 as the source of `region.health` and the down-policy semantics, and RFC-007 as the downstream consumer.

---

## 1. Overview, scope, owned contracts

The alerting service is a stateless (state-in-Postgres) Kafka consumer that turns a stream of region-tagged check results into incidents and notification events. It consumes `check.results`, reduces the per-region results for a monitor's check round into one healthy/unhealthy verdict using the monitor's `down_policy` and live probe-fleet health, runs the reused per-monitor state machine, persists the incident and alert counters, and emits `notify.events` for the notifier to deliver. A leader-elected task inside the same service produces the hourly `check_rollups`.

### 1.1 What this RFC owns

| Owned contract | Where |
|----------------|-------|
| The distributed wrapper around `internal/alerting.Apply` (load state, reduce verdict, apply, persist, emit) | Sections 2, 5 |
| The multi-region verdict reduction before `Apply` (aggregation window, late/missing results, probe-health exclusion, coverage-degraded) | Section 3 |
| Per-monitor ordering via `monitor_id` partitioning | Section 4 |
| The redelivery-safety contract (idempotent transitions, the `last_applied_result_id` watermark, conditional incident open/close) | Section 5 |
| Incident lifecycle (open/recovered/disabled/manual close, duration) mapped to PRD-002 | Section 6 |
| `notify.events` emission with dedup id `hash(incident_id, event_type)` and the region line | Section 7 |
| Hourly rollup production by a leader-elected task | Section 8 |

### 1.2 What this RFC does not own

| Not owned | Owner |
|-----------|-------|
| `check.results` / `notify.events` wire schema and the dedup-token contract | RFC-002 |
| `incidents`, `monitors` (alert columns), `check_rollups`, `notify_dedup` schema and the partial unique open-incident index | RFC-001 |
| The pure state-machine logic itself (PRD 12.5 table) | `internal/alerting` (reused unchanged), PRD-002 |
| Probe-fleet health detection, the `region.health` staleness bound, failover | RFC-008 |
| Disable-while-down and edit-while-down transitions (these run in api, not here) | RFC-003/RFC-012, see section 6.4 |
| Notification delivery, channel fan-out, retry, dedup-id suppression | RFC-007 |

### 1.3 Service shape (from RFC-000 section 2.4)

| Aspect | Decision |
|--------|----------|
| Statefulness | Logically stateless. All alert state (`consecutive_fails`, `first_fail_at`, open incident, `last_applied_result_id`) is in Postgres. In-flight per-partition ordering is Kafka's. The only in-memory state is the short multi-region aggregation window (section 3), which is reconstructable and never the source of truth |
| Scaling | HPA on `check.results` consumer lag; scales by adding consumers in the `alerting` group up to the partition count (128) |
| Reads | Postgres (alert state, open incident, monitor config for `down_policy`/`failure_threshold`), Kafka (`check.results`, `region.health`), Redis (the aggregation window, current per-region health snapshot) |
| Writes | Postgres (`incidents`, monitor alert counters, `check_rollups`), Kafka (`notify.events`) |
| Singleton parts | The rollup/partition task is leader-elected (one active across all alerting pods); the consume path is not a singleton |

---

## 2. Reuse of `internal/alerting` (the heart, untouched)

The brief is binding: this RFC wraps `Apply`, it does not reimplement it. `internal/alerting.Apply(m *domain.Monitor, res *domain.CheckResult, state *domain.AlertState) Decision` carries forward verbatim (RFC-000 section 14). It is the proven PRD 12.5 table, table-tested.

### 2.1 Why its purity is the enabler

`Apply` is a pure function:

| Property | Evidence in `internal/alerting/alerting.go` | Why it matters for distribution |
|----------|---------------------------------------------|---------------------------------|
| No I/O | does no DB work, sends no notifications, starts no goroutines (package doc, lines 1-12) | the wrapper controls the transaction boundary; `Apply` cannot do a partial write |
| No clock | uses `res.CheckedAt` as "now", never the wall clock (lines 74-75, 92, 110, 136) | re-running on a redelivered result with the same `CheckedAt` yields the same `Decision`; the result is deterministic in its input, so it is replay-safe |
| Pure function of (monitor, result, state) | `Decision` is computed only from the three inputs (lines 75-148) | applying a duplicate result against already-advanced state yields `ActionNone` (the counter is already past the threshold and the incident is already open), so the duplicate is naturally a no-op at the decision layer too |

Because "now" is `res.CheckedAt` and not a service clock, two pods that both process the same redelivered result compute byte-identical decisions. That determinism is what lets the persistence layer (section 5) make the writes idempotent with simple conditional SQL rather than distributed locks.

### 2.2 What the service adds around `Apply`

`Apply` decides; the service does everything that touches the outside world. The split:

```
                 +--------------------------------------------------+
 check.results   |  alerting service (the distributed wrapper)      |
 (per region) -->|                                                  |
                 |  1. aggregate per-region results for the round   |  section 3
                 |  2. reduce to ONE verdict via down_policy +      |
                 |     region.health  -> reducedResult / degraded   |
                 |  3. load AlertState (txn)                        |  section 5
                 |  4. decision := alerting.Apply(...)   <-- PURE    |  unchanged
                 |  5. persist incident + counters + watermark      |  section 5
                 |  6. emit notify.events (dedup id)               |  section 7
                 +--------------------------------------------------+
```

`Apply` is step 4 only. Steps 1-3 and 5-6 are this RFC. The state machine never sees regions, Kafka, or Postgres; it sees one monitor, one reduced result, one state, exactly as in v1.

---

## 3. Multi-region verdict reduction BEFORE Apply (the hard part)

A monitor is checked from N selected regions. The scheduler fans out one `check.jobs` per (monitor, region) per tick (RFC-002 section 4.3, PRD-007 section 4). Each region's worker produces one `check.results`, all keyed by `monitor_id`, so they all land on the same partition and one alerting consumer sees them all. Before the state machine can run, those N region results for one logical check round must collapse to one healthy/unhealthy verdict (or coverage-degraded) per the monitor's `down_policy` (PRD-007 section 5).

This is the load-bearing distributed problem: the N results do not arrive together. They are produced in N regions, mirrored home (RFC-002 section 7), and interleaved on the partition with results from other ticks. The service must decide when it has "enough" to compute a verdict and what to do about results that arrive late or never.

### 3.1 The correlation key: `(monitor_id, scheduled_at)`

A check round is identified by `(monitor_id, scheduled_at)`. `scheduled_at` is stamped by the scheduler on every job of a tick and carried through to the result (RFC-002 section 4.3, 4.4: `job_id = <monitor_id>:<region>:<scheduled_at_unix>`, and `check.results` carries `scheduled_at`'s tick via the same `job_id`). All N region jobs for one tick share one `scheduled_at`, so all N results carry it. That is the join key for the round.

Deviation flag and dependency: RFC-002 section 4.4's `check.results` example does not list `scheduled_at` as a top-level field, but it carries `job_id`, which is `<monitor_id>:<region>:<scheduled_at_unix>`, so `scheduled_at` is recoverable by parsing `job_id`. This RFC needs `scheduled_at` as a first-class field for the round key rather than parsing it out of a string on the hot path. Ask to RFC-002: add `scheduled_at` as an explicit RFC3339 field on `check.results` (it is already on `check.jobs`). Until then the wrapper parses it from `job_id`. Either way the round key is `(monitor_id, scheduled_at)`.

### 3.2 The aggregation window (decision)

Decision: a bounded, per-round aggregation window keyed by `(monitor_id, scheduled_at)`, opened by the first result of a round, closed by whichever comes first of (a) all expected healthy regions reported, or (b) a fixed close delay after the round's `scheduled_at`. The expected set is the monitor's selected regions minus regions currently probe-unhealthy. The window buffer lives in Redis (short-lived), not in service memory, so a consumer rebalance or crash does not lose a partially-filled round.

```
on check.results for (monitor_id, scheduled_at, region, healthy, result_id, ...):
  R_expected := selectedRegions(monitor) intersect healthyRegions(now)   -- section 3.4
  add (region -> result) to Redis hash agg:{monitor_id}:{scheduled_at}
       with TTL = window_close + slack
  if collected regions  ==  R_expected      -> close the round now (complete)
  elif now >= scheduled_at + window_close    -> close the round now (timeout)
  else -> return nil (commit this result's offset; the round stays open)
  on close: reduce the buffered results -> verdict -> feed Apply (section 5)
```

Why these two close conditions:

| Close trigger | Reason |
|---------------|--------|
| All expected healthy regions reported | the common, fast path. The moment the last expected region's result is in, the verdict is final and there is no reason to wait. This is what keeps the 5s result-to-decision SLO (section 9) met on the happy path |
| `scheduled_at + window_close` reached | the safety net for a region whose result is slow or never arrives. Without it a single missing region would hold the round open forever and the monitor would never transition |

`window_close` is a fixed, monitor-independent bound (proposed default 10s after `scheduled_at`, tuned in RFC-010 against the mirror-delay and worker-latency p99). It must be larger than the worst-case (worker timeout + mirror delay) so a healthy-but-slow region is not wrongly dropped, and small enough that the round still closes inside the 5s-after-last-result decision budget for the timeout path. The 5s SLO is measured from the result event to the decision; the window-close timer runs from `scheduled_at`, so the two budgets compose (see section 9.3).

### 3.3 How late and missing results are handled

| Case | Handling |
|------|----------|
| A region's result arrives before close | buffered into the round; counts toward the verdict |
| A region's result arrives after the round already closed (late) | the round key is gone from Redis. The late result is applied as its own degenerate round: it re-keys `(monitor_id, scheduled_at)`, finds the round closed (a sentinel "closed" marker is left in Redis for a grace period past the TTL), and is dropped as a no-op for verdict purposes. Per-monitor ordering still holds because the state machine has already advanced past this tick; re-running `Apply` against the advanced state and the `last_applied_result_id` guard (section 5) makes the late result a no-op. The raw row is still in Postgres for history regardless |
| A region's result never arrives | the timeout close fires at `scheduled_at + window_close`. The verdict is computed over whatever healthy regions did report. If that set is too thin, the round is coverage-degraded (section 3.5), not down |
| A region is probe-unhealthy for the whole round | it is excluded from `R_expected` up front (section 3.4), so the round does not wait for it and it is never counted as the target being down |

The sentinel-closed marker matters: without it, a late result from a region that was slow would reopen a fresh round for an already-decided tick and could, at the timeout, produce a second verdict for the same `scheduled_at`. The watermark in section 5 is the ultimate backstop (it makes any second apply for an older result a no-op), but the closed-marker stops the wasted reduce work and keeps the metrics clean.

### 3.4 How `region.health` feeds the exclusion

The wrapper holds a current per-region health snapshot built from the `region.health` topic (RFC-002 section 4.6, compacted so a fresh consumer reads current liveness on join). A region is in the down set `D` when its latest `region.health.status` is not `healthy` (so `degraded` and `unhealthy` both exclude, PRD-007 section 5.2, 6) or when its latest heartbeat is older than the RFC-008 staleness bound (RFC-002 section 4.6: recency-based liveness). `lifecycle_state = retired` also excludes the region (PRD-007 section 2: a retired region stops executing checks).

```
selectedRegions(monitor) = monitor.regions                       -- the set S (PRD-007)
D = { r in S : health(r).status != "healthy"
              OR  health(r) is stale (RFC-008 bound)
              OR  health(r).lifecycle_state == "retired" }
R = S \ D                                                          -- healthy reporting regions
```

`R` is both the expected set the window waits for (section 3.2) and the denominator the down-policy reduces over (section 3.5). A region in `D` is never an implicit healthy vote and never counted as the target down (PRD-007 section 5.2). This is why probe-fleet health must be available to alerting before the reduce: the snapshot from the compacted `region.health` topic is read on every reduce.

### 3.5 The reduction and coverage-degraded

Let `U` = the regions in `R` whose result for the round is `unhealthy`. The verdict (PRD-007 section 5.2, PRD-002 section 4.9):

| `down_policy` | Monitor verdict = unhealthy when | Coverage-degraded (cannot decide) when |
|---------------|----------------------------------|----------------------------------------|
| `any` | `|U| >= 1` | `|R| = 0` |
| `quorum` (default) | `|U| > |R| / 2` (strict majority) | `|R| = 0`, or `|R| < 2` (the quorum minimum-regions floor, PRD-007 section 13) |
| `all` | `|U| = |R|` and `|R| >= 1` | `|R| = 0` |

Worked quorum cases (PRD-007 section 5.2): `|R|=4` needs 3 unhealthy; `|R|=3` needs 2; `|R|=2` needs 2 (a 1-of-2 tie is not a majority, stays healthy); `|R|=1` is below the floor, so coverage-degraded.

The reduce produces one of three outcomes, and only the first two reach `Apply`:

| Outcome | What is fed to `Apply` |
|---------|------------------------|
| Verdict unhealthy | a synthetic `domain.CheckResult{Healthy:false, CheckedAt: round time, FailureReason: representative reason}` |
| Verdict healthy | a synthetic `domain.CheckResult{Healthy:true, CheckedAt: round time}` |
| Coverage-degraded | `Apply` is NOT called. No counter change, no incident open, no notification. The monitor surfaces a coverage-degraded indicator (orthogonal to up/down, PRD-007 section 6). The watermark is still advanced so the round is not reprocessed |

The synthetic reduced result is what the brief calls feeding "a single healthy/unhealthy input" to the untouched `Apply` (RFC-000 section 14). Its `CheckedAt` is the round time (`scheduled_at`), so `started_at = first-fail-of-run` and the duration math stay correct and clock-free. Its `FailureReason` is the highest-priority reason among the unhealthy regions, by the PRD-002 section 3.2 order (`blocked_target -> connection_error -> timeout -> status_mismatch -> latency_exceeded -> body_assertion_failed`), so the incident `cause_reason` and the notification reason line are meaningful. The `result_id` used for the watermark is the largest `result_id` in the round (section 5.3).

Coverage-degraded is "no false page" made concrete: when our own probe coverage is too thin to tell whether the target is down, we do not open an incident or notify (PRD-007 section 6, 12). A monitor that was up and loses coverage stays up with a coverage-degraded indicator, never flips to down.

### 3.6 Single-region monitors

When `|S| = 1` the window degenerates: the one result closes the round immediately (it is the whole expected set), the verdict is just that result's `healthy`, and coverage-degraded only arises if that single region is in `D` (then `|R| = 0`). This is the common Free-tier case and it pays no aggregation cost beyond a single Redis put-and-close.

---

## 4. Per-monitor ordering

`check.results` is partitioned by `monitor_id` (RFC-002 section 3.2). The consequences chain:

```
all results for monitor M  -> one partition (key = monitor_id)
one partition              -> exactly one consumer in the alerting group at a time
one consumer per monitor   -> M's results processed in arrival order
in-order processing        -> the state machine sees a coherent run for M
```

| Why it matters | Detail |
|----------------|--------|
| The state machine is a sequence machine | `Apply` reads `state.ConsecutiveFails` and decides relative to it. If a recovery were processed before the fail that preceded it, the counter and incident would be wrong. Per-partition order forbids that reorder within a monitor |
| One-down/one-up needs a single decider | two consumers concurrently applying the same monitor's results could both try to open an incident. With one partition owner, opens are serialized; the partial unique index (section 5) is the backstop, but ordering means the race is rare, not the norm |
| Cross-monitor parallelism is unbounded | the key space is the whole monitor set, so adding consumers scales throughput without breaking any single monitor's order (section 9) |

Ordering is per partition, not global, and that is exactly right: monitors are independent, so cross-monitor ordering is meaningless and we get full parallelism across them. The aggregation window (section 3) operates within a single monitor's partition stream, so it never needs cross-partition coordination.

Rebalance safety carries from RFC-002 section 8.5: cooperative-sticky balancing moves a monitor's partition to exactly one new owner, offsets commit only after process, and the watermark (section 5) makes the brief reprocessing overlap safe. The in-flight aggregation window survives the move because it is in Redis, not pod memory.

---

## 5. Idempotent transitions (redelivery safety, the correctness core)

This is the binding contract from RFC-000 section 8. A redelivered or duplicate `check.result` must not double-open, double-close, or double-count. Delivery is at-least-once (RFC-002 section 6.1), so the wrapper must be safe to run twice on the same input.

### 5.1 The persistence sequence

One round's decision is persisted in a single Postgres transaction. The notify event is emitted after commit (so we never emit for a write that rolled back). The transaction runs under the org-scoped `withOrg` (RFC-001 section 5.3) so RLS and tenancy hold.

```
on round close for (monitor_id, scheduled_at):    -- after the reduce, section 3
  reducedResult, maxResultID := reduce(round)      -- section 3.5
  if coverage-degraded:
      advance watermark only (step 4 below), surface indicator, return  -- no Apply

  BEGIN  (txn, app.current_org set)
    1. state := load AlertState(monitor_id)
            -- consecutive_fails, first_fail_at, open incident, last_applied_result_id
            -- SELECT ... FOR UPDATE on the monitor's alert row to serialize
    1a. if maxResultID <= state.last_applied_result_id:
            ROLLBACK; return            -- already applied this round or a newer one; no-op

    2. decision := alerting.Apply(monitor, reducedResult, state)   -- PURE, no I/O, no clock

    3. apply the incident action:
       OpenIncident:
         INSERT INTO incidents (org_id, monitor_id, started_at, cause_reason, first_result_id, ...)
           -- guarded by uniq_open_incident (org_id, monitor_id) WHERE ended_at IS NULL
           ON CONFLICT (the partial unique index) DO NOTHING
           RETURNING id
         if no row returned (an open incident already existed): treat as already-open, no Notify
       CloseIncident:
         UPDATE incidents SET ended_at=:t, close_reason='recovered'
           WHERE id = state.open_incident.id AND ended_at IS NULL
         if 0 rows updated (already closed): no Notify

    4. UPDATE monitors
         SET consecutive_fails = decision.NewConsecutive,
             first_fail_at     = decision.NewFirstFailAt,
             last_applied_result_id = :maxResultID
         WHERE id = :monitor_id
           AND (last_applied_result_id IS NULL OR last_applied_result_id < :maxResultID)
  COMMIT

  5. if decision.Notify != nil AND the incident action actually happened:
        emit notify.events { dedup_key = sha256(incident_id, event_type), ... }   -- section 7
  6. return nil  -> commit the Kafka offset(s) for the round's results
```

### 5.2 Why each hazard cannot happen

| Hazard | Guard | Mechanism |
|--------|-------|-----------|
| double-open | partial unique index `uniq_open_incident (org_id, monitor_id) WHERE ended_at IS NULL` (RFC-001 section 4.3) | the second `INSERT ... ON CONFLICT DO NOTHING` returns no row; the wrapper sees "already open" and emits no second down event |
| double-close | conditional `UPDATE ... WHERE ended_at IS NULL` | the second update touches zero rows; no second recovery event |
| double-count | the watermark `last_applied_result_id` plus the `WHERE last_applied_result_id < :maxResultID` guard | re-applying the same or an older round updates zero rows; counters do not drift |
| double-notify | the re-emitted event carries the same `dedup_key = sha256(incident_id, event_type)` | the notifier suppresses the duplicate (RFC-002 section 6.5); one down, one up per incident holds |
| out-of-order within a monitor | `monitor_id` partitioning (section 4) plus the watermark | the partition forbids cross-tick reorder; the watermark drops any stale replay that slips through a rebalance overlap |
| duplicate / replayed result | `Apply` purity plus the watermark | applying a duplicate against already-advanced state yields `ActionNone`, and step 1a/4's watermark check makes it a zero-row no-op even before `Apply` runs |

### 5.3 The watermark: `last_applied_result_id`

`last_applied_result_id` is a new monotonic column on `monitors` (alongside the carried-forward `consecutive_fails` and `first_fail_at`). It records the largest `check_results.id` whose round has been applied to this monitor's alert state.

| Property | Value |
|----------|-------|
| Granularity | per monitor |
| Value written | the max `result_id` across the round's buffered results (`maxResultID`) |
| Monotonicity | `result_id` is a monotonic Postgres identity (RFC-001 section 3.1), so a later round always has a larger max id; the `<` comparison is a clean watermark |
| Guard placement | both as an early read (step 1a, skip the whole apply) and in the `UPDATE ... WHERE` (step 4, the atomic backstop inside the txn) |

Why the max id of the round, not a single result id: a round is reduced from N region results, each with its own `result_id`. Using the max makes the watermark advance past every result in the round, so no member of an already-applied round can re-trigger an apply. A late straggler from the round (section 3.3) has a `result_id <= maxResultID`, so step 1a/4 drops it.

Dependency: RFC-001 must add `monitors.last_applied_result_id BIGINT` (nullable). RFC-002 section 10.2 already lists "`last_applied_result_id` on alert state" as a Postgres constraint this RFC's idempotency leans on, and RFC-000 section 8 names the watermark; this RFC fixes that it lives on `monitors` (where `consecutive_fails`/`first_fail_at` already live, RFC-001 section 4.3) and holds the round max id.

### 5.4 Why one transaction is enough (no distributed transaction)

The incident write, the counter write, and the watermark write are all in Postgres, so one local transaction makes them atomic. The Kafka offset commit and the notify emit are outside that transaction, which is the deliberate at-least-once seam:

| Crash point | Effect | Recovery |
|-------------|--------|----------|
| Before COMMIT | the txn rolls back; nothing persisted | the result offset was not committed; Kafka redelivers; the wrapper reprocesses cleanly |
| After COMMIT, before notify emit | the incident is open/closed but no notify went out yet | redelivery re-runs: the conditional writes are no-ops (already applied), and the notify is re-emitted with the same dedup id; the notifier delivers it once (RFC-002 section 6.5). So the notification is not lost |
| After notify emit, before Kafka offset commit | everything persisted and emitted | redelivery re-runs: all writes no-op, the re-emit carries the same dedup id and is suppressed. Net: exactly-once in effect |

The notify-after-commit-with-stable-dedup-id pattern is what lets us avoid Kafka transactions (RFC-000 ADR-0009). The dedup id closes the one window (emit duplicated on redelivery) that at-least-once opens.

Note on timing: each `check.results` row is upserted into `check_results` promptly when it is consumed (keyed `(org_id, monitor_id, region, checked_at)`, RFC-005 section 5.2), so history and latency views are not delayed by the aggregation window. The verdict transition above is applied later, at round close, once the round's regions are reduced.

---

## 6. Incident lifecycle

Maps directly to PRD-002. The state machine (`Apply`) drives recovered closes and all opens; api drives disabled/manual closes (section 6.4).

### 6.1 Open

| Field | Value | Source |
|-------|-------|--------|
| `started_at` | first-fail-of-run, carried forward, NOT the threshold-crossing check | PRD-002 section 4.3; `internal/alerting` lines 108-139 compute this and return `IncidentStartedAt` |
| `cause_reason` | the reduced result's `failure_reason` (highest-priority among unhealthy regions, section 3.5) | PRD-002 section 3.2; `Decision.CauseReason` |
| `first_result_id` | the round's representative failing `result_id` (soft reference, may age out) | RFC-001 section 4.3 |
| trigger | `Decision.Action == ActionOpenIncident`, i.e. `consecutive_fails` reached `failure_threshold` and no incident open | PRD-002 section 4.2 steps 4-6 |

The carried-forward `started_at` is why the wrapper persists `first_fail_at` on every failing round (step 4 in section 5.1) even before the threshold is crossed: `Apply` reads it back on the threshold-crossing round to stamp `started_at` correctly (PRD-002 section 4.2 step 6 uses the step-4 time).

### 6.2 Close

| Close kind | `close_reason` | Recovery notify? | Who triggers | Mapping |
|------------|----------------|------------------|--------------|---------|
| Recovered | `recovered` | yes (one up) | alerting `Apply` on a healthy round while an incident is open | PRD-002 section 4.2 step 8; `internal/alerting` lines 90-95 |
| Disabled | `disabled` | no | api, on disabling a down monitor | PRD-002 section 4.5 |
| Manual | `manual` | no | api, owner/admin manual close | PRD-002 section 6.4 |

Duration is `ended_at - started_at`, computed at close and surfaced on the recovery notify as `duration_seconds` (PRD-002 section 4.3, PRD-003 section 4.2). For disabled/manual closes there is no notify, so no duration line is sent.

### 6.3 Annotations and audit

A manual close is an audited action (PRD-002 section 6.4): api writes `closed_by` on the incident and emits an `audit.events` record (RFC-002 section 4.8, `action: incident.closed_manual`). The alerting consumer does not write audit for its own recovered closes (a recovery is a system fact, not a human action), matching RFC-000 section 2.4 ("`audit.events` for manual-close interactions handled in api, not here").

### 6.4 Disable-while-down and edit-while-down (handled in api, not here)

The reused `internal/alerting` package doc (lines 9-11) is explicit: disabling a down monitor and editing a down monitor are NOT driven by a check, so they are not in `Apply`. They live in api:

| Action | api behavior | What the alerting consumer sees next |
|--------|--------------|--------------------------------------|
| Disable while down | api closes the open incident `close_reason=disabled`, resets `consecutive_fails`, sends no recovery notify (PRD-002 section 4.5) | the monitor is disabled, so the scheduler stops dispatching; no more results arrive. If a late in-flight result does arrive, the watermark and the now-closed incident make it a no-op |
| Edit while down | api does NOT auto-close the incident (PRD-002 section 4.6) | the next check round with the new config drives the transition normally through `Apply`: a healthy round closes it as a normal recovery (one up), a failing round keeps it open |

Concurrency note: a manual/disabled close in api and a recovered close in the alerting consumer could race. Both are conditioned on `ended_at IS NULL` (the same partial unique index and the `WHERE ended_at IS NULL` update), so whichever commits first wins and the other updates zero rows. If api's disabled-close wins, the alerting consumer's recovered-close is a no-op and emits no up event; correct, because the monitor was turned off, not observed to recover (PRD-002 section 4.5).

---

## 7. notify.events emission

The wrapper emits `notify.events` only when `Decision.Notify != nil` and the incident action actually happened (the conditional write returned a row). The schema is RFC-002 section 4.5.

### 7.1 The dedup id

```
dedup_key = hex(sha256( incident_id || ":" || event_type ))     -- event_type in {down, recovery}
```

This is the RFC-002 section 6.2 token and the PRD-003 section 5 identity. It is stamped both in the body (`dedup_key`) and the `pulse-dedup-key` header (RFC-002 section 2.4). Because it is a function of `(incident_id, event_type)` and an incident has exactly one down and one recovery, there are at most two distinct notify events per incident ever. A redelivery-driven re-emit reuses the same key, so the notifier suppresses it (RFC-002 section 6.5, Redis set with a Postgres `notify_dedup` backstop, RFC-001 section 4.6).

### 7.2 One-down/one-up, no re-notify in v1

| Rule | Enforcement |
|------|-------------|
| One down per incident | `Apply` returns `Notify{EventDown}` only on the open transition (`internal/alerting` line 146), which happens once per incident (the open is guarded by the unique index) |
| One up per incident | `Apply` returns `Notify{EventRecovery}` only on the recovered close (line 94), guarded by the conditional close |
| No re-notify while down | `Apply` stays quiet on further failing checks while an incident is open (lines 121-124, returns `ActionNone`); PRD-002 section 4.2 step 7 |
| Disabled/manual close sends no up | those closes happen in api and do not emit a `notify.events` recovery (section 6.4); PRD-002 section 4.5, 6.4, PRD-003 section 4.1 |

### 7.3 Region detail for the notifier

The down event carries `regions_observed_unhealthy` (RFC-002 section 4.5): the set `U` from the reduce (section 3.5), the regions in `R` that saw the round fail. The notifier renders it as an additive human-readable line (`Regions: eu-west, us-east`) in Slack/Discord/email bodies only (PRD-003 section 7). It is NOT added to the locked appendix-B generic-webhook envelope (PRD-003 section 4.3, section 7: any structured region field is a later additive phase, open decision 11.1). Single-region monitors carry no region line (PRD-003 section 7).

### 7.4 What the event carries

The wrapper builds the full event after the incident is persisted, because the incident id and timestamps are only known then (`internal/alerting` lines 48-50 deliberately keep `NotifyEvent` minimal for this reason). It carries the monitor snapshot, incident, triggering reduced-check fields, `duration_seconds` (recovery only), `channel_ids`, and `regions_observed_unhealthy` (RFC-002 section 4.5). A monitor with zero attached channels still gets the event emitted (the incident still opens/closes); the notifier simply sends nothing (PRD-002 section 4.8, RFC-002 section 4.5).

---

## 8. Rollup production

The hourly `check_rollups` (RFC-001 section 6.3) are produced by a leader-elected periodic task inside the alerting service. Alerting already reads `check.results`, holds the per-monitor view, and runs in the control plane next to Postgres (RFC-001 section 6.3), so the rollup job lives here rather than in a sixth service.

### 8.1 Scheduling and leader election

| Aspect | Decision |
|--------|----------|
| Leader election | Kubernetes Lease via `client-go` `leaderelection`, the same mechanism as the scheduler (RFC-000 section 11.2). Exactly one alerting pod runs the rollup/partition task at a time; the consume path keeps running on every pod |
| Cadence | once per hour, a few minutes after the hour closes, so late-arriving results for the just-closed hour are mostly in (RFC-001 section 6.3) |
| Why Lease not a Redis lock | the rollup task tolerates a brief lapse (a missed hour is caught up next run), but we keep one leader-election mechanism across the control plane; Redis stays for coordination that tolerates a lapse (the aggregation window), the Lease for singletons (RFC-000 section 11.2) |

### 8.2 Aggregation

For each `(org_id, monitor_id, region, bucket_hour)` with new raw rows in the just-closed hour, aggregate `check_results` into one `check_rollups` row: `checks_total`, `checks_ok`, `checks_failed`, `latency_p50/p95/max` (RFC-001 section 6.3). Region is part of the key, so rollups stay per-region, matching the per-region history the UI shows (PRD-007 section 4.2).

### 8.3 Idempotency of rollup writes

```
UPSERT INTO check_rollups (org_id, monitor_id, region, bucket_hour, ...)
  VALUES (...)
  ON CONFLICT (org_id, monitor_id, region, bucket_hour) DO UPDATE SET ...
```

The primary key is `(org_id, monitor_id, region, bucket_hour)` (RFC-001 section 6.3), so a re-run of the job for the same hour recomputes the same aggregate and the upsert overwrites with an identical value. Re-running is safe and is the recovery path if the leader changed mid-run. In the same pass the job prunes each org's raw rows older than its `entitlements.retention_days` (RFC-001 section 6.2), which is also idempotent (deleting already-deleted rows is a no-op). The partition pre-create/drop maintenance (RFC-001 section 6.4) runs on the same leader-elected task, also idempotently.

---

## 9. Scaling

### 9.1 Consumer-group parallelism

`check.results` has 128 partitions (RFC-002 section 3.3), keyed by `monitor_id`. The alerting consumer group scales from 1 up to 128 consumers; HPA drives it on `check.results` lag (RFC-000 section 11.1). Each consumer owns a slice of partitions, each partition is one consumer's, and each monitor maps to one partition, so per-monitor order is preserved at any scale (section 4).

### 9.2 How it scales with monitor count

| Driver | Effect |
|--------|--------|
| More monitors | more distinct partition keys, spread across the existing 128 partitions; no ordering change, just more keys per partition |
| Higher check rate (region fan-out) | more results per partition; HPA adds consumers up to 128; beyond that the partition count grows (RFC-002 section 3.3, a one-time reshuffle) |
| The reduce and the txn | O(regions) per round and one short Postgres txn per round; the firehose is results, but notify volume is tiny (only incident open/close), so the write amplification to `notify.events` is small |

### 9.3 The result-to-decision SLO (master 12: within 5s p99) and how it is met

The SLO is "alert state updated within 5s of result, p99" measured by the alerting verdict-latency histogram (RFC-000 section 9.4, event `checked_at`/result timestamp to decision).

| Budget contributor | How it is kept inside 5s |
|--------------------|--------------------------|
| Mirror delay (regional -> central) | intra-cloud MirrorMaker is sub-second to low-seconds; comfortably inside 5s (RFC-002 section 7.4) |
| Aggregation window, happy path | closes the instant the last expected region reports, adding no fixed wait; for the common single-region monitor it closes on the one result |
| Aggregation window, timeout path | a missing region holds the round to `scheduled_at + window_close`. The SLO is measured per result-to-decision; the timeout case is the missing-region path, which surfaces as coverage-degraded or a verdict over the regions that did report, and RFC-010 tracks the timeout-path latency separately so the happy-path p99 is the SLO number |
| Consume lag | HPA on lag keeps the queue short; sustained lag is the alarm that threatens the SLO (RFC-002 section 8.1) |
| The Postgres txn | one indexed txn (`FOR UPDATE` on one monitor row, one incident upsert, one monitor update); milliseconds |

Measurement: the verdict-latency histogram records `decision_time - result.checked_at` per round, exported to Prometheus; the p99 over a window is the SLO check (RFC-000 section 9.1, 9.4). A second histogram records window-open-to-close time so the window bound can be tuned without guessing.

### 9.4 SLIs exposed (RFC-000 section 9.1)

`check.results` lag, verdict latency (result -> decision), incidents opened/closed, redelivery/no-op count (watermark drops), coverage-degraded rounds, window timeout-closes, notify events emitted, DLQ writes.

---

## 10. Failure modes

| Mode | Behavior | Why it is safe |
|------|----------|----------------|
| Redelivery of a `check.result` | reprocessed; conditional writes and the watermark make it a no-op; any re-emitted notify is suppressed by dedup id | section 5; RFC-000 section 8 |
| Out-of-order results within a monitor | `monitor_id` partitioning forbids cross-tick reorder; the watermark drops a stale replay from a rebalance overlap | section 4, 5.3 |
| A region's results never arrive | the window timeout closes the round at `scheduled_at + window_close`; the verdict is over the regions that did report, or coverage-degraded if `R` is too thin; no false page | section 3.3, 3.5 |
| A whole region down (probe-fleet unhealthy) | excluded from `R` via `region.health`; never counted as the target down; if exclusion makes `R` too thin, coverage-degraded | section 3.4, 3.5; PRD-007 section 6 |
| Consumer crash mid-process | offset uncommitted (before COMMIT, nothing persisted; after COMMIT, writes are durable and the notify re-emits with the same dedup id); at-least-once + watermark make catch-up clean | section 5.4 |
| Consumer group fully down | `check.results` retention (24h, RFC-002 section 3.4) is the catch-up budget; on recovery the group drains the backlog, HPA scales it out; durable history is in Postgres regardless | RFC-002 section 8.4 |
| Postgres unavailable | the txn fails; the wrapper returns a non-poison error so `bus` does not commit the offset; the result redelivers and is retried when Postgres is back (transient policy, RFC-002 section 8.3). No partial state, no notify for an uncommitted write | section 5.4, RFC-002 section 8.3 |
| Redis (aggregation window) unavailable | the window buffer is unavailable. Fallback: treat each result as a single-region round of its own (reduce over the one region present), or hold and retry the result (non-poison error -> redeliver) until Redis returns. Redis is fail-soft here because it is a window cache, not the source of truth (RFC-000 treats Redis as fail-open coordination); the durable correctness is the Postgres txn + watermark, which does not depend on Redis. The risk is a transiently wrong verdict during a Redis outage; we choose hold-and-retry so we do not page on a partial round. Open question 11.2 |
| A poison result (malformed, schema-invalid) | the handler returns `bus.Poison(err)`; `bus` routes the raw record to `check.results.dlq` and commits the offset so the partition is not blocked; a DLQ write raises an RFC-010 alert | RFC-002 section 8.2 |

---

## 11. Open questions and dependencies

### 11.1 Open questions

| # | Question | Owner |
|---|----------|-------|
| 1 | The `window_close` bound (proposed 10s after `scheduled_at`): the exact value must be tuned against the measured mirror-delay + worker-latency p99 so a healthy-but-slow region is never wrongly dropped on the timeout path | RFC-010 (measure), this RFC (set) |
| 2 | Redis-unavailable verdict policy (section 10): hold-and-retry vs degrade-to-single-region. This RFC proposes hold-and-retry to avoid a false page on a partial round; confirm against the availability target | this RFC / RFC-010 |
| 3 | `scheduled_at` as an explicit field on `check.results` (section 3.1): add it rather than parsing `job_id` on the hot path | RFC-002 |
| 4 | `monitors.last_applied_result_id` column (section 5.3): add it where `consecutive_fails`/`first_fail_at` live | RFC-001 |
| 5 | The quorum minimum-regions floor (`|R| >= 2`, section 3.5) is PRD-007 section 13's recommendation; confirm it is locked, since it changes when single-region-after-exclusion goes coverage-degraded vs decides | product / RFC-008 |
| 6 | Coverage-degraded indicator surfacing: how the indicator is stored/exposed (it is orthogonal to the four statuses, PRD-007 section 6); this RFC raises it from the reduce but the storage column is not yet schema'd | RFC-001 / RFC-008 |

### 11.2 Deviations flagged

| Deviation | Note |
|-----------|------|
| RFC-000 section 13 typo | RFC-006's down-policy/probe-health dependency is RFC-008, not RFC-007 (RFC-007 is the notifier, downstream). Stated in the header |
| `check.results` lacks explicit `scheduled_at` | recovered from `job_id` until RFC-002 adds the field (open question 3) |
| `domain.CloseReason` enum | v1 has `recovered`/`disabled` only; the SaaS adds `manual` to match `incidents.close_reason IN ('recovered','disabled','manual')` (RFC-001 section 4.3). The alerting consumer only ever writes `recovered`; `disabled`/`manual` are written by api |

### 11.3 Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-000 | ordering/idempotency decisions (section 8), leader-election (section 11.2), the SLO (section 9.4) |
| RFC-001 | `incidents` + `uniq_open_incident`, `monitors` alert columns + `last_applied_result_id`, `check_rollups`, `notify_dedup` |
| RFC-002 | `check.results` consume, `notify.events` produce, dedup tokens, the idempotent-transition pseudocode (section 6.4), `region.health` schema |
| RFC-008 | down-policy semantics, probe-fleet `region.health` and the staleness bound |
| PRD-002/003/007 | the state-machine table, notify payload + region line, multi-region verdict + coverage-degraded |
| `internal/alerting` | the pure `Apply`, reused unchanged |

| Depends on this RFC | For |
|---------------------|-----|
| RFC-007 (notifier) | consumes `notify.events` with the dedup id this RFC emits |
| RFC-008 (multi-region) | the verdict reduction that consumes its `region.health` |

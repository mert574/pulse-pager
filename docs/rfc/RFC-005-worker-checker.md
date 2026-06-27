# RFC-005 - Worker / Checker

Status: DRAFT for review
Author: Principal Engineering (data plane)
Parent: `docs/rfc/RFC-000-architecture-overview.md` (sections 2.3 worker, 4 topology, 5 eventing, 10 security/SSRF, 14 reuse map; ADR-0006 messaging, ADR-0009 at-least-once)
Depends on: RFC-000 (topology, SSRF stance), RFC-002 (`check.jobs.<region>` / `check.results` / `region.health` contracts), RFC-001 (`check_results` schema, the `(org_id, monitor_id, region, checked_at)` unique key, the partition write path)
Depended on by: RFC-006 (alerting consumes `check.results`), RFC-008 (consumes `region.health`)
Product source: `docs/prd/PRD-002` (check execution, assertion priority, failure reasons, 64KB cap, timeout), PRD master section 13 (SSRF on by default), PRD-007 (regional execution, heartbeats)
Reuse: `internal/checker` (`checker.go`, `ssrf.go`, `statuscodes.go`) wrapped unchanged.

House style: all timestamps RFC3339 UTC on the wire. No em-dashes. Tables and code blocks over prose.

---

## 1. Overview and scope

The worker is the data-plane horizontal scaler. One worker fleet runs per operated region. A worker consumes `check.jobs.<this-region>`, runs the HTTP check using the proven `internal/checker` package, builds a region-tagged `domain.CheckResult`, gets the result persisted to the control-plane `check_results` table, emits `check.results` back to the control plane, and emits `region.health` heartbeats so RFC-008 can tell "region down" from "target down".

### 1.1 What this RFC owns

| Owned contract | Section |
|----------------|---------|
| Reuse of `internal/checker` (the worker wraps it, does not reimplement checks) | 2 |
| The consume loop: franz-go group, commit-after-process, in-pod concurrency vs pod count | 3 |
| Job processing: decode -> check -> measure -> build region-tagged result | 4 |
| The idempotent result write keyed by `(org_id, monitor_id, region, checked_at)`, and the persistence-path decision | 5 |
| Secret-header handling on `check.jobs` (memory-only, never logged) and the decrypt-boundary recommendation | 6 |
| Per-region heartbeats to `region.health` | 7 |
| SSRF posture in the regional context (resolution-time validation + egress NetworkPolicy) | 8 |
| Scaling and capacity (HPA on consumer lag, connection pooling, DNS caching caveat) | 9 |
| Failure modes | 10 |

### 1.2 What this RFC does not own

| Not owned | Owner |
|-----------|-------|
| The `check.jobs` / `check.results` / `region.health` wire schemas and dedup tokens | RFC-002 |
| The `check_results` DDL, partitioning, and the `InsertResult` upsert SQL | RFC-001 |
| Down-policy aggregation across regions, coverage-degraded, failover | RFC-006 / RFC-008 |
| Region-health detection thresholds (staleness bound, detection-latency SLO) | RFC-008 |
| Job creation, `job_id` / `checked_at` stamping, region fan-out, entitlement on dispatch | RFC-004 |
| NetworkPolicy / egress runtime provisioning, KMS key source | RFC-011 |

A worker makes no product decision and holds no durable state. It runs a check and reports one per-region result. Aggregating per-region results into a single monitor verdict happens in alerting (RFC-006), never here.

---

## 2. Reuse of internal/checker

### 2.1 The worker wraps the proven checker unchanged

The check itself is already built and tested in `internal/checker`. The worker calls it and does not reimplement any of it. Per RFC-000 section 14 the package is reused unchanged; the only change is that the SSRF guard moves from opt-in to always-on by config, which is a config flag, not a code change.

The entry point is:

```go
func (c *checker.Checker) Check(ctx context.Context, m *domain.Monitor) *domain.CheckResult
```

Everything below carries forward verbatim from `internal/checker` and the worker relies on it as-is:

| Behavior | Where it lives today | Carries forward |
|----------|----------------------|-----------------|
| Assertion priority (`blocked_target` -> `connection_error` -> `timeout` -> `status_mismatch` -> `latency_exceeded` -> `body_assertion_failed`) | `checker.go:176-186`, PRD-002 section 3.2 | yes, untouched |
| Per-check timeout covering dial + request + body read | `checker.go:113-119` (`context.WithTimeout` from `TimeoutSeconds`) | yes |
| Latency measured from request start | `checker.go:130,173` | yes |
| 64KB body cap, body read only when there is a body assertion | `checker.go:21-22,150-171` (`io.LimitReader` to `BodyCapBytes`) | yes |
| Status matching incl. `2xx`/explicit specs | `statuscodes.go` (`ParseStatusCodes` / `Matches`) | yes |
| Error-text truncation (no full body ever stored) | `checker.go:206-217` (`MaxErrorTextLen`, default 500) | yes |
| SSRF pre-resolve refusing loopback/link-local/private | `ssrf.go:34-45` (`resolveAndCheck`), `checker.go:92-105` | yes |
| SSRF dialer `Control` re-check of the connected IP (TOCTOU / DNS-rebind guard) | `ssrf.go:51-67` (`dialControl`), wired in `checker.go:57-59` | yes |

Note one carry-forward gap that the worker inherits and that RFC-008/the security pass should track, not fix here: `internal/checker` applies the SSRF pre-resolve and the dial `Control` guard, but it does not re-validate redirect hops. RFC-000 section 10 calls for "redirects re-validated per hop." The dialer `Control` callback does fire on every dial including a redirect's dial, so the connected-IP block still applies to a redirect target. The pre-resolve, though, only covers the first URL. This is acceptable because `Control` is the authoritative guard (it runs on the actual connection for every hop), but it is flagged in section 8 and as an open question so the security review confirms redirect handling is sufficient or adds an explicit per-hop pre-resolve.

### 2.2 SSRF goes from opt-in to always-on by config

`checker.Config.BlockPrivateNetworks` (`checker.go:29`) gates the whole SSRF block today: the pre-resolve in `Check` and the dialer `Control` are only wired when it is true (`checker.go:57-59,92`). RFC-000 section 10 and PRD master section 13 require SSRF on by default and not customer-disableable.

The worker therefore constructs its `Checker` with `BlockPrivateNetworks: true` always, sourced from a config default that has no path to false in any tenant-facing setting. There is no code change inside `internal/checker`; the worker simply never passes false.

```go
chk := checker.New(checker.Config{
    BlockPrivateNetworks: true, // always on; no tenant setting can flip this (RFC-000 s10)
    BodyCapBytes:         64 * 1024,
    MaxErrorTextLen:      500,
})
```

### 2.3 What the worker adds around the checker

The checker is pure check execution. The worker is the distributed wrapper around it:

| The worker adds | Why |
|-----------------|-----|
| franz-go consumer-group loop on `check.jobs.<region>` with commit-after-process | the at-least-once delivery spine (RFC-002 section 6) |
| Bounded in-pod worker pool feeding `Check` | concurrency without an unbounded goroutine fan-out (section 3.3) |
| Decode the job, hydrate a `domain.Monitor` from the snapshot in the job (no Postgres read) | keep the worker off the hot-path DB read (RFC-000 section 2.3) |
| Stamp `region` and `org_id` onto the result; carry `job_id` and `result_id` through | RFC-002 `check.results` schema (section 4.4 of RFC-002) |
| Get the result persisted idempotently and emit `check.results` | section 5 |
| Emit `region.health` heartbeats | section 7 |
| Metrics (lag, checks/sec, duration histogram, SSRF blocks, emit failures) and trace propagation over Kafka headers | RFC-000 section 9, RFC-002 section 2.4 |

---

## 3. Consume loop

### 3.1 Client and topic

franz-go group consumer (RFC-002 ADR-0003) joining group `worker-<region>` on the single topic `check.jobs.<region>` from the regional Kafka cluster (RFC-002 section 3.1, ADR-0006: jobs are consumed locally, never cross-region). The loop is built on the `internal/bus` `Consumer` (RFC-002 section 2.4), so the worker never touches raw franz-go types.

### 3.2 At-least-once, commit-after-process

The non-negotiable discipline, identical to every other consumer in the platform (RFC-002 section 6.1):

| Rule | Behavior |
|------|----------|
| No auto-commit | offsets commit only after the handler returns nil |
| Commit after the full unit | the unit is "check ran AND result persisted AND `check.results` emitted." Only then commit the job offset |
| Crash mid-check | offset uncommitted, Kafka redelivers to another worker, the idempotent write (section 5) makes the rerun a no-op |
| Transient downstream error (Postgres deadlock, Kafka produce blip) | return a normal error so `bus` does not commit; the message redelivers and idempotency makes it safe |
| Poison job (unparseable JSON, schema-invalid) | return `bus.Poison(err)`; `bus` routes the raw record to `check.jobs.<region>.dlq` and commits the original offset so one bad job does not loop the partition (RFC-002 section 8.2) |

The ordering guarantee that `check.jobs` is keyed by `monitor_id` (RFC-002 section 3.2) does not constrain the worker's correctness, because the worker is stateless and idempotent per result. It matters downstream (alerting needs per-monitor order). The worker preserves it for free: it does not reorder across partitions.

### 3.3 Concurrency model

Two independent dials, like v1's in-process pool plus the SaaS pod scaling:

```
                 check.jobs.<region>  (64 partitions, RFC-002 s3.3)
                          |
         +----------------+----------------+
         |                |                |
   worker pod 1     worker pod 2     worker pod N        <- k8s replicas, HPA on lag
   +-----------+                                          (between-pod scaling)
   | franz-go  |  poll batch
   |  group    |----+
   +-----------+    |  jobs channel (buffered)
                    v
        +---------------------------+
        | bounded worker pool       |  <- in-pod concurrency, fixed size
        |  goroutine 1 .. goroutine W|     (within-pod scaling)
        |   each: checker.Check(...) |
        +---------------------------+
                    |  result -> persist -> emit -> ack offset
                    v
              commit-after-process
```

| Dial | What it is | Sized by |
|------|------------|----------|
| In-pod concurrency `W` | a fixed-size pool of `W` goroutines, each pulling a job off a buffered channel, running `Check`, persisting, emitting | how many concurrent in-flight HTTP checks one pod's CPU + file-descriptor + outbound-connection limits comfortably carry. A check is almost entirely I/O wait (network), so `W` can be well above core count |
| Pod count `N` | k8s replicas behind the HPA | Kafka consumer lag on `check.jobs.<region>` (section 9) |

Why both, not one: a single huge in-pod pool on a few fat pods concentrates blast radius (one pod eviction loses many in-flight checks) and wastes the partition parallelism Kafka already gives us. A swarm of tiny pods each doing one check at a time wastes per-pod fixed cost (the franz-go client, the connection pool, the metrics endpoint). The pair gets cheap I/O concurrency inside a pod and cheap horizontal scale across pods.

### 3.4 Sizing in-pod concurrency vs pod count

The binding constraint on useful pod count is the partition count: a consumer group can have at most one consumer per partition usefully. `check.jobs.<region>` is 64 partitions (RFC-002 section 3.3), so up to 64 worker pods per region do useful work; beyond that, extra pods sit idle in the group. Total per-region check concurrency is therefore `min(active_pods, 64) * W`.

| Lever | Range | Rationale |
|-------|-------|-----------|
| `W` (in-pod) | start ~50, tune by pod CPU/FD headroom and outbound connection limits (section 9.4) | a check is I/O-bound; `W` much larger than cores is fine until connection or FD limits bite |
| `N` (pods) | 1 .. 64 per region, HPA-driven | one consumer per partition is the ceiling; size partitions (RFC-011) with headroom so the fleet can scale out |
| If a region needs more than `64 * W` | raise partition count first (RFC-011, one-time reshuffle), then pods | partition count is the hard ceiling on parallel consumers |

Concrete target: at the ~10k checks/sec single-region baseline (PRD master section 12) with `W = 50`, the per-region fleet needs roughly `10000 / (50 * checks_per_sec_per_goroutine)` pods. With a typical check completing in a few hundred ms a goroutine sustains a handful of checks/sec, so the fleet lands in the low tens of pods, comfortably under the 64-partition ceiling with headroom. RFC-011 owns the exact numbers per environment; RFC-008 owns the per-region multiplier.

---

## 4. Job processing

One job is one `(monitor, region, scheduled_at)` tick (RFC-002 section 4.3). The handler is straight-line:

```
handle(job check.jobs):
  1. decode envelope + payload (job_id, monitor_id, region, scheduled_at, check{...})
     - restore trace context from Kafka headers into ctx + logger (RFC-002 s2.4)
  2. hydrate a *domain.Monitor from job.check (URL, method, headers incl. decrypted
     secret values, body, expected_status_codes, timeout_seconds, max_latency_ms,
     body_contains). No Postgres read: the snapshot is in the job (RFC-000 s2.3).
  3. result := checker.Check(ctx, monitor)        // the proven package, SSRF always on
     - result.CheckedAt is set by Check to the actual run start (UTC)
  4. tag the result: result.OrgID = job.org_id; result.Region = job.region
  5. persist idempotently keyed by (org_id, monitor_id, region, checked_at)  -> result_id  (section 5)
  2b. read the org's failure_snapshot entitlement (RFC-009); pass it to Check as
        captureResponse so an ungated org's response is never read for a snapshot
  5b. if unhealthy AND a response came back (status_mismatch / latency_exceeded /
        body_assertion_failed) AND capture was on: upsert monitor_last_failure with
        the captured response (status, headers, body up to the 64 KB cap, truncated
        flag), overwriting the prior row for this monitor (PRD-002 3.8, RFC-001 4.3)
  6. emit check.results { result_id, job_id, monitor_id, region, checked_at, healthy,
        failure_reason, status_code, latency_ms, error_text }   (RFC-002 s4.4)
     - dedup key = (monitor_id, region, checked_at); also stamped in pulse-dedup-key header
       (the captured response is NOT on the event; it goes straight to the side table)
  7. return nil  -> commit the job offset
```

Notes that bind the decode to the schemas already fixed:

| Step | Binding detail |
|------|----------------|
| `checked_at` | the worker uses `result.CheckedAt` (the actual run start, `checker.go:131`) as the result `checked_at`, which is the dedup key component. `scheduled_at` from the job anchors the `job_id` only; two genuine ticks never collide on `checked_at` because `interval_seconds >= timeout_seconds` (PRD-002 section 3.6, RFC-002 section 6.3) |
| header hydration | `check.headers[].value` carries decrypted secret values on the job (RFC-002 section 4.3); the worker sets them on the request via the same `req.Header.Set` path `checker.go:126-128` already uses. Non-secret headers are present too |
| failure_reason | comes straight from `domain.FailureReason` set by `Check`; the six values match the `check.results` enum verbatim (RFC-002 section 4.4) |
| no body on the wire | `error_text` is the truncated transport detail only; the body is never on the `check.results` event. The body is persisted in one place only, the `monitor_last_failure` snapshot on a response-level failure (step 5b, PRD-002 3.8), capped at 64 KB |

---

## 5. Idempotent result write (the load-bearing persistence decision)

### 5.1 The two questions

1. Is the write idempotent under redelivery? Yes, by the composite unique key.
2. Does the worker write to Postgres directly AND emit `check.results`, or only emit and let a control-plane consumer persist? This is the load-bearing call and it interacts with the regional / control-plane split.

### 5.2 Idempotency (settled by RFC-001 / RFC-002)

The result row is keyed by `(org_id, monitor_id, region, checked_at)`, which is the `check_results` primary key (RFC-001 section 6.1) and the `check.results` dedup token (RFC-002 section 6.2). The write is an upsert no-op on conflict (RFC-001 section 7.4):

```sql
INSERT INTO check_results
  (org_id, monitor_id, region, checked_at, healthy, failure_reason, status_code, latency_ms, error_text)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (org_id, monitor_id, region, checked_at) DO NOTHING
RETURNING id;
-- on conflict (row already existed): re-read the existing row id to carry result_id forward
```

A redelivered job reruns `Check`, produces the same `checked_at` from the same tick (redelivery carries the same job, so the same run is repeated; in practice the rerun's `checked_at` is a fresh `time.Now()` but the dedup correctness comes from the fact that a redelivery within the same partition is the same logical result and the row already exists from the first successful handle). The conflict makes the second write a no-op, and the re-emitted `check.results` carries the same dedup token so alerting dedups it too (RFC-002 section 6.4). The unique key (not the surrogate id) is the identity that dedups, exactly as RFC-001 section 6.1 fixes.

Subtlety worth stating plainly: the redelivery hazard the unique key protects against is "the same job ran twice and the first run's row is already committed." If the first run crashed before committing the row, the redelivery is the first successful write, which is correct. If the first run committed the row but crashed before committing the Kafka offset, the redelivery reruns the check, hits the conflict, re-reads the existing `result_id`, and re-emits the same event. Either way exactly one row and one logical result survive.

### 5.3 Persistence path: decision

RFC-000 section 2.3 states the platform decision: workers write `check_results` to Postgres directly AND emit `check.results` to Kafka. The Postgres row is durable history; the event drives alerting; writing the row first and putting `result_id` in the event keeps history authoritative and makes alerting a pure reaction.

The tension this RFC must resolve: workers live in regional data planes, Postgres lives in the control plane (RFC-000 section 4.1). A regional worker writing directly to control-plane Postgres means every check, in every region, opens a cross-region database connection to the home region on the hot path. That is the thing the regional split exists to avoid.

#### Options

| Option | Where the row is written | Cross-region cost | Failure coupling |
|--------|--------------------------|-------------------|------------------|
| A. Worker writes Postgres directly + emits (literal RFC-000 2.3) | regional worker -> control-plane Postgres, synchronously, per check | a cross-region DB round trip on every check (10k/sec x regions). High egress, high latency, couples in-region checking to home-region DB health | a home-region DB blip stalls checking in every region |
| B. Worker emits `check.results` only; a control-plane consumer persists the row, then alerting reacts | worker -> regional Kafka -> mirror -> control-plane `result-persister` consumer -> Postgres | only the already-required `check.results` mirror egress (RFC-002 section 7); no new cross-region path | a home-region DB blip backs up the persister consumer (Kafka buffers 24h, RFC-002 section 3.4); checking keeps running |
| C. Worker writes a regional Postgres, results mirrored / synced home | regional DB per region | a whole stateful store per region; sync complexity | breaks "no durable state in the data plane" (RFC-000 section 1.2) |

#### Decision: Option B (emit only; a control-plane consumer persists)

The worker emits `check.results` to its regional Kafka cluster and does not write Postgres itself. A small control-plane consumer (`result-persister`, in the `pulse-control` namespace next to Postgres) consumes the mirrored `check.results`, performs the idempotent upsert from section 5.2, and that is the durable write. Alerting consumes the same `check.results` stream independently for the verdict.

This is a deliberate, flagged deviation from the literal wording of RFC-000 section 2.3 ("workers write check_results directly to Postgres AND emit"). The deviation is in *who runs the upsert*, not in *what is guaranteed*. Every property RFC-000 section 2.3 wanted is preserved:

| RFC-000 2.3 wanted | Preserved how under Option B |
|--------------------|------------------------------|
| Durable Postgres history is authoritative | the `result-persister` does the exact same `(org_id, monitor_id, region, checked_at)` upsert (RFC-001 section 7.4); the row is still the durable record |
| The event drives alerting | unchanged; alerting consumes `check.results` |
| History and alerting cannot diverge into two failure modes | they read the *same* `check.results` stream; persistence and verdict both derive from one event, so they cannot diverge based on two different writes |
| `result_id` in the event lets alerting be a pure reaction | the persister stamps `result_id` after its upsert and republishes, OR alerting tolerates `result_id` being filled by the persister; see 5.4 |

Why B over the literal A: A puts a synchronous cross-region DB write on the 10k/sec-times-regions hot path and couples in-region checking to home-region Postgres availability, which directly contradicts RFC-000 section 4.1 ("nothing in a regional data plane holds durable state" and workers consume locally so a region/home partition does not stall checking). RFC-000 section 2.3 reasoned about *divergence between the durable write and the alerting trigger*; that reasoning is fully satisfied by B because both flow from one event. RFC-000 itself already mirrors `check.results` home (section 4.2, RFC-002 section 7) for alerting to consume, so B adds no new cross-region transport: the persister consumes the stream that is already being mirrored. The cost is one extra control-plane consumer and that the `result_id` is assigned at persist time, not at worker time (handled next).

Rejected A: cross-region synchronous DB write per check is the disqualifier (egress, latency, and the home-DB-blip-stalls-all-regions coupling). Rejected C: a stateful store per region breaks the single-source-of-truth tenancy model (RFC-001 section 5) and the "data plane holds no durable state" invariant for no benefit at the control-plane-in-one-region scale.

### 5.4 The result_id wrinkle and how it is handled

Under A the worker would have the `result_id` to put in the event. Under B the id is assigned by the persister's upsert. Two clean ways, decision is the second:

| Approach | How |
|----------|-----|
| Persister republishes with id | persister upserts, then republishes an enriched `check.results` (or a `check.results.persisted`) carrying `result_id`; alerting consumes the enriched one. Adds a topic hop |
| Alerting does not need the worker-assigned id (chosen) | alerting's idempotency guards on `(monitor_id, region, checked_at)` and on `last_applied_result_id` (RFC-002 section 6.4). The `result_id` it needs for the counter guard is the row id, which it can read in its own transaction when it reads alert state, or the persister and alerting share the upsert. Simplest: the persister IS part of the alerting consumer path |

Decision: fold the persist into the alerting consumer's transaction. Alerting already opens a transaction per `check.results` to read alert state and apply the verdict (RFC-002 section 6.4). The same transaction does the `check_results` upsert first (getting `result_id` locally), then applies the verdict guarded by that `result_id`. This collapses Option B's "extra consumer" into work alerting already does, keeps the durable write and the verdict in one transaction (so they truly cannot diverge), and removes the republish hop.

The worker's job ends at "emit `check.results`." It does not touch Postgres. RFC-006 owns the alerting transaction that now also performs the idempotent result upsert; this RFC fixes only that the worker emits and does not persist, and that the durable write is the `(org_id, monitor_id, region, checked_at)` upsert from RFC-001 section 7.4 wherever it runs.

Open item flagged to RFC-006 / RFC-000: this refines RFC-000 section 2.3's "worker writes Postgres directly." If RFC-000 wants to keep the worker as the literal writer, the only correct way to do that without a cross-region hot-path write is a regional DB (Option C), which the topology rejects. So the refinement is necessary, not optional, given the topology. RFC-006 confirms the fold-into-alerting-transaction shape; RFC-000 section 2.3 should be amended to "the result is written to Postgres by a control-plane consumer of `check.results` (folded into alerting), not by the regional worker."

---

## 6. Secret headers

### 6.1 What arrives on the job

`check.jobs` carries `check.headers[].value` with decrypted secret header values present (RFC-002 section 4.3). This is the one place a customer secret rides the bus, and RFC-002 explicitly defers the final encryption decision to this RFC's security pass (RFC-002 section 10.1, open question 1).

### 6.2 Handling rules (binding on the worker)

| Rule | Implementation |
|------|----------------|
| Never logged | secret header values are never written to any log line, error, metric label, span attribute, or DLQ record. The worker logs `job_id`, `monitor_id`, `region`, never header values. The checker already truncates `error_text` and never includes the body (`checker.go:206-217`); the worker adds: no header value ever reaches a log sink |
| Memory only | the decrypted value lives only in the in-process `domain.Monitor.Headers` for the life of one check, then is dropped when the handler returns. Never written to disk, never cached, never put in a result row or event |
| Not echoed back | `check.results` carries no header values; `error_text` is transport detail only |
| TLS in transit | the regional `check.jobs` topic is TLS (RFC-000 section 10), short retention (1h, RFC-002 section 3.4), and on the regional cluster only |

### 6.3 The decrypt-boundary question and recommendation

Who turns the encrypted DB value into the plaintext the worker sends? Two boundaries:

| Option | Who decrypts | Secret in plaintext where | Key reach |
|--------|--------------|---------------------------|-----------|
| Scheduler decrypts (current RFC-002 posture) | scheduler reads encrypted `monitor_headers` from Postgres, decrypts via `internal/crypto`, puts plaintext on `check.jobs` over TLS | in the scheduler's memory, on the `check.jobs` topic (TLS, 1h), in the worker's memory | the AES-256-GCM key (RFC-000 section 10) lives only in the control plane (scheduler), never in a data-plane region |
| Worker decrypts | scheduler puts the encrypted blob on `check.jobs`; the worker decrypts with a key it holds | only in the worker's memory and in transit as ciphertext | the decryption key must be present in every regional data plane |

Trade-off:

- Scheduler-decrypts keeps the crypto key in the control plane only. The exposure is the plaintext sitting on the regional `check.jobs` topic (bounded: TLS, 1h retention, regional cluster) and in worker memory. The blast radius of a compromised region is the secrets currently in-flight in that region's job topic, not the key.
- Worker-decrypts keeps ciphertext on the bus, but it requires the crypto key in every region. A compromised region then holds the key and can decrypt any job it sees. The key in a data plane is a larger, longer-lived exposure than transient plaintext on a short-retention topic.

Recommendation for the security review: scheduler decrypts; plaintext rides `check.jobs` over TLS with 1h retention; the crypto key never leaves the control plane. The exposure window is bounded and short, and the key (the long-lived, high-value secret) stays out of the data plane entirely. Worker-decrypt trades a bounded transient-plaintext exposure for an unbounded key-in-every-region exposure, which is the worse posture.

If the security review rejects plaintext-on-the-bus outright, the middle path RFC-002 section 4.3 names is an encrypted blob plus a per-region unwrap key (envelope encryption): the scheduler encrypts the job's secrets to a per-region key, the worker unwraps with that region's key only. This keeps ciphertext on the bus and limits a region's key to that region's jobs. It is more moving parts (per-region key distribution and rotation) and is the fallback, not the default. The `check.headers[].value` schema field stays the same either way (RFC-002 section 4.3); only what fills it (plaintext vs sealed blob) changes.

Decision recorded for RFC-005: scheduler-decrypts, plaintext over TLS on the regional `check.jobs` topic, key stays in the control plane. Flagged for the dedicated security pass to ratify or escalate to envelope encryption.

---

## 7. Heartbeats / region health

### 7.1 Purpose

RFC-008 must tell "the region's workers are down" (coverage-degraded, do not page) from "the target is down" (real incident). Missing results alone are ambiguous: a region with no live workers also produces no results. Heartbeats disambiguate by asserting "workers in this region are alive and consuming," so the absence of heartbeats, not the absence of results, is what RFC-008 reads as region-down (RFC-000 section 4.1, PRD-007).

### 7.2 What a heartbeat asserts and its shape

Workers (and the region controller) produce `region.health`, keyed by `region`, compacted so the latest per region is always readable (RFC-002 section 4.6). A worker heartbeat asserts: this region has live workers consuming `check.jobs.<region>` right now.

| Field (RFC-002 section 4.6) | Worker fills with |
|-----------------------------|-------------------|
| `region` | this worker's region (partition + compaction key) |
| `status` | `healthy` while the worker is consuming and able to emit; `degraded` if it is up but, e.g., its result emit is failing |
| `healthy_workers` | the count of live workers backing the region (see 7.4) |
| `reason` | null when healthy; short text otherwise |
| `lifecycle_state` | the region catalog state (`available`/`deprecated`/`retired`); `retired` means stop dispatching |

The worker fixes only the wire values it sources; RFC-008 owns how those values are aggregated into a region verdict and the staleness bound that turns "no recent heartbeat" into "effectively unhealthy" (RFC-002 section 4.6, section 10.1 open question 4).

### 7.3 Cadence

| Aspect | Value | Rationale |
|--------|-------|-----------|
| Heartbeat interval | every ~10s per worker (RFC-008 sets the final number against its detection-latency target) | frequent enough that a short staleness bound (e.g. 30s, "no_heartbeat_30s") catches a dead region within the result-to-decision SLO budget, infrequent enough to be negligible volume |
| Liveness model | recency-based: a consumer treats a region as effectively unhealthy if the latest `region.health` is older than the staleness bound (RFC-002 section 4.6) | a fully-down region simply stops producing heartbeats; no special "I am dying" message is needed |

### 7.4 How a fully-down region produces no heartbeats

The key property: a heartbeat is produced *by the workers themselves*, so it is alive-only by construction. If every worker in a region is gone (pods evicted, region partitioned, cluster down), there is nothing left to produce a heartbeat. The latest `region.health` for that region goes stale, crosses the staleness bound, and RFC-008 reads the region as unhealthy and excludes it from the down-policy denominator `R` (RFC-000 section 4.1), so the monitor goes coverage-degraded instead of paging. This is exactly why heartbeats come from the data plane, not from a control-plane prober: a control-plane heartbeat could not distinguish "region down" from "results delayed."

To avoid every worker independently spamming `region.health`, the worker fleet elects (or the region controller aggregates) a single heartbeat emitter per region that reports `healthy_workers` from the group membership, with each worker still able to emit a `degraded`/`unhealthy` self-report if its own emit path breaks. RFC-008 owns the exact aggregation; this RFC fixes that the signal originates in the data plane and that absence-of-heartbeat is the region-down indicator.

---

## 8. SSRF in the regional context

SSRF defense is two layers: the in-process guard from `internal/checker` (application layer) and the pod egress controls (network layer). Neither alone is enough; together a bypass of one is caught by the other. This ties to RFC-011 (which provisions the NetworkPolicy and the pod runtime).

### 8.1 Layer 1: in-process resolution-time validation (reused, always on)

Carried forward from `internal/checker` unchanged, always on (section 2.2):

| Guard | Code | What it stops |
|-------|------|---------------|
| Pre-resolve before any byte is sent | `ssrf.go:34-45` `resolveAndCheck`, called in `checker.go:92-105` | a target whose name resolves to loopback / link-local / RFC1918 / IPv6 ULA returns `blocked_target` with no connection made |
| Dialer `Control` re-check of the connected IP | `ssrf.go:51-67` `dialControl`, wired at `checker.go:57-59` | DNS rebinding / TOCTOU: even if DNS changed between pre-resolve and dial, the actual connected IP is re-checked and a blocked IP is refused at dial time |
| Per-hop coverage | `Control` fires on every dial, including a redirect's dial | a redirect to an internal IP is refused at the redirect's dial |

Redirect re-validation caveat (flagged in section 2.1): the pre-resolve covers the first URL only; redirect hops are guarded by `Control` (which does fire per hop) but not by a fresh pre-resolve. `Control` is the authoritative guard because it runs on the real connection for every hop, so an internal redirect target is still refused at dial. The open question is whether the security review wants an explicit per-hop pre-resolve in addition; it is belt-and-suspenders, not a correctness gap, because `Control` already blocks the dial.

### 8.2 Layer 2: egress network controls on worker pods (RFC-011)

Even if the in-process guard were bypassed, the worker pod must not be able to reach anything internal. RFC-011 provisions, this RFC requires:

| Control | Requirement |
|---------|-------------|
| NetworkPolicy egress | worker pods may egress only to: the regional Kafka cluster, the public internet for checks, DNS, and the metrics/observability sinks. No egress to the cluster's internal service ranges, no egress to Postgres/Redis (the worker does not touch them under the section 5 decision) |
| No cloud metadata | egress to the cloud metadata endpoint (`169.254.169.254` and the IPv6 equivalent) is blocked at the network layer, independent of the in-process link-local block, so a guard bypass still cannot reach instance credentials |
| No RFC1918 / internal ranges | egress to private ranges is blocked at the NetworkPolicy / egress-firewall layer, duplicating the in-process block at the network boundary |
| Least privilege | the worker pod runs as non-root, read-only root filesystem, no added capabilities, minimal service-account permissions (it needs none beyond running; it reads no k8s API on the hot path). It holds no Postgres or Redis credentials under the section 5 decision, shrinking what a compromised pod can reach |
| TLS | the regional Kafka connection is TLS (RFC-000 section 10) |

The pairing is the point: layer 1 returns a clean `blocked_target` result for the customer's misconfiguration, and layer 2 is the hard backstop so a bug in layer 1 still cannot turn a worker into an SSRF pivot into internal infrastructure. RFC-011 owns the runtime; the security pass verifies both layers are present before GA.

---

## 9. Scaling and capacity

### 9.1 HPA signal

| Signal | Use |
|--------|-----|
| Kafka consumer lag on `check.jobs.<region>` (primary) | lag directly tracks "checks waiting to run." Rising lag means due checks are queuing; the HPA adds worker pods up to the partition ceiling (RFC-000 section 2.3, 11.1; RFC-002 section 8.1) |
| CPU (secondary) | guards against a pod being CPU-bound (e.g. TLS handshakes) before lag would show it |

Lag is the right primary signal precisely because the worker's job is to drain a queue; lag is the queue depth.

### 9.2 How the fleet scales with the workload

Total check load is `monitors x regions_per_monitor x (1 / interval)`. The per-region job rate is the share of that load tagged for the region.

| Driver | Effect |
|--------|--------|
| More monitors | more jobs/sec across regions; lag rises; HPA adds pods per region |
| More regions per monitor | the fan-out multiplier (RFC-002 section 9.1); each added region is an independent `check.jobs.<region>` topic and `worker-<region>` group, scaling independently |
| Shorter interval | more jobs/sec for the same monitor count; same lag-driven scale-out |
| New region | adds a topic + a worker group + a mirror flow, no change to existing regions (RFC-002 section 9.3) |

### 9.3 The throughput target

PRD master section 12 sets ~10k checks/sec sustained as the single-region baseline; multiplied by the per-monitor region count (Free 1 up to Business 6, PRD-006 / PRD-007) the platform-wide check execution rate is order 10k-60k/sec (RFC-002 section 9.1). Per region, the worker fleet must drain its region's share. With 64 partitions per region (RFC-002 section 3.3) and in-pod concurrency `W` (section 3.4), one region sustains `min(pods, 64) * W` concurrent checks; sized so a region's steady job rate is comfortably below its drain rate, with the HPA absorbing bursts.

### 9.4 Outbound connection pooling and limits

The reused checker already pools outbound connections sanely; the worker tunes the limits for fleet density:

| Knob | Source | Note |
|------|--------|------|
| Idle connection pool | `http.Transport` in `checker.New` (`checker.go:61-69`): `MaxIdleConns: 100`, `IdleConnTimeout: 90s`, HTTP/2 attempted | reuses connections to repeat targets, cutting handshake cost. The worker may raise `MaxIdleConnsPerHost` if many checks hit the same host |
| Per-check deadline, not a global client timeout | `checker.go:75` (no global `http.Client.Timeout`); the per-check context is the deadline (`checker.go:113-119`) | a slow target only stalls its own check, never the client; this is what lets the timeout cut hung targets cleanly (section 10) |
| In-pod concurrency `W` vs FDs | section 3.3 | `W` in-flight checks means up to `W` open sockets plus pooled idles; size `W` under the pod's file-descriptor ulimit with headroom |
| Dialer timeout | `checker.go:53-56` (`Timeout: 30s`) | the dial itself is bounded independent of the per-check deadline |

### 9.5 DNS caching with the SSRF re-validation caveat

DNS caching cuts per-check resolution cost at high check rates, but it interacts with the SSRF guard:

| Concern | Handling |
|---------|----------|
| Caching resolution helps throughput | a per-pod resolver cache (or the platform resolver's cache) avoids re-resolving the same host on every check of the same monitor |
| Caching must not weaken the rebind guard | the dialer `Control` re-check (`ssrf.go:51-67`) runs on the *actual connected IP* at dial time regardless of any DNS cache, so even a stale-cached good answer is re-validated at the moment of connection. The cache can speed resolution; it cannot let a rebind through, because `Control` is the last word on the connected IP |
| Pre-resolve vs cache | the pre-resolve (`resolveAndCheck`) may read a cached answer; that only affects the early `blocked_target` short-circuit. The authoritative block is still `Control` on the real dial |

So DNS caching is safe to use for throughput precisely because the connected-IP `Control` check does not trust the cache. This is a property the reused checker already has; the worker just must not disable `Control` (it never does, section 2.2).

---

## 10. Failure modes

| Failure | What happens | Why it is safe |
|---------|--------------|----------------|
| Slow / hung target | the per-check context deadline (`checker.go:113-119`) cuts the check at `timeout_seconds`; result is `timeout` (or `connection_error`) | one slow target stalls only its own goroutine for at most the timeout; the pool keeps serving other jobs; no global client timeout to trip everything |
| Flood of due checks (e.g. many monitors due at once) | lag on `check.jobs.<region>` rises; HPA adds pods; in-pod pool absorbs short bursts | lag-driven scale-out (section 9.1); jobs wait in Kafka (1h retention) until drained; a stale job past usefulness is dropped by retention, surfacing as a missing result -> coverage-degraded, never a false page (RFC-002 section 8.4) |
| Control-plane Postgres unavailable | under the section 5 decision the worker does not touch Postgres, so checking is unaffected; the persist (folded into alerting in the control plane) backs up on Kafka (`check.results` 24h retention) and catches up on recovery | the data plane keeps checking and emitting; the durable write is delayed, not lost; this is exactly why Option B was chosen over a synchronous cross-region write |
| Kafka unavailable (regional cluster) | the worker cannot consume jobs or emit results; it stops, the HPA/lag signals it, checks pause for that region | a regional Kafka outage is a region-scoped event; results stop flowing, heartbeats stop, RFC-008 reads the region as unhealthy (coverage-degraded), no false page. On recovery the group resumes from the last committed offset |
| Kafka emit (`check.results`) fails after the check ran | the handler returns a non-nil (non-poison) error so the job offset is NOT committed; Kafka redelivers; the rerun is idempotent (section 5.2) | at-least-once + idempotent write means a failed emit just retries the whole unit; no partial result is committed because the offset only commits after a successful emit |
| Partial result (timeout during body read) | the checker returns `timeout` (or `connection_error`) with the status code kept for context (`checker.go:155-166`); it is a normal, complete result, not a partial one | the checker already turns a mid-body failure into a definite failure result with a reason; the worker emits it like any other result |
| Pod eviction mid-check | the in-flight check's job offset is uncommitted; on rebalance Kafka redelivers the job to another pod | commit-after-process (section 3.2) means an evicted pod's in-flight jobs redeliver; the idempotent write makes the rerun a no-op if the first run had somehow committed the row; cooperative-sticky rebalancing (RFC-002 section 8.5) keeps the disruption to the evicted pod's partitions only |
| Poison job (unparseable / schema-invalid) | `bus.Poison(err)` routes the raw record to `check.jobs.<region>.dlq` and commits the original offset | one bad job cannot loop a partition (RFC-002 section 8.2); the DLQ write raises an RFC-010 alert |

---

## 11. Open questions and dependencies

### 11.1 Open questions

| # | Question | Owner |
|---|----------|-------|
| 1 | Secret-header decrypt boundary: ratify scheduler-decrypts + plaintext-over-TLS-on-`check.jobs` (section 6 recommendation), or escalate to envelope encryption (per-region unwrap key)? | security pass + RFC-005 |
| 2 | Persistence-path amendment: RFC-000 section 2.3 says "worker writes Postgres directly." Section 5 here refines that to "a control-plane consumer (folded into alerting) does the upsert; the worker only emits," because a cross-region synchronous write contradicts the topology. Confirm the amendment and the fold-into-alerting-transaction shape | RFC-000 + RFC-006 |
| 3 | Redirect re-validation: `Control` guards every hop's dial, but pre-resolve covers only the first URL (section 2.1, 8.1). Does the security review want an explicit per-hop pre-resolve too, or is the `Control` dial guard sufficient? | security pass + RFC-008 |
| 4 | Heartbeat cadence and the staleness bound that turns "no heartbeat" into "effectively unhealthy" (section 7.3) are set by RFC-008 against its detection-latency target | RFC-008 |
| 5 | In-pod concurrency `W` default and the per-region partition count beyond 64 if a region exceeds `64 * W` (section 3.4, 9.3) | RFC-011 + RFC-008 |

### 11.2 Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-000 | topology (control/data-plane split), SSRF on-by-default, the result-persistence decision this RFC refines |
| RFC-002 | `check.jobs.<region>` consume, `check.results` produce, `region.health` produce, the `(monitor_id, region, checked_at)` dedup token, the `internal/bus` API, the mirror seam |
| RFC-001 | the `check_results` schema, the `(org_id, monitor_id, region, checked_at)` unique key, the `InsertResult` upsert SQL, partitioning |
| PRD-002 | assertion priority, failure reasons, 64KB cap, timeout semantics (all already in `internal/checker`) |
| PRD-007 | regional execution and heartbeat / region.health requirements |
| `internal/checker` | the proven `Check` that the worker wraps unchanged |

| Depends on this RFC | For |
|---------------------|-----|
| RFC-006 (alerting) | consumes `check.results`; under section 5 also performs the idempotent result upsert in its transaction |
| RFC-008 (multi-region) | consumes `region.health` heartbeats; owns the staleness bound and per-region aggregation |
| RFC-011 (infra) | provisions the worker NetworkPolicy/egress controls, pod least-privilege, partition counts, HPA wiring |

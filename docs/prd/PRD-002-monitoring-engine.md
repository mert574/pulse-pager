# PRD-002 - Monitoring Engine

Status: Draft for architecture
Owner: Product (Principal PM)
Parent: `docs/PRD.md` (master PRD v2.1)
Audience: Distributed-systems architecture team
Related sub-PRDs: PRD-003 Notifications, PRD-006 Billing, PRD-007 Multi-Region

Note on reuse: the monitoring mechanics here are lifted forward from the proven v1 PRD and restated by the master in section 6. Where this document says "reused from v1" or cites a master section, the behavior is identical and must not be changed. This sub-PRD goes deeper than the master on execution, the state machine, incidents, check-now, history, and acceptance, but it adds no new behavior that contradicts a locked decision. Master section references are written as "master 6.x"; appendix references are "master appendix A".

---

## 1. Overview, goals, non-goals

### 1.1 What the monitoring engine is

The monitoring engine is the core product domain of Pulse (master section 6). It owns the loop that the whole product exists for: a monitor is registered, checked on schedule from a worker fleet across one or more regions, judged healthy or not, run through a per-monitor state machine that opens and closes incidents, and handed to the notifier when state changes (master section 6, 6.6). This document specifies that loop end to end.

The engine spans four of the five services described in master 6.6:

| Service | Role in the engine | Owns |
|---------|--------------------|------|
| scheduler | decides which checks are due, enqueues one job per selected region | schedule, cadence, dispatch entitlement checks |
| worker fleet | executes the HTTP request, applies SSRF policy, writes the result | check execution, latency measurement, failure_reason |
| alerting | reduces per-region results to one verdict, runs the state machine | counts, threshold, incidents, one-down/one-up dedup |
| api | CRUD on monitors, reads of status/history/incidents, check-now, manual close | monitor entity, validation, status derivation, history reads |

The notifier (master 6.6) is downstream of this engine and is specified in PRD-003. Channel attachment is referenced here because the state machine emits notification events, but delivery is out of scope.

### 1.2 Goals

1. Reproduce the proven v1 monitoring mechanics exactly: check execution and failure reasons (master 6.3), assertion priority, the alerting state machine (master 6.4), monitor status derivation (master 6.5), and per-field validation (master appendix A).
2. Run that loop distributed at the scale committed in master section 12: ~10,000 checks/sec sustained with 2x burst, scheduling within 5 s of due at p99, no single point that stalls everyone.
3. Accept per-region results from the regional data plane and reduce them to one monitor verdict using `down_policy` and probe-fleet health (master 6.7), without changing the state machine.
4. Keep history fast and uptime correct across the plan's retention window (master section 12 data retention).
5. Make every behavior testable: this document carries a full acceptance table and concrete acceptance criteria (section 10).

### 1.3 Non-goals

1. **Non-HTTP check types.** v1 GA is HTTP / HTTPS only (master 6.1). TCP, TLS-cert-expiry, DNS, keyword/browser, and ICMP are phased to Phase 3 (master section 15). The `type` field exists on the monitor entity from day one so adding types is additive and never a migration of meaning (master 6.1). This sub-PRD specifies HTTP semantics only; the field is reserved and defaults to `http`.
2. **A separate "degraded" or "latency" monitor status in v1.** Status stays the four derived values disabled / pending / down / up (master 6.5, master decision 7). Latency-degraded-but-up is SLO/percentile alerting, phased to Phase 3. The unrelated **coverage-degraded** signal from multi-region (master 6.7) is a separate field, not a status value (section 5).
3. **Re-notify-while-down and escalation.** The contract is one down alert, one up alert, per incident (master 6.4). Re-notify is a phased option, default off (master section 15, section 11 of this doc).
4. **Notification delivery.** Channels, payloads, retry/backoff are PRD-003.
5. **Multi-region topology and probe-fleet health internals.** Region selection, fleet heartbeats, and fail-over are PRD-007. This document specifies only how per-region results feed the engine (section 4.9).
6. **Billing and entitlement source of truth.** Interval floors and monitor caps are PRD-006. This document references where the engine reads and enforces them (section 2.4).

---

## 2. Monitor entity and configuration

### 2.1 Ownership and identity

Each monitor belongs to exactly one org (master 6.2). Every monitor row carries `org_id` and every read and write is scoped to the caller's active org (master section 13 tenant isolation). The monitor carries a `type` field that defaults to `http` and is the only supported value in v1 (master 6.1, section 1.3).

### 2.2 Configuration fields

Fields and semantics are identical to v1, scoped to an org, reused verbatim from master 6.2:

| Field | Required | Type / enum | Default | Notes |
|-------|:--------:|-------------|---------|-------|
| `type` | yes | enum, `http` only in v1 | `http` | reserved for phased types (master 6.1) |
| `name` | yes | string | - | human label |
| `url` | yes | string | - | full http(s) URL |
| `method` | yes | GET/POST/PUT/PATCH/DELETE/HEAD | GET | |
| `headers` | no | list of {key, value, secret?} | empty | `secret` true means encrypted at rest, redacted on read (master section 13) |
| `body` | no | string | none | only for POST/PUT/PATCH |
| `expected_status_codes` | yes | list of explicit codes and/or `2xx/3xx/4xx/5xx` | `200` | |
| `timeout_seconds` | yes | integer 1..60 | 10 | |
| `interval_seconds` | yes | integer, min 30 hard floor, >= timeout | 60 | plan tier may raise the effective floor (master section 11, PRD-006) |
| `enabled` | yes | bool | true | drives `disabled` status |
| `max_latency_ms` | no | positive integer | none | latency assertion |
| `body_contains` | no | string | none | substring assertion, body read up to 64 KB cap |
| `failure_threshold` | yes | integer >= 1 | 1 | consecutive unhealthy checks to open an incident |
| `notification_channel_ids` | no | list | empty | empty = tracked silently (master 6.4) |
| `regions` | yes | non-empty list of region codes | plan home region | plan-gated count and set (PRD-007, PRD-006) |
| `down_policy` | yes | enum `any` / `quorum` / `all` | `quorum` | cross-region verdict rule (master 6.7, PRD-007) |

### 2.3 Per-field validation (reused verbatim from master appendix A / v1 12.4)

Enforced on create and update, server-side. The server is authoritative; the UI mirrors. Errors use the standard envelope (`code` / `message` / `fields`) from v1 12.3.

| Field | Validation rule |
|-------|-----------------|
| `name` | required, non-empty after trim, max 200 chars |
| `url` | required, absolute URL with scheme `http` or `https` only (others rejected), must have a host |
| `method` | required, one of GET/POST/PUT/PATCH/DELETE/HEAD, default GET |
| `headers` | optional list of {key, value, secret?}; non-empty keys, no duplicates, max ~50; `secret` default false (when true, encrypted at rest and redacted on read) |
| `body` | optional; only for POST/PUT/PATCH (rejected for GET/HEAD/DELETE), size cap ~1 MB |
| `expected_status_codes` | required, non-empty; explicit codes (100..599) and/or `2xx/3xx/4xx/5xx`; default `200` |
| `timeout_seconds` | required integer 1..60, default 10 |
| `interval_seconds` | required integer, minimum 30 hard floor, must be >= timeout, default 60 (plan tier may raise the effective floor) |
| `enabled` | required bool, default true |
| `max_latency_ms` | optional positive integer |
| `body_contains` | optional string, max ~1000 chars; engine reads up to the 64 KB body cap to test it |
| `failure_threshold` | required integer >= 1, default 1 |
| `notification_channel_ids` | optional list; each must reference an existing channel in the same org; empty allowed (tracked, silent) |
| `regions` | required non-empty list of region codes; each must be a region the org's plan includes (rejected otherwise with the per-field error); no duplicates; count limited by the plan; default the plan's home region (master section 11, 6.7, PRD-007) |
| `down_policy` | required enum, one of `any` / `quorum` / `all`, default `quorum` (master 6.7) |

Cross-field rules made explicit (already implied by the table, restated so they are testable):

- `interval_seconds >= timeout_seconds` is a hard rule. Setting interval below timeout is rejected on the `interval_seconds` field.
- `body` is rejected when `method` is GET/HEAD/DELETE, on the `body` field.
- The interval floor is the max of the 30 s hard floor and the plan's tier floor (master section 11). On Free the effective floor is 900 s (15 min); a request below it is rejected on `interval_seconds` with an upsell (PRD-006).

### 2.4 Where entitlements are enforced (cross-reference PRD-006 and PRD-007)

The engine does not own entitlements; it reads and enforces them in two independent places, matching master section 11:

1. **api on write.** A create or update that sets `interval_seconds` below the plan floor, picks a `region` not in the plan, exceeds the region count, or would create a monitor past the monitor cap is rejected with the per-field error shape and an upsell. Source of truth for plan limits is PRD-006; region entitlement is PRD-007.
2. **scheduler on dispatch.** The scheduler independently respects the org's interval floor and region entitlement on every dispatch, so a monitor created under a higher plan cannot keep running faster or in a richer region set after a downgrade (master section 11). Neither side trusts the other.

Entitlements are cached for the hot path; invalidation follows plan changes (master section 11, PRD-006).

---

## 3. Check execution semantics

This section reuses master 6.3 / v1 4.2 and specifies the worker behavior in depth. The mechanics are proven; this is the exact contract.

### 3.1 The healthy / unhealthy decision

For each scheduled check a worker makes the configured request with the configured timeout and records the outcome. A check is **healthy** only if ALL configured assertions pass, evaluated in this order:

1. Request completed without connection error and without timing out, AND
2. Status code matches `expected_status_codes`, AND
3. If `max_latency_ms` is set, measured time is at or under it, AND
4. If `body_contains` is set, the response body contains the substring.

If any step fails the check is **unhealthy** with exactly one primary `failure_reason`.

### 3.2 Assertion priority order and the full failure_reason set

When a check is unhealthy, exactly one primary `failure_reason` is recorded, in this priority order (reused from master 6.3 / v1):

`blocked_target` -> `connection_error` -> `timeout` -> `status_mismatch` -> `latency_exceeded` -> `body_assertion_failed`

| `failure_reason` | Meaning | Request sent? |
|------------------|---------|:-------------:|
| `blocked_target` | target resolved to loopback, link-local, cloud-metadata, or private (RFC1918) range; SSRF policy blocked it (master section 13) | no |
| `connection_error` | DNS failure, connection refused, TLS error, reset, or any transport-level failure before a response | attempted |
| `timeout` | no full response within `timeout_seconds` | attempted |
| `status_mismatch` | response received, status code not in `expected_status_codes` | yes |
| `latency_exceeded` | status matched, but measured time exceeded `max_latency_ms` | yes |
| `body_assertion_failed` | status and latency passed, but the response body (read up to 64 KB) did not contain `body_contains` | yes |

The priority order means the first failing condition wins. A 503 that also took too long records `status_mismatch`, not `latency_exceeded`, because status is checked before latency. A target that resolves to a private IP records `blocked_target` and the request is never sent, so there is no status code and usually no latency.

### 3.3 The 64 KB body cap rule

Bodies are read only up to a 64 KB cap and only when `body_contains` is set. Full bodies are never stored (master 6.3). A match string that would only appear past the 64 KB cap fails the body assertion and records `body_assertion_failed`; this limit is documented in the UI help so customers do not expect deep-body matching (master 6.3). When `body_contains` is not set, the body is not read for assertion purposes at all.

### 3.4 What a check result stores

Each check result stores (reused from master 6.3):

| Field | Type | Notes |
|-------|------|-------|
| monitor id | id | |
| org id | id | tenant scope (master section 13) |
| region | region code | the region the check ran from; present from Phase 0 (master 6.3, PRD-007) |
| timestamp | RFC3339 UTC | when the check ran |
| healthy | bool | the section 3.1 decision |
| failure_reason | enum or null | null when healthy; one of section 3.2 otherwise |
| http status code | integer or null | null on `blocked_target`, `connection_error`, `timeout` |
| latency ms | integer or null | null when no response was measured (see 3.6) |
| error text | short truncated string or null | short transport-level detail; never a full body |

Full response bodies are never stored on the per-check result row (the high-volume firehose stays lean). The one place a response is captured is the last-failure snapshot in section 3.8, a separate per-monitor record. The `region` field is in the check-job and check-result schema from Phase 0 so cross-region aggregation is additive, never a migration (master 6.3, PRD-007).

### 3.5 Latency measurement

Measured time is the wall-clock duration of the request from connection start to the point the engine has enough of the response to judge it (status received, and body read up to the 64 KB cap when `body_contains` is set). It is the value compared against `max_latency_ms` and the `latency ms` stored on the result. Latency is recorded on any check that received a response, including a `status_mismatch` (the response came back, it was just the wrong code). Latency is null when no response was measured: `blocked_target` (never sent), `connection_error` (no response), and `timeout` (no full response).

### 3.6 Timeout behavior

The worker enforces `timeout_seconds` as the total time to get a full response. On expiry the request is abandoned, the check is unhealthy with `timeout`, status code is null, and latency is null. Because `interval_seconds >= timeout_seconds` is a validation rule (section 2.3), a check can always finish or time out before the next one for that monitor is due, so a slow endpoint cannot cause checks to overlap for the same monitor in the same region.

### 3.7 SSRF enforcement at execution time

SSRF protection is on by default and not customer-disableable (master section 13). The worker resolves the host, validates every resolved IP against the block list (loopback, link-local, cloud-metadata `169.254.169.254`, RFC1918), then connects to the validated IP to defeat DNS rebinding. Redirects are re-validated on each hop. A blocked target records `blocked_target` with the request not sent. Regional worker fleets run least-privilege with their own egress controls (master section 13, PRD-007).

### 3.8 Failed-check response capture (last failure snapshot)

To help a user debug *why* a monitor is failing, the platform keeps the response from the **most recent failed check**, per monitor. When a check is unhealthy and a response actually came back (`status_mismatch`, `latency_exceeded`, or `body_assertion_failed`), the worker captures:

- the response status code,
- the response headers,
- the response body, read up to the same 64 KB cap as `body_contains` (a `truncated` flag is set when the body was longer).

This is stored as a single per-monitor record that is **overwritten on each new failure** (it is the "last failure," not a history), so it does not grow with check volume. The capture-level failures that never produced a response (`blocked_target`, `connection_error`, `timeout`) have nothing to capture; their `error text` on the result already carries the transport detail.

Whether the snapshot is captured at all is a **plan entitlement** (`failure_snapshot`, PRD-006). The worker reads the org's entitlement before running the check and, when the org is not entitled, the check does not read the response for a snapshot at all, so an ungated org pays no extra cost on the worker (not just "captured then discarded"). It is **currently enabled for all plans**; the specific tiers it is limited to are GTM-tunable and decided when billing ships. The enforcement seam exists from day one so turning it into a paid feature is a config change, not a rebuild.

Data treatment (decision): the captured response is treated as ordinary **operational** data (master section 13), not as a secret. Pulse does not assume a monitored endpoint returns anything sensitive on a failure; what a customer's endpoint returns is the customer's responsibility. The snapshot is therefore stored in plaintext, org-scoped like every other result (a tenant only ever sees its own), and is **never** shown on a public status page (status pages expose only friendly names and never check internals, master 8). It is visible to org members in the app/API alongside the monitor's other check detail.

---

## 4. Alerting state machine

This section reuses master 6.4 / v1 12.5 exactly. The state machine is the correctness-critical core of the engine.

### 4.1 State per monitor

State per monitor is a running count of consecutive unhealthy checks and whether an incident is open. `T` = `failure_threshold`. In-flight state is derived from stored data, never held only in memory (master 6.6), so the alerting service survives restarts.

### 4.2 Full acceptance table (reused verbatim from master 6.4 / v1 12.5)

The table uses `T = 3`; `H` = healthy, `F` = unhealthy. This is the canonical behavior; every implementation must reproduce it row for row.

| Step | Check | Fails before | Action | Fails after | Incident | Status | Notification |
|------|-------|--------------|--------|-------------|----------|--------|--------------|
| 1 | H | 0 | none | 0 | none | up | none |
| 2 | F | 0 | count++ | 1 | none | up | none |
| 3 | H | 1 | reset (blip absorbed) | 0 | none | up | none |
| 4 | F | 0 | count++ | 1 | none | up | none |
| 5 | F | 1 | count++ | 2 | none | up | none |
| 6 | F | 2 | count++ reaches T, open incident | 3 | open (started_at = step 4 time) | down | ONE down alert per attached channel |
| 7 | F | 3 | stay down, no re-notify | 4 | open | down | none |
| 8 | H | 4 | close incident, reset count | 0 | closed (ended_at = step 8 time, close_reason recovered) | up | ONE recovery alert per attached channel |

With `T = 1`, a single `F` opens the incident immediately; the next `H` closes it.

### 4.3 started_at = first-fail-of-run

`started_at` is the FIRST failing check in the run that opened the incident (step 4 in the table), not the threshold-crossing check (step 6) (master 6.4). The recovery duration is `ended_at - started_at`. Concretely: with `T = 3`, the run of failures begins at step 4, the incident opens at step 6, but the incident's `started_at` is the step-4 timestamp. The engine must remember the timestamp of the first fail in the current run so it can stamp `started_at` correctly when the threshold is later crossed.

### 4.4 Blip reset

Any `H` while count > 0 and no incident is open resets count to 0 with no notification (step 3, master 6.4). A single failing check before `T` is reached never opens an incident and never changes status to down. This is what absorbs a one-off flaky failure on an otherwise healthy endpoint.

### 4.5 Disable-while-down

Disabling a down monitor closes the open incident with `ended_at` = disable time, `close_reason = disabled`, and **no** recovery alert; the monitor's status becomes `disabled` (master 6.4, 6.5). The count is reset. This is distinct from a recovery close: no up notification is sent because the monitor was not observed to recover, it was turned off.

### 4.6 Edit-while-down

Editing a down monitor's config does **not** auto-close the open incident (master 6.4). The next check with the new config drives the transition normally: if the new config makes the check pass, the incident closes as a normal recovery (one up alert); if it still fails, the incident stays open. The edit itself is never a state transition.

### 4.7 One-down / one-up contract

Exactly one down alert and one up alert per incident, per attached channel (master 6.4). No re-notify while down in v1 (step 7 sends nothing). Dedup and per-monitor ordering are the correctness-critical part and must be exactly-once in effect even if a check-result event is redelivered (master 6.6). A redelivered "open incident" event must not produce a second down alert; a redelivered "close" must not produce a second up alert.

### 4.8 No-channels-still-transitions

A monitor with zero attached channels still opens and closes incidents and changes status; it just sends nothing (master 6.4). The state machine and incident records are independent of whether any channel is attached. This keeps the monitoring truth complete for history, status pages, and the API even when the org chose silent tracking.

### 4.9 How the multi-region down_policy verdict feeds the state machine

When a monitor has more than one region, the alerting service first reduces the per-region results into a single monitor-level healthy / unhealthy verdict, then drives the exact state machine above with that verdict (master 6.4, 6.7). The state machine itself is unchanged: counts, threshold, incident open/close, and one-down/one-up dedup all govern the transition as written.

The verdict is computed from the per-region results for one scheduled tick, using `down_policy` and probe-fleet health (master 6.7, PRD-007):

| `down_policy` | Monitor verdict is unhealthy when |
|---------------|-----------------------------------|
| `any` | at least one healthy-region result is unhealthy |
| `quorum` (default) | a majority of healthy-region results are unhealthy |
| `all` | every healthy-region result is unhealthy |

Probe-fleet health rules that the engine relies on (specified in full in PRD-007, summarized here because they change the verdict input):

- A region that returns an unhealthy result counts toward `down_policy` (the target is down from that region).
- A region that returns no result and is itself unhealthy is excluded from `down_policy`, never counted as the target being down. A missing result from a region we run is never read as the customer being down (master 6.7 product guarantee).
- When too few healthy regions remain to satisfy the monitor's `down_policy`, the engine does not declare the monitor down on missing data. It surfaces a **coverage-degraded** signal on the monitor (section 5) and, plan permitting, fail-over restores coverage (PRD-007). Coverage-degraded is not a down verdict and does not page.

The single verdict per tick is what step-by-step counting consumes. The `failure_threshold` then still requires `T` consecutive unhealthy verdicts before an incident opens, exactly as in single-region.

---

## 5. Monitor status values

Exactly four derived values, evaluated top to bottom (reused from master 6.5 / v1 12.1):

| Status | Condition | Priority |
|--------|-----------|:--------:|
| `disabled` | `enabled` is false | 1 (over all) |
| `pending` | enabled, zero check results yet | 2 |
| `down` | enabled, has results, an incident is open | 3 |
| `up` | enabled, has results, no open incident | 4 |

Rules:

- A single failing check before `T` is reached does NOT make status `down`. Status stays `up` until the incident opens (master 6.5).
- "Last check time" = timestamp of the most recent result; null when pending.
- "Last latency" = latency of the most recent result; null when pending or when the last check had no latency (connection error, timeout, or blocked target) (master 6.5).
- Status is derived, never user-editable (master 6.5).
- There is no separate "degraded" or "latency" status in v1 (master decision 7). An individual monitor is binary up/down once it has results.

### 5.1 Coverage-degraded (separate from status, from multi-region)

The **coverage-degraded** signal is a separate boolean/indicator on the monitor, surfaced in the UI and API, set when too few of our own healthy probe regions remain to satisfy the monitor's `down_policy` (master 6.7, PRD-007). It is NOT one of the four status values and it is unrelated to the target being slow (master decision 7). A coverage-degraded monitor keeps its normal up/down/pending status driven by the regions that did report; coverage-degraded only tells the customer that our probe coverage is reduced, not that their service is unhealthy. Full definition and the fail-over behavior live in PRD-007.

---

## 6. Incidents

### 6.1 Lifecycle

An incident is the durable record of one down-and-recovery cycle for one monitor. It has two states: **open** and **closed**. It opens when the state machine crosses `T` (step 6) and closes on recovery (step 8), on disable-while-down (section 4.5), or on manual close (section 6.4). Incidents and monitor config are retained for the life of the org, not subject to raw-result cleanup (master section 12).

### 6.2 Fields

| Field | Type | Set when |
|-------|------|----------|
| id | id | open |
| monitor id, org id | id | open |
| started_at | RFC3339 UTC | open, = first-fail-of-run timestamp (section 4.3) |
| ended_at | RFC3339 UTC or null | null while open; set on close |
| cause | enum (the `failure_reason` that opened it) | open; the failure_reason of the threshold-crossing run |
| close_reason | enum `recovered` / `disabled` / `manual` / null | null while open; set on close |
| closed_by | user id or null | set only on manual close (section 6.4) |
| annotations | list of {author, text, at} | any time while open or after close (section 6.3) |

`cause` records why the incident opened (the primary `failure_reason` of the failing run), so the incident timeline and notifications can say "Reason: status_mismatch (HTTP 503)" (master appendix B payloads).

### 6.3 Annotations

Owner, admin, and member can post a short human update on an incident (master 6.4 permission matrix: acknowledge/annotate is member+). Annotations are timestamped and attributed to the author. They appear on the incident timeline and, for incidents on displayed monitors, on the public status page (master section 8). Annotations never change the incident's state or timing.

### 6.4 Manual close

Owner or admin can manually close an open incident (master permission matrix; member cannot). A manual close sets `ended_at` = close time, `close_reason = manual`, and `closed_by` = the actor. It is an override of the alerting machine, which is why it is restricted (master decision 8, master 6.4 design notes). A manual close does NOT send a recovery notification, because the monitor was not observed to recover; the next healthy check resets the count and the monitor returns to `up` normally. Manual close is an audited action (master section 13).

### 6.5 Duration computation

Incident duration = `ended_at - started_at`. Because `started_at` is the first-fail-of-run (section 4.3), the reported duration reflects when the outage actually began, not when the threshold was crossed. Duration is computed at close and is what recovery notifications report as `duration_seconds` (master appendix B) and what the status page and incident list display. While an incident is open, duration is "open since started_at" (no fixed end yet).

---

## 7. Check-now (manual trigger)

Reused from master 6.3. Check-now lets an authorized user (owner/admin/member; viewer cannot, per the matrix) trigger an immediate check.

### 7.1 Behavior

- A check-now produces a normal check result and feeds the alerting machine exactly like a scheduled check (master 6.3). It can open or close an incident if it crosses a threshold or recovers a down monitor.
- For a multi-region monitor, check-now runs one check per selected region (the same fan-out as a scheduled tick) and the per-region results are reduced by `down_policy` into one verdict, exactly as section 4.9. Check-now is not single-region.
- Check-now does NOT shift the scheduled cadence (master 6.3). The next scheduled check still fires at its originally planned time. The manual result is simply an extra data point inserted into the timeline.

### 7.2 Per-monitor serialization (Redis lock) and 409 semantics

Check-now is serialized per monitor: one check per monitor at a time (master 6.3). In the distributed runtime this per-monitor exclusion is coordinated with a short Redis lock (master 6.3, 6.6). The lock covers a scheduled check and a check-now for the same monitor so the two cannot run concurrently for the same monitor in the same region.

- If a check-now is requested while a check for that monitor is in flight, the API returns the in-flight or just-finished result, or `409` (master 6.3). The `409` carries the standard error envelope and signals "a check is already running for this monitor, try again shortly."
- The lock is short-lived and self-expiring so a crashed worker cannot hold a monitor's check-now hostage; the lock TTL is bounded by `timeout_seconds` plus headroom (architecture chooses the exact TTL).

---

## 8. History and retention

### 8.1 Per-plan retention tiers

Raw check results are retained per the org's plan tier (master section 12, PRD-006):

| Plan | Raw check-result retention |
|------|----------------------------|
| Free | 7 days |
| Starter | 30 days |
| Team | 90 days |
| Business | 180 days |

After the tier window, raw results are deleted by a background cleanup job (section 8.4). Incidents and monitor config are retained for the life of the org and are not subject to raw-result cleanup (master section 12).

### 8.2 Rollups for fast history and uptime at scale

At scale, long retention is backed by rollups (hourly aggregates) so per-monitor history and uptime views stay fast as raw rows grow (master section 12). Raw rows age out while rollups persist for uptime math, including on status pages. Rollups are per monitor and per region so per-region history and uptime survive raw cleanup, and cross-region uptime aggregates correctly (master 6.7, PRD-007).

The product commitment is **fast history and correct uptime over the retention window** (master section 12). How the rollup is structured is an architecture detail; the engine guarantees:

- History reads (the recent check table and sparkline) are fast at the committed scale, served from raw rows inside the window and rollups beyond it.
- Uptime over a window (24h / 7d / 90d, master section 8) is correct and stays correct after raw rows are cleaned, because the rollup carries the aggregate.
- Rollups land in Phase 2 (master section 15); at beta scale raw queries are fine (master Phase 1 out-of-scope note).

### 8.3 Uptime definition

Uptime over a window is the fraction of time the monitor was up versus the total monitored time in the window, derived from check results and incidents. The status page shows an uptime summary per displayed monitor (master section 8). Pending periods (no results yet) and disabled periods are not counted as down. The exact uptime formula (per-check ratio vs incident-duration ratio) is an open decision (section 11).

### 8.4 Cleanup behavior

A background cleanup job deletes raw check results older than the org's retention tier (master section 12). Cleanup:

- Never deletes incidents or monitor config.
- Never deletes rollups within the product's uptime guarantee window.
- Is org-scoped and respects the per-org tier, so a downgrade shortens the window and a future cleanup pass trims raw rows accordingly.
- Runs as a background job and does not block check execution or reads.

---

## 9. Scale promise

These are the engine's slice of the committed targets in master section 12. They are commitments, not aspirations.

| Target | Commitment (master section 12) | Engine implication |
|--------|--------------------------------|--------------------|
| Sustained check throughput | ~10,000 checks/sec sustained, 2x burst | worker fleet scales out with monitor count and frequency; stateless workers |
| Scheduling accuracy | check dispatched within 5 s of scheduled time at p99 under normal load | scheduler + Kafka path stays short; no per-org or per-monitor serial bottleneck |
| Check-result to decision latency | alerting state updated within 5 s of the result at p99 | worker -> Kafka -> alerting path stays short |
| Active monitors | hundreds of thousands (design for 500k) | per-org isolation holds; schedule rebuildable from PostgreSQL on start |

No pile-ups and no single stall point (master 6.6):

- The scheduler enqueues check jobs keyed so a given monitor's checks are ordered and not duplicated (master 6.6). It rebuilds the schedule from PostgreSQL on start and derives in-flight incident state from stored data, never memory-only.
- Workers are stateless and horizontally scaled; one slow endpoint occupies one worker's request slot, not the fleet. `interval >= timeout` means a slow monitor cannot queue overlapping checks of itself (section 3.6).
- The alerting service keys per-monitor work so ordering and dedup hold per monitor without a global lock; one noisy monitor does not stall others.
- No single component is a shared serial path for all orgs' checks. A failure or slowdown in one worker, one region, or one monitor does not stall the whole fleet (master 6.6 product promise).

---

## 10. User stories and acceptance criteria

### 10.1 User stories

1. As a developer, I create an HTTP monitor for my health endpoint and within one interval I see a check result, so I know it is being watched.
2. As an operator, when my endpoint returns 503 for `T` consecutive checks, I get exactly one down alert and an incident opens timed from the first failure.
3. As an operator, when my endpoint recovers, I get exactly one recovery alert with the correct outage duration and the incident closes.
4. As an operator, a single flaky failure on an otherwise healthy endpoint does not page me, because the blip is absorbed below threshold.
5. As an operator, I run check-now to confirm a fix without waiting for the next scheduled check, and it does not shift my cadence.
6. As an SRE with multi-region monitors, a single regional network blip does not page me under the default `quorum` policy, and I am never paged because Pulse's own region went down.
7. As an admin, I can manually close a stuck incident, and it is recorded as a manual close without firing a false recovery alert.
8. As a viewer, I can read status, history, and incidents but cannot edit monitors or run check-now.

### 10.2 Acceptance criteria (testable)

Validation (section 2.3):

- A1: Creating a monitor with `interval_seconds = 20` is rejected on `interval_seconds` (below the 30 s hard floor).
- A2: On Free, creating a monitor with `interval_seconds = 60` is rejected on `interval_seconds` with an upsell (below the 120-min plan floor) (PRD-006).
- A3: Creating a GET monitor with a `body` is rejected on `body`.
- A4: Creating a monitor with `interval_seconds = 5, timeout_seconds = 10` is rejected on `interval_seconds` (interval < timeout).
- A5: Creating a monitor with a `region` not in the org's plan is rejected on `regions` (PRD-007).
- A6: A `url` with scheme `ftp` is rejected on `url`.

Execution (section 3):

- B1: A 200 within `max_latency_ms` with `body_contains` present in the first 64 KB is healthy.
- B2: A 503 records `status_mismatch` with `status_code = 503` and a non-null latency.
- B3: A response slower than `max_latency_ms` but with a matching status records `latency_exceeded` (status checked before latency).
- B4: A target resolving to `169.254.169.254` records `blocked_target`, request not sent, status and latency null.
- B5: A DNS failure records `connection_error`, status and latency null.
- B6: No response within `timeout_seconds` records `timeout`, status and latency null.
- B7: A `body_contains` string appearing only after the first 64 KB records `body_assertion_failed`.

State machine (section 4, `T = 3`):

- C1: F, F, F opens an incident on the third F, sends one down alert, `started_at` = first F's timestamp, status becomes down.
- C2: F, F, H resets the count, no incident, no alert, status stays up (blip absorbed).
- C3: After an incident is open, F, F, F sends no further alerts (no re-notify).
- C4: After an incident is open, an H closes it, sends one recovery alert, `close_reason = recovered`, duration = close - first F.
- C5: With `T = 1`, one F opens immediately and one H closes.
- C6: A redelivered open-incident event does not send a second down alert.
- C7: A monitor with zero channels still opens/closes the incident and changes status, sending nothing.
- C8: Disabling a down monitor closes the incident with `close_reason = disabled`, no recovery alert, status becomes disabled.
- C9: Editing a down monitor does not close the open incident; the next check drives the transition.

Multi-region verdict (section 4.9, PRD-007):

- D1: Two of three regions report unhealthy under `quorum`; the verdict is unhealthy.
- D2: One of three regions reports unhealthy under `quorum`; the verdict is healthy (single blip not paging).
- D3: One region returns no result and is itself unhealthy; it is excluded, the verdict uses the remaining healthy regions, and the monitor is never declared down on the missing region.
- D4: Too few healthy regions remain to satisfy `down_policy`; the monitor is not declared down, coverage-degraded is surfaced.

Check-now (section 7):

- E1: Check-now produces a normal result that can open or close an incident.
- E2: Check-now does not move the next scheduled check time.
- E3: A check-now while a check is in flight for the same monitor returns the in-flight/just-finished result or `409`.
- E4: A viewer's check-now is rejected by role (master matrix).

Incidents and history (sections 6, 8):

- F1: Manual close by an admin sets `close_reason = manual`, `closed_by`, no recovery alert, and is audited.
- F2: A member cannot manually close an incident (master matrix).
- F3: Uptime over a window stays correct after raw results past the retention tier are cleaned (served by rollups).
- F4: Raw results older than the org's retention tier are deleted; incidents and config are retained.

### 10.3 Edge cases

| Edge case | Engine behavior |
|-----------|-----------------|
| Flaky endpoint (intermittent failures below `T`) | blip reset absorbs them; no incident, no page, status stays up (section 4.4). If the customer finds this noisy, raising `failure_threshold` is the lever (master section 14 signal-to-noise). |
| Slow endpoint (response near or over timeout) | `timeout` if no full response in `timeout_seconds`; `latency_exceeded` if `max_latency_ms` is set and exceeded but a response arrived in time. `interval >= timeout` prevents self-overlap (section 3.6). |
| Redirects | followed; each hop's resolved IP is re-validated for SSRF (section 3.7). The final response's status and body are what the assertions judge. |
| Large bodies | body read only up to 64 KB and only when `body_contains` is set; full bodies never stored; a match past 64 KB fails the assertion (section 3.3). |
| DNS failure | `connection_error`, request attempted, status and latency null (section 3.2). |
| SSRF / private target | `blocked_target`, request never sent, not customer-disableable (section 3.7). |
| Our own probe region down | excluded from the verdict, never counted as the target down; coverage-degraded if coverage drops too low; never pages (section 4.9, master 6.7 guarantee). |
| Check event redelivered | dedup keeps one-down/one-up exactly-once in effect (section 4.7). |

---

## 11. Open decisions (with recommended defaults)

These are the engine-scoped open decisions. Each ships at the recommended default unless overridden. Master-level decisions (master section 16) are not re-litigated here.

1. **Separate "degraded / latency" monitor status in v1.** Recommended: **no.** Keep the four statuses disabled/pending/down/up (master 6.5, master decision 7). Latency-degraded-but-up is SLO/percentile alerting, phase 3. Trade-off: a slow-but-up endpoint is not visually distinct from a fast-but-up one in v1; acceptable, `max_latency_ms` already turns "too slow" into a down via `latency_exceeded` when the customer wants that.

2. **Re-notify-while-down.** Recommended: **off** (the one-down/one-up contract, master 6.4). Trade-off: a long outage produces only the initial page; mitigated because the incident stays visibly open. Re-notify and escalation are phase 3, default off (master section 15).

3. **Uptime formula.** Recommended: **incident-duration based** (uptime = 1 - sum(open incident durations) / monitored time in window), not per-check ratio. It matches what customers expect on a status page (downtime = how long the incident was open) and survives raw-result cleanup cleanly because incidents are retained for the life of the org (section 6.1, 8.4). Trade-off: brief sub-threshold blips do not reduce uptime, which is consistent with "a blip is not an outage" (section 4.4). Open until status-page work (master section 8) confirms.

4. **Latency stored on `status_mismatch`.** Recommended: **yes, store it** (section 3.5). The response came back, so the latency is real and useful for charts. Trade-off: a latency value next to a failed check can read oddly; the UI labels it clearly.

5. **Check-now contention response.** Recommended: **return the in-flight/just-finished result when one is available, else `409`** (section 7.2, master 6.3). Trade-off: clients must handle both shapes; documented in the API reference.

6. **Coverage-degraded surfacing detail.** Recommended: a boolean plus the affected region list on the monitor read (section 5.1). Full behavior is PRD-007; flagged here so the engine's monitor read contract is stable.

---

## 12. Dependencies

| Dependency | What this engine needs from it | Direction |
|------------|--------------------------------|-----------|
| PRD-003 Notifications | the engine emits notification events on incident open/close (one-down/one-up, section 4.7); PRD-003 owns channels, payloads (master appendix B), retry/backoff, and delivery latency (master section 12) | engine -> notifier (downstream) |
| PRD-006 Billing | source of truth for the per-plan interval floor, monitor cap, retention tier, and region count; the engine reads cached entitlements and enforces them at api-on-write and scheduler-on-dispatch (section 2.4, master section 11) | engine reads entitlements |
| PRD-007 Multi-Region | region selection, per-region check fan-out, probe-fleet health, coverage-degraded definition, and fail-over; the engine consumes per-region results and reduces them by `down_policy` into one verdict (section 4.9), and surfaces the coverage-degraded signal (section 5.1) | engine consumes per-region results |

Shared infrastructure (master 6.6): PostgreSQL (monitor config, check results, incidents, rollups), Redis (per-monitor check-now lock, cached entitlements), Kafka (check jobs and check-result events). These are owned at the platform level; the engine is a consumer.

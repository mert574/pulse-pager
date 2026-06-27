# RFC-020 - Heartbeat / Cron Monitoring

Status: DRAFT for review
Author: Engineering (data plane)
Parent: `docs/rfc/RFC-000-architecture-overview.md` (sections 2 topology, 5 eventing, 14 reuse map)
Depends on: RFC-001 (monitor + incident schema, RLS, `WithOrg`), RFC-002 (`check.results` contract), RFC-004 (the scheduler tick loop), RFC-005 (the `domain.CheckResult` shape), RFC-006 (alerting state machine and incident open/close), RFC-012 (`api/openapi/v1.yaml` is the source of truth)
Depended on by: RFC-007 (notification copy gains a missing-ping reason line), RFC-013 (the create form and the ping-url display)
Product source: `docs/PRD.md` / `docs/prd/PRD-002` (the `type` field is additive from day one), `docs/BACKLOG.md` ("More check types: SSL-expiry and cron/heartbeat"; SSL shipped, heartbeat is the open half)

House style: timestamps RFC3339 UTC on the wire. No em-dashes. Tables and code blocks over prose.

---

## 1. Overview and scope

Every monitor today is an active check: the scheduler dispatches a job, a worker dials the target, and the result drives alerting. Heartbeat monitoring inverts that. The monitored thing (a cron job, a backup script, a queue worker) calls **us** on a schedule by hitting a ping URL. The monitor is healthy while the pings keep arriving on time and goes down when a ping is late past its grace window. There is no outbound dial and no worker involved.

The whole design rests on one decision (section 2): a heartbeat does not get a new alerting path. A small sweep turns "a ping is overdue" into a synthetic `domain.CheckResult` and drops it onto `check.results`, the exact topic the worker already produces to. From there the existing alerting state machine (RFC-006) opens the incident, and the notifier (RFC-007) delivers it, with zero changes to either. A ping arriving after a down period emits a healthy result the same way, which closes the incident as a normal recovery.

### 1.1 What this RFC owns

| Owned | Section |
|-------|---------|
| The `heartbeat` monitor type and its new failure reason | 3 |
| The `monitor_heartbeats` config + state table and the token-at-rest scheme | 3 |
| The public, token-gated ingest endpoint (`POST/GET /ping/{token}`) | 4 |
| The overdue sweep and the synthetic-result contract that reuses alerting | 5 |
| The recovery-on-ping emission | 5 |
| The `v1.yaml` changes: monitor type, heartbeat config, `ping_url`, token rotate | 6 |
| Entitlements: monitor-cap counting and the period floor | 7 |
| Frontend: the create form, the ping-url reveal, the live late/down chip | 8 |
| Security of a public unauthenticated ingest (token entropy, hashing, rate limit, no enumeration) | 9 |

### 1.2 What this RFC does not own

| Not owned | Owner |
|-----------|-------|
| The alerting state machine, incident open/close, the `last_applied_result_id` watermark | RFC-006 (reused unchanged) |
| Notification delivery and dedup | RFC-007 (reused unchanged) |
| The `check.results` wire schema | RFC-002 / RFC-005 |
| The monitor row, RLS policy, `WithOrg` accessor pattern | RFC-001 |
| `/start` and `/fail` sub-pings and job-duration measurement | Section 11 (fast-follow, out of v1) |
| Multi-region attribution | n/a (a heartbeat has no region; section 5.3) |

---

## 2. Why a heartbeat does not get its own pipeline

### 2.1 Decision

A heartbeat monitor reuses the active-check pipeline end to end. The only new producers of state are the ingest endpoint (records that a ping arrived) and a sweep (notices a ping is overdue). Both express their outcome as a `domain.CheckResult` on `check.results`. Everything downstream is unchanged.

### 2.2 Reasoning

The alerting machine (RFC-006) is already the one place that decides "this monitor is down, open an incident" and "it recovered, close it", with idempotency, the open-incident partial unique index, and the notify seam all solved. The verdict it needs is a boolean `Healthy` plus a `FailureReason`. A missing ping produces exactly that. Building a parallel "heartbeat incident" path would duplicate the hardest, most correctness-sensitive code in the system for no benefit, and would split incidents into two models the UI and the API would both have to special-case.

So the inversion is contained to the edge: how a result is *produced*. A worker produces results by dialing out; a heartbeat produces them by the absence or arrival of an inbound ping. Past that boundary the two are identical.

### 2.3 Rejected alternatives

| Alternative | Why not |
|-------------|---------|
| A dedicated heartbeat incident table + its own alerting | Duplicates RFC-006's idempotency, dedup, and incident lifecycle; forks the incident model the API and FE consume |
| Compute the verdict in the API on each ping (no sweep) | A ping that never arrives means the API is never called, so the down transition has no trigger. Absence must be detected by something that runs on a clock |
| Run the sweep inside alerting | Alerting is a Kafka consumer with no timer; it reacts to results, it does not poll. The scheduler already owns the tick loop and the monitor list |

---

## 3. Data model

### 3.1 New monitor type and failure reason

`internal/domain/domain.go` today defines `MonitorHTTP` and `MonitorSSL` (`domain.go:18-20`) and nine failure reasons, six HTTP and three SSL (`domain.go:64-76`). Add:

```go
const MonitorHeartbeat MonitorType = "heartbeat"

// the monitored job did not ping us within its period + grace window.
const ReasonMissingPing FailureReason = "missing_ping"
```

One reason is enough for v1. "Late but within grace" is a display state, not a failure (section 5.4), so it never reaches a `CheckResult`.

### 3.2 Heartbeat config and state live in a side table

A heartbeat does not use `url`, `method`, `headers`, `body`, the assertion fields, `regions`, or `down_policy` on the `Monitor` row. It needs a period, a grace, an ingest token, and the last-ping bookkeeping. Rather than null out half the `monitors` columns and bolt four more on, keep the heartbeat-specific data in its own table keyed by monitor id, one row per heartbeat monitor.

```
CREATE TABLE monitor_heartbeats (
  monitor_id      BIGINT PRIMARY KEY REFERENCES monitors(id) ON DELETE CASCADE,
  org_id          BIGINT NOT NULL,                 -- carried for RLS, like every tenant table
  period_seconds  INT    NOT NULL,                 -- expected time between pings
  grace_seconds   INT    NOT NULL,                 -- allowed lateness before down
  token_hash      BYTEA  NOT NULL,                 -- SHA-256 of the ping token; the token is never stored
  last_ping_at    TIMESTAMPTZ,                     -- null until the first ping ever
  last_ping_source TEXT  NOT NULL DEFAULT '',      -- the caller IP/UA, for the UI; best-effort
  down            BOOLEAN NOT NULL DEFAULT false,   -- edge marker so the sweep emits once per transition (5.2)
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uniq_monitor_heartbeats_token ON monitor_heartbeats (token_hash);
```

RLS mirrors every other tenant table (RFC-001 6.1): enable + force row level security, policy on `org_id = current_org`, grant to `pulse_app`. The table is written through `store.Pool.WithOrg` like the rest. The one exception is the ingest lookup by `token_hash`, which has no org context yet (section 4.2): it uses a `SECURITY DEFINER` function returning a single monitor id, the same shape already used for `person_had_recent_inactive_subscription`.

`token_hash` is unique so the ingest can resolve a monitor from the token alone in one indexed lookup.

Decision: side table over widening `monitors`. Reasoning: the two shapes share almost no columns; a side table keeps the `monitors` row honest (an HTTP monitor's columns stay required, a heartbeat's stay absent) and matches the repo's one-file-per-entity store layout. Rejected: nullable columns on `monitors` (turns every HTTP-monitor query into a sea of nullable heartbeat fields and invites "which columns apply to which type" bugs).

### 3.3 The token is a URL credential, hashed at rest

The ping token is the only thing standing between a caller and writing a ping, so it is a high-entropy opaque secret, minted like an API key (RFC-003) or a webhook signing secret: generated once, shown to the user once, stored only as a SHA-256 hash. The ingest hashes the incoming token and looks it up by `token_hash`. A `rotate` action re-mints it. Format `pulse_hb_<32+ bytes base62>` so it is greppable in logs as "a heartbeat token leaked" without revealing which monitor.

---

## 4. The ingest endpoint

### 4.1 Shape

```
POST /ping/{token}      -> 200 (recorded) | 404 (no such token)
GET  /ping/{token}      -> 200 | 404         (GET is allowed: curl, wget, and cron one-liners default to GET)
```

It is hand-wired into the mux as its own `http.Handler`, not part of the generated `StrictServerInterface`, exactly like the auth routes and the billing webhook (`internal/billing/ingest.go:47` serves `POST /billing/webhooks/{provider}` the same way). It is unauthenticated by design: the token in the path is the credential, so there is no session, no org middleware, no `Authorization` header. This is the standard heartbeat ingest pattern (Healthchecks.io, Cronitor): the URL is the secret you bake into your cron line.

It lives on the api service. It is not under `/api/v1` because it is not a versioned JSON resource and not in the OpenAPI spec (same reasoning RFC-012 uses to keep `/auth/*` and jwks out of the spec). A short, memorable path matters because users paste it into crontabs.

### 4.2 Behavior

On a ping:

1. Hash the token, resolve the monitor id via the definer lookup. No match -> 404 with an empty body (no detail, section 9.3).
2. If the monitor is disabled, record nothing and return 200 (a disabled monitor is not monitored; we still 200 so the caller's cron does not see errors).
3. `UPDATE monitor_heartbeats SET last_ping_at = now(), last_ping_source = $ip, updated_at = now() WHERE monitor_id = $1`.
4. If the row was `down = true`, this ping is a recovery: flip `down = false` and emit a **healthy** synthetic result (section 5.3) so alerting closes the incident. If it was already up, do nothing else.
5. Return 200 fast. The emit in step 4 is the only non-trivial work and only happens on the recovery edge.

The endpoint is idempotent and side-effect-light: the common case (an up monitor pinging on time) is a single `UPDATE` and a 200. Two pings in the same instant both just set `last_ping_at`; last write wins; nothing downstream cares.

### 4.3 Why update-in-place and not an event per ping

A ping is not interesting on its own; only the *gap* between pings is. Storing every ping as a row or an event would be a high-volume write for data nobody queries. We keep just `last_ping_at`. The history that matters (down periods) is already captured as incidents by the reused pipeline.

---

## 5. The verdict: reusing alerting via synthetic results

### 5.1 The overdue sweep runs on the scheduler tick

The scheduler is already a tick loop with the monitor list in hand (`internal/scheduler/scheduler.go`, the `Dispatcher` with its `nextRun` map and `dispatchDue` on each `tick`). Add a parallel sweep on the same tick that handles heartbeat monitors instead of dispatching jobs for them. Heartbeat monitors are **not** put in the dispatch path (there is nothing to dispatch); the sweep is their entire scheduler footprint.

Each tick, for enabled heartbeat monitors:

```
expected_by = last_ping_at + period_seconds + grace_seconds
if last_ping_at is not null and now > expected_by and not down:
    set down = true
    emit a missing-ping CheckResult   (5.3)
```

A monitor that has never pinged (`last_ping_at IS NULL`) is **pending**, not down: we do not alert on a heartbeat that was created but whose job has not run yet. It becomes eligible for a down verdict only after its first ping establishes the cadence. (Open question 12.1 revisits an optional "expected first ping by" deadline.)

### 5.2 Edge-triggered, so it emits once per outage

The `down` boolean is the guard. The sweep emits a missing-ping result only on the false->true transition and never again while the monitor stays down, so `check.results` is not flooded tick after tick. Recovery (true->false) happens in the ingest path (4.2 step 4), also once. This mirrors how the rest of the system is edge-driven, and it leans on, rather than fights, alerting's idempotency: even if a transition double-fired, alerting's `last_applied_result_id` watermark and the open-incident partial unique index (RFC-006 5) would swallow the duplicate. The edge marker is the cheap first line; the watermark is the safety net.

### 5.3 The synthetic result

The sweep and the recovery path both build a `domain.CheckResult` and produce it to `check.results` exactly as a worker would (RFC-005). A heartbeat has no region, so it uses a sentinel region code `"ingest"` and the monitor carries an empty `Regions` set with the reduction treated as single-source (section 5.5).

| Field | Missing-ping (sweep) | Recovery (on ping) |
|-------|----------------------|--------------------|
| `Healthy` | false | true |
| `FailureReason` | `missing_ping` | nil |
| `Region` | `"ingest"` | `"ingest"` |
| `CheckedAt` | the sweep time (= when we noticed) | the ping time |
| `ScheduledAt` | `expected_by` (when the ping was due) | the ping time |
| `LatencyMs` / `StatusCode` / body fields | nil | nil |

Alerting consumes these with no idea they came from a heartbeat. It opens an incident with `CauseReason = missing_ping` and closes it on the healthy result with the normal `recovered` close reason. The incident, the notification, the status-page effect, and the history are all the existing machinery.

### 5.4 Up / late / down, and where "late" lives

The product wants a visible "late" state (overdue but inside grace) distinct from "down" (past grace). "Late" is not a failure and must not open an incident, so it never becomes a `CheckResult`. Instead it is surfaced through the live per-monitor state layer the FE already reads (`internal/checkstate`, the Redis chips the dashboard renders):

| State | Condition | Effect |
|-------|-----------|--------|
| pending | `last_ping_at IS NULL` | no alert; "waiting for first ping" |
| up | `now <= last_ping_at + period` | healthy |
| late | `period < since-last-ping <= period + grace` | display-only chip; no incident |
| down | `now > last_ping_at + period + grace` | incident open (via 5.3) |

The sweep writes the up/late/down marker into checkstate on each tick (cheap, it is already iterating heartbeat monitors), and only the down edge emits a result. This reuses the chip path built for active checks; no new FE state model.

### 5.5 No region reduction

A heartbeat reports from nowhere, so down-policy aggregation (RFC-006 / RFC-008) does not apply. The single `"ingest"`-tagged result is the verdict. Alerting's reduction over one source is a no-op (`any`/`quorum`/`all` over a single region all yield that region's result), so this needs no alerting change; we just never give it more than one source for a heartbeat.

---

## 6. API contract changes (`api/openapi/v1.yaml`)

All changes are in the spec; both the Go stubs and the TS client regenerate from it (RFC-012, `make gen`). Never hand-edit the generated files.

| Change | Detail |
|--------|--------|
| `Monitor.type` enum | add `heartbeat` |
| Heartbeat config | `heartbeat: { period_seconds, grace_seconds }` sub-object on the monitor; required when `type = heartbeat`, absent otherwise |
| `ping_url` (read-only) | returned in full (with the token) **once**, in the create/rotate response only, like a freshly minted API key; the GET monitor response returns it masked |
| Rotate token | `POST /orgs/{orgId}/monitors/{id}/heartbeat/rotate-token` -> new `ping_url`, owner/admin-ish (same role gate monitor edits use) |
| Validation | for `type = heartbeat`: `url`/`method`/assertions/`regions`/`down_policy` must be absent or ignored; `period_seconds` and `grace_seconds` required and positive |

`internal/api/monitors.go` validates the type discriminator: an HTTP monitor still requires its URL and assertions; a heartbeat requires its period and grace and rejects HTTP-only fields, so the two shapes cannot be mixed into an invalid row.

## 7. Entitlements

| Question | Decision | Reasoning |
|----------|----------|-----------|
| Counts toward the monitor cap? | Yes | A heartbeat is a monitor; it consumes a slot like any other. `CountEnabledMonitors` already counts the row regardless of type, so this is free |
| Available on which plans? | All tiers | Matches HTTP monitoring (PRD-006 gates by cap and interval, not by check type). Revisit only if a tier needs it withheld |
| Minimum period? | The plan interval floor applies to `period_seconds` | A Free org should not get 10-second heartbeat resolution any more than 10-second HTTP checks. Reuse `MonitorLimits.EffectiveIntervalFloor()` as the floor on `period_seconds`, enforced on write (api) like the HTTP interval is |

No new entitlement field. The existing monitor cap and interval floor cover it.

## 8. Frontend (RFC-013)

| Surface | Change |
|---------|--------|
| Create/edit form | a type selector (HTTP / heartbeat); choosing heartbeat swaps the URL+assertion fields for period + grace inputs |
| Ping URL | shown once on create with a copy button and a "store this now, you will not see it again" note (the API-key reveal pattern); a masked form plus a Rotate button afterward |
| Live chip | the dashboard chip reads the up/late/down marker from checkstate (section 5.4); "late" is a distinct color, "pending" reads "waiting for first ping" |
| Setup help | a short "paste this into your crontab" snippet next to the URL |

No new state model: the chip reuses the per-monitor live-state path, the reveal reuses the secret-once pattern.

## 9. Security of a public ingest

### 9.1 The token is the only guard

The endpoint is unauthenticated, so the token must be unguessable and cheap to verify. High-entropy (>= 32 random bytes), SHA-256 at rest, looked up by hash (constant-ish, indexed). No org id, monitor id, or slug in the URL, only the opaque token, so the URL leaks nothing about the tenant.

### 9.2 Rate limiting and abuse

A public POST that does a DB write needs a ceiling. Reuse the per-something rate-limit approach already in the codebase for check-now (a Redis counter, `429` with `Retry-After`): cap pings per token per window (a healthy job pings on a period of seconds-to-hours, so even a generous cap like a few per second per token is far above legitimate use and blunts a flood). An unknown token (404) is also counted per source IP so the endpoint cannot be used to brute-force tokens (9.3).

### 9.3 No enumeration

A bad token returns a bare `404` with no body and the same timing whether the token is malformed or simply not found, so an attacker cannot tell "close" from "wrong". Combined with the per-IP cap on 404s, guessing a 32-byte token is not feasible.

### 9.4 What a leaked token can do

Only mark one monitor as "alive". The blast radius of a leaked ping token is a single false-healthy monitor, never data access. Rotation (section 6) is the remedy. This is the same risk profile as a leaked Healthchecks.io URL and is acceptable for the feature.

## 10. Failure modes

| Failure | Behavior |
|---------|----------|
| Scheduler down (no sweep) | No down transitions fire while it is down; on restart the next tick catches every overdue monitor and emits then. Late detection, not lost detection. Same property the active-check dispatch already has |
| Two scheduler instances both sweep | The `down` edge marker is a row update; the open-incident unique index and watermark in alerting dedup any double-emit (5.2). Worst case is a duplicate result, swallowed downstream |
| Ping arrives during the same tick it goes down | Last write wins on `last_ping_at`; if the ping commits first the sweep sees it and does not mark down; if the down commits first the ping is a recovery and closes the just-opened incident. Either order converges to "up" within one tick |
| Clock skew between ingest and sweep | Both read `now()` from the same Postgres, not wall clocks on different hosts, so `last_ping_at` and the sweep comparison share a clock |
| `check.results` produce fails after the edge flip | The edge marker is already flipped, so a retry of the sweep will not re-emit. Mitigate by flipping `down` in the same path as the emit and treating a produce error as "leave `down` unchanged, retry next tick" (the emit and the flip must agree; see open question 12.2) |

## 11. Phasing

v1 is the simple success ping: one URL, ping = "I am alive", missing ping = down. That alone closes the competitive gap.

Fast-follow (not in this RFC's build):

| Later | What |
|-------|------|
| `/start` and `/fail` sub-pings | `POST /ping/{token}/start` then `/ping/{token}` measures job duration; `/ping/{token}/fail` signals an explicit failure without waiting for the grace window. Healthchecks.io's model |
| Job duration + run log | with start/finish pings, record and show how long each run took |
| Expected-schedule (cron expression) | instead of a fixed period, accept a cron expression so "every weekday at 02:00" is expressible, with the sweep computing the next expected time from the expression |

## 12. Open questions

1. **First-ping deadline.** A never-pinged heartbeat is pending forever (5.1). Do we want an optional "if no ping within N of creation, alert" so a never-deployed cron is caught? Leaning yes as a later opt-in field, no in v1.
2. **Emit/flip ordering on produce failure.** Section 10 wants the `down` flip and the result emit to agree. Options: emit-then-flip (risk: double-emit on retry, swallowed by alerting) vs flip-then-emit (risk: a dropped result means no incident until the next state change). Leaning emit-then-flip and trusting the alerting watermark, but flag for the build to confirm against RFC-006's exact idempotency guarantee.
3. **Period vs grace defaults.** What grace do we default to (a fraction of period? a fixed floor?) so a user who sets only a period gets sensible lateness tolerance.

## 13. Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-006 alerting | the entire down/recover/incident lifecycle, consumed unchanged via synthetic results |
| RFC-005 / RFC-002 `check.results` | the result shape and topic the sweep produces to |
| RFC-004 scheduler | the tick loop the sweep hangs off |
| RFC-001 | the monitor row, RLS pattern, `WithOrg`, and the definer-function precedent for the token lookup |
| RFC-012 | spec-first changes and `make gen` |
| `internal/billing/ingest.go` | the public hand-wired token-gated `http.Handler` pattern to model the ingest on |
| `internal/checkstate` | the live up/late/down chip the FE already reads |

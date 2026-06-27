# RFC-000 - Pulse Architecture Overview (Master)

Status: DRAFT for review
Author: Principal Architecture
Audience: every engineer who will write a sub-RFC or a service
Supersedes for runtime concerns: `docs/archive/ARCHITECTURE_v1_monolith.md` (the single-binary topology is obsolete; the package contracts it defined are reused, see section 14)
Product source of truth: `docs/PRD.md` and `docs/prd/PRD-001..007`. Where this RFC finds a product gap it is flagged in an "Open questions for product" box rather than decided here.

## 0. Purpose and how to read this

This is the single technical source of truth for Pulse. It is decision-complete on the cross-cutting concerns so the thirteen sub-RFCs (section 13) can be written in parallel against stable seams without contradicting each other. It does not duplicate the deep design of any one system; each section ends by naming the sub-RFC that owns the detail.

Every load-bearing choice below states the decision, the reasoning, and the rejected alternatives. The locked platform constraints (Go microservices in one module, PostgreSQL + Redis + Kafka, Prometheus/Grafana/OpenTelemetry, Docker/Kubernetes, Lit SPA behind nginx, Google/GitHub OIDC + RS256 JWT + per-org API keys, multi-tenant orgs with RBAC, multi-region probe fleet, tiered entitlements, reuse of the built Go packages) are taken as given and are not re-litigated.

Conventions used throughout: "control plane" = the home-region services and stores; "data plane" = the regional worker fleets that reach customer endpoints. "Org" is the tenant. All timestamps are RFC3339 UTC on the wire. Banned in this doc by house style: em-dashes.

---

## 1. System context and container view

Pulse splits into a single control plane in one home region and N regional data planes that only run workers. The control plane owns all state and all decisions; the data plane only executes HTTP checks and reports results back.

### 1.1 C4 container diagram (control plane vs regional data planes)

```
                                EXTERNAL
   +-------------+   +----------------------+   +--------------------------+
   | End users   |   | OAuth providers      |   | Notification targets     |
   | (browsers)  |   | Google OIDC / GitHub |   | Slack/Discord/webhook/   |
   +------+------+   +----------+-----------+   | SMTP (and customer URLs) |
          |  HTTPS              | OIDC          +-------------+------------+
          v                     |                             ^ outbound
   +------+---------------------v-----------------------------+--------------+
   |                       CONTROL PLANE (home region, k8s)                  |
   |                                                                        |
   |   +-----------+        +------------------------------------------+    |
   |   |  nginx    |  TLS   |               api service (HPA)          |    |
   |   | ingress + |------->|  SPA backend + public REST /api/v1       |    |
   |   | SPA static|        |  OAuth callback + JWKS (/.well-known)    |    |
   |   +-----------+        |  Stripe webhooks  +  Swagger UI /api/docs|    |
   |        ^               |  authn (JWT/keys) + authz (RBAC+entitle) |    |
   |        | static page   +----+--------+--------------+-------------+    |
   |        | serving (cached)   |reads/   |produce       |reads          |
   |   +----+-----------+        |writes   |monitor.changed|cache         |
   |   | status-page    |        v         v              v               |
   |   | serving (api   |   +----+----+  +--+-----+   +----+----+          |
   |   |  read path,    |   |Postgres |  | Kafka  |   |  Redis  |          |
   |   |  cache-first)  |   |(primary |  |(control|   |(cache + |          |
   |   +----------------+   | + read  |  | bus)   |   | locks + |          |
   |                        | replicas)| +-+----+-+   | rl + ent)|         |
   |                        +----+----+   | ^    |    +---------+          |
   |                             ^        | |    |                         |
   |   +--------------+  reads    |   prod | |cons| prod/cons               |
   |   | scheduler    |-----------+   ----+ |    +-----+                    |
   |   | (singleton,  | produce check.jobs.<region>    |                   |
   |   |  leader-     |---------------------+          |                   |
   |   |  elected)    |                     |          |                   |
   |   +--------------+                     |          |                   |
   |                                        |          v                   |
   |   +-----------+  consume check.results | +--------+--------+          |
   |   | alerting  |<-----------------------+ | notifier (HPA)  |          |
   |   | (HPA, per-|  produce notify.events   | consume notify  |          |
   |   |  monitor  |------------------------->| events, deliver |          |
   |   |  ordering)|  write incidents (PG)    | with retry      |          |
   |   +-----------+                          +--------+--------+          |
   |                                                   | outbound to       |
   |   +--------------------------------------------+  | targets           |
   |   | Observability: Prometheus scrape, Grafana, |  |                   |
   |   | OTel collector (traces), Loki/structured   |  |                   |
   |   | logs. Scrapes every service + infra.       |  |                   |
   |   +--------------------------------------------+  |                   |
   +-------------------+------------------------------+|-------------------+
                       | check.jobs.<region>          || check.results
                       | (across-region transport,    || (results flow back
                       |  see section 4)              ||  to control Kafka)
                       v                              v|
   +-------------------+------------------+   +-------+|------------------+
   | REGIONAL DATA PLANE: region eu-west  |   | REGIONAL DATA PLANE: ... |
   |  +-------------------------------+   |   |  (one per operated       |
   |  | worker fleet (HPA, stateless) |   |   |   region; same shape)    |
   |  |  consume check.jobs.eu-west   |   |   +--------------------------+
   |  |  run HTTP check + SSRF guard  |   |
   |  |  least-privilege egress       |   |
   |  |  emit check.results (region   |   |
   |  |   tagged) back to control     |   |
   |  |  emit region heartbeats       |   |
   |  +-------------------------------+   |
   +--------------------------------------+
```

### 1.2 What each external actor touches

| Actor | Enters through | Notes |
|-------|----------------|-------|
| Browser (SPA user) | nginx -> api | JWT in Authorization header (or httpOnly cookie carrying it); SPA static assets served by nginx directly |
| API client (key) | nginx -> api `/api/v1` | per-org API key, rate-limited per key |
| Status page viewer | nginx -> status-page read path | public, unauthenticated, cache-first, must stay up during customer incidents |
| Google / GitHub | api OAuth callback + JWKS | api is the only service that talks to OIDC providers |
| Stripe | api webhook endpoint | signature-verified; api is the only Stripe integration point |
| Customer endpoints / Slack / Discord / SMTP | outbound only, from workers (checks) and notifier (alerts) | never inbound |

The hard split: nothing in a regional data plane holds durable state or makes a product decision. A region runs HTTP checks and emits results. If a whole region disappears, the control plane keeps all state and simply sees missing results, which probe-fleet health turns into coverage-degraded rather than a customer incident (PRD 6.7).

---

## 2. Service catalog

Five services, one shared module. Each is a separate `cmd/<service>` binary built from the same `go.mod` so they share `internal/` packages and stay version-locked.

### 2.1 api

| Aspect | Decision |
|--------|----------|
| Responsibility | SPA backend + public REST `/api/v1`; authn (OAuth/OIDC login, JWT issue+verify, API key verify); authz (RBAC + entitlement checks on write); CRUD for all org resources; read paths for status/history/incidents; Swagger UI at `/api/docs`; OAuth callback + JWKS at `/.well-known/jwks.json`; Stripe webhooks; status-page read serving |
| Statefulness | Stateless. All state in Postgres/Redis. Holds the RS256 signing private key in memory (from a mounted secret/KMS) and publishes the public key via JWKS |
| Scaling | HPA on CPU and on request concurrency (in-flight requests / p95 latency via a custom metric). Read replicas absorb read-heavy traffic |
| Reads | Postgres (primary for writes, replicas for reads), Redis (entitlement cache, rate-limit counters, JWKS/session revocation, status-page cache) |
| Writes | Postgres (all org resources, incidents-via-read-only here, audit events); Kafka (`monitor.changed`, `audit.events`, `billing.events`); Redis (cache fills, rate-limit increments) |
| Failure behavior | Stateless, so a pod loss is invisible behind the HPA + load balancer. If Postgres primary is down, writes fail closed with 5xx and reads fall to replicas (stale-tolerant). If Redis is down, entitlement and rate-limit checks fail safe per section 12 (fail-closed on entitlement writes, fail-open with a conservative default on rate limit) |

OAuth callback, JWKS, and Stripe webhooks all live in api on purpose. They are request/response HTTP surfaces that need the same TLS, routing, and secret access api already has, and splitting them into a separate identity or billing service would add a network hop and a second JWT-signing key holder for no isolation benefit at this scale. RFC-003 owns auth detail, PRD-006/RFC-009 own billing.

### 2.2 scheduler

| Aspect | Decision |
|--------|----------|
| Responsibility | Owns the schedule for every org's monitors. Decides which checks are due, fans out one check job per (monitor, selected region), enforces the org's interval floor and region entitlement on dispatch (independent of api), publishes jobs to Kafka keyed for per-monitor ordering. Rebuilds its schedule from Postgres on start. Consumes `monitor.changed` to pick up live edits |
| Statefulness | Stateful in memory only (an in-memory schedule / next-run heap), but that state is fully derived from Postgres and rebuildable on boot. Runs as a singleton via leader election (section 11) so two replicas never double-dispatch |
| Scaling | Does not horizontally scale for throughput; it is a single active leader with warm standbys. Dispatch is cheap (publish to Kafka); the work is in the workers. If one scheduler ever cannot keep up, we shard by org-hash across a small fixed set of leaders before we make it stateless. Not needed at 500k monitors / 10k checks per second because publishing is light |
| Reads | Postgres (monitors, enabled set, entitlements snapshot on boot), Redis (entitlement cache on the hot dispatch path), Kafka (`monitor.changed`) |
| Writes | Kafka (`check.jobs.<region>`); Redis (per-monitor "check now" coordination lock, dispatch dedup keys) |
| Failure behavior | If the leader dies, a standby acquires the lease (section 11) and rebuilds the heap from Postgres within the lease timeout. A brief gap delays some checks by seconds, within the 5s scheduling-accuracy SLO budget after recovery. Because next-run is recomputed from stored interval and last dispatch, no checks are permanently lost |

### 2.3 worker

| Aspect | Decision |
|--------|----------|
| Responsibility | Stateless check executor. Consumes `check.jobs.<region>` for its own region, runs the HTTP request with the configured timeout, applies SSRF resolution-time validation, applies all assertions in PRD 6.3 priority order, emits `check.results` (region-tagged), and does not write Postgres itself; a control-plane consumer does the durable `check_results` upsert. Emits per-region heartbeats for probe-fleet health |
| Statefulness | Fully stateless. Reuses `internal/checker` unchanged |
| Scaling | HPA on Kafka consumer lag for the region's `check.jobs` topic (primary signal) plus CPU. Lag is the right signal because it directly tracks "checks waiting to run" |
| Reads | Kafka (`check.jobs.<region>`). Monitor config travels inside the job payload so workers do not read Postgres at all (see section 5) |
| Writes | Kafka (`check.results`, region heartbeats), Redis (per-monitor "check now" lock acquire/release). No Postgres write; the control-plane consumer does the durable `check_results` upsert |
| Failure behavior | A worker crash mid-check leaves the job unacked; Kafka redelivers it to another worker. The idempotent upsert by the control-plane consumer (section 5) makes redelivery safe. If a whole region's workers are down, no results flow for that region; the control plane sees missing results and the alerting aggregation excludes that region (coverage-degraded), never pages |

Decision: workers emit `check.results` to their regional Kafka only and do not write Postgres. A single control-plane consumer does the idempotent `check_results` upsert (keyed `(org_id, monitor_id, region, checked_at)`) and applies the verdict in the same transaction (RFC-006 section 5.4). This means the durable row and the alerting trigger share one write path and cannot diverge under partial failure, and the worker stays fully off the Postgres path. Alternative considered: have the worker write the row directly and emit the event after. Rejected because two write paths give the durable result and the alerting trigger two different failure modes. RFC-005 owns the worker, RFC-006 the control-plane consumer, RFC-008 the regional aspect.

### 2.4 alerting

| Aspect | Decision |
|--------|----------|
| Responsibility | Consumes `check.results`, reduces per-region results to one monitor verdict via `down_policy` + probe-fleet health (PRD 6.7), runs the reused per-monitor state machine (`internal/alerting`), persists incident open/close and alert counters, emits `notify.events`. Owns one-down/one-up dedup and per-monitor ordering correctness |
| Statefulness | Logically stateless; all alert state (consecutive fails, first-fail time, open incident) lives in Postgres exactly as v1 did. In-flight per-partition ordering state is Kafka's, not the service's |
| Scaling | HPA on `check.results` consumer lag. Scales by adding consumers within the consumer group; per-monitor ordering is preserved because results are partitioned by monitor id (section 5) so one monitor is always handled by one consumer at a time |
| Reads | Postgres (alert state, open incident), Kafka (`check.results`), Redis (per-region health for the verdict reduction; short-lived multi-region result aggregation window) |
| Writes | Postgres (incidents, alert counters), Kafka (`notify.events`), Postgres/Kafka (`audit.events` for manual-close interactions handled in api, not here) |
| Failure behavior | A crash leaves the result event unacked; redelivery re-runs the verdict. The state machine is made idempotent under redelivery (section 8) so a re-processed result does not double-open or double-close an incident or double-fire a notification |

### 2.5 notifier

| Aspect | Decision |
|--------|----------|
| Responsibility | Consumes `notify.events`, loads the attached channels, delivers to Slack/Discord/webhook/SMTP with retry/backoff (reuses `internal/notify`), records delivery outcome (incident timeline + audit/log). Also delivers org-level outbound webhooks (PRD 9) |
| Statefulness | Stateless. Retry state is per-attempt in-process; a give-up is recorded durably |
| Scaling | HPA on `notify.events` consumer lag |
| Reads | Kafka (`notify.events`), Postgres (channel config decrypted via `internal/crypto`, attached-channel list), Redis (notification dedup id cache) |
| Writes | outbound HTTP/SMTP to targets; Postgres (delivery outcome / failure marker); Redis (dedup id set) |
| Failure behavior | At-least-once delivery. A redelivered `notify.event` is recognized by its dedup id (section 5) so a duplicate is suppressed within the dedup window. A target being down is retried then recorded as failed, visible in the UI; it does not block other channels |

### 2.6 Entitlements: library, not a service (decision)

Decision: entitlements is a shared `internal/entitlements` library used by api (enforce on write) and scheduler (enforce on dispatch), backed by a Redis cache with Postgres as source of truth. It is not a standalone service.

Reasoning:
- The two enforcement points (api on write, scheduler on dispatch, PRD 11) both need a fast "what is org X allowed" lookup on a hot path. A library call against a Redis-cached snapshot is sub-millisecond; a network call to an entitlements service adds a hop and a new failure mode to the two most latency-sensitive paths.
- Entitlement data is small and changes rarely (only on plan change), which is the ideal shape for a cache-with-invalidation library, not a service.
- Keeping it a library avoids over-fragmentation. We have five services already; a sixth that does one cached read would be ceremony.

Rejected alternative - entitlements as a service: justified only if entitlement logic needed independent deploy cadence or held its own large dataset. Neither holds. If a future need appears (for example a complex usage-metering engine), the library's interface stays the same and we can put a service behind it without changing callers. RFC-009 owns the model; the cache invalidation contract is in section 12.

### 2.7 Service summary

| Service | Stateful? | Scale signal | Kafka role | Singleton? |
|---------|-----------|--------------|-----------|-----------|
| api | no | CPU + in-flight/p95 | produce monitor.changed, audit, billing | no |
| scheduler | derived only | n/a (singleton) | produce check.jobs.<region>; consume monitor.changed | yes (leader-elected) |
| worker | no | check.jobs lag + CPU | consume check.jobs.<region>; produce check.results | no |
| alerting | no (state in PG) | check.results lag | consume check.results; produce notify.events | no |
| notifier | no | notify.events lag | consume notify.events | no |

---

## 3. Repository and module layout

One Go module, one `go.mod`, multiple binaries, shared `internal/`. This keeps the reused packages shared without versioning friction and lets a single PR change a contract and all its consumers together.

```
pulse/
  go.mod  go.sum                       # one module: module pulse
  Makefile
  cmd/
    api/main.go                        # api service
    scheduler/main.go                  # scheduler service (leader-elected singleton)
    worker/main.go                     # worker service (regional)
    alerting/main.go                   # alerting service
    notifier/main.go                   # notifier service
  internal/
    domain/                            # REUSED, extended: + OrgID, Region, new entities
    crypto/                            # REUSED unchanged: AES-256-GCM secret encryption
    checker/                           # REUSED unchanged: HTTP check + SSRF guard
    alerting/                          # REUSED unchanged: pure state machine
    notify/                            # REUSED unchanged: channel delivery + retry
    store/                             # REPLACED: Postgres impl of the Store interface (was SQLite)
    bus/                               # NEW: Kafka producer/consumer wrappers, topic+key helpers
    redis/                             # NEW: cache, locks, rate-limit, dedup helpers
    authn/                             # NEW: OIDC login, JWT (RS256) issue/verify, JWKS, API keys
    authz/                             # NEW: RBAC role matrix evaluation + request scoping
    entitlements/                      # NEW: plan-limits model + Redis-cached lookup (library)
    region/                            # NEW: region registry, probe-fleet health, verdict reduction
    obs/                               # NEW: metrics (Prometheus), slog setup, OTel tracing wiring
    config/                            # REUSED, extended: per-service env config
  web/                                 # REUSED: Lit + Vite + TS SPA (served by nginx, not embedded)
  deploy/
    docker/                            # one Dockerfile per service + nginx image
    helm/                              # Helm charts (per service + umbrella)
    terraform/                         # managed Postgres/Redis/Kafka, k8s, DNS/TLS
  api/openapi/                         # OpenAPI 3 spec (single source of truth, RFC-012)
```

Notes:
- `domain` still imports nothing from other internal packages. It stays the shared vocabulary; the new fields (`OrgID`, `Region`) are additive.
- The frontend is no longer embedded in a Go binary. nginx serves the built SPA static assets and proxies `/api` to the api service. This decouples frontend deploys from backend and lets nginx cache and serve status pages resiliently (PRD 12 availability note).
- Per-service Dockerfiles are thin: a multi-stage build that compiles one `cmd/<service>` with `CGO_ENABLED=0` into a distroless/static image. The nginx image is a separate stage that takes `web/dist`.

RFC-013 owns the frontend, RFC-011 owns Dockerfiles/Helm/Terraform.

---

## 4. Topology and multi-region

### 4.1 The split

The control plane (api, scheduler, alerting, notifier, Postgres, Redis, control Kafka, observability) runs in one home region. Each operated region runs only a worker fleet plus the regional side of the messaging transport. A check job is born in the scheduler, tagged with its target region, delivered to that region's worker fleet, executed there, and its result flows back to the control plane where all aggregation (down policy, uptime, status pages, incidents) happens against the one Postgres source of truth.

This is exactly the PRD 6.6 control-plane / regional-data-plane model. The deep design (failover, cost-aware scheduling, region health detail) is RFC-008. This section sets the one load-bearing decision: the cross-region messaging shape.

One verdict rule is locked at the product layer and binds RFC-006/RFC-008: the down-policy denominator is the set of healthy-reporting regions `R` (selected regions minus heartbeat-unhealthy ones), not all selected regions `S`. `quorum` needs a strict majority of `R` unhealthy, `any` needs at least one of `R`, `all` needs all of `R`. A heartbeat-unhealthy region is excluded, never counted as an implicit healthy vote, and when `R` falls too low to decide (PRD-007: `|R| < 2` for quorum, `|R| = 0` for any/all) the monitor goes coverage-degraded instead of down. This is why probe-fleet health (`region.health`) must be available to alerting before it reduces the verdict.

### 4.2 Cross-region messaging shape (decision)

Decision: regional Kafka clusters per data-plane region, with `check.jobs.<region>` produced into the regional cluster and `check.results` mirrored back to the central control-plane cluster. The control plane writes a region's jobs into that region's cluster; the regional workers consume locally; results are mirrored (MirrorMaker 2 or the managed equivalent) from each regional cluster back to the central cluster where alerting consumes them.

Reasoning:
- Workers must keep running and keep draining their job queue even if the link to the home region is briefly slow. A regional cluster gives the data plane a local broker so consume is always local and low-latency, and a partition between region and home does not stall in-region checking.
- Results tolerate a short mirror delay (the 5s decision-latency SLO is comfortably met by intra-cloud mirroring) while jobs need to be delivered close to the workers that run them.
- It contains blast radius: a regional broker incident does not take down the central bus that api/alerting/notifier depend on.

Rejected alternatives:

| Option | Why rejected |
|--------|--------------|
| Single central Kafka, region-keyed partitions, workers consume cross-region | Every worker poll crosses the region boundary. Cross-region consume is latency- and egress-heavy and couples in-region checking to the health of the home-region brokers. A home-region network blip would stall checking everywhere |
| Region-scoped topics on one central cluster (`check.jobs.eu-west` etc.) consumed remotely | Same cross-region consume problem as above; topic naming alone does not move the broker closer to the worker |
| Full active-active Kafka mesh | Over-built for the control-plane-in-one-region model; operational cost not justified until multi-region control plane (a later phase) |

Cost and egress trade-off (stated, owned by RFC-008): mirroring `check.results` home is cross-region egress, paid per region. It is bounded by result volume (one small row per check per region), and we keep it cheap by mirroring only `check.results` and region heartbeats home, not the full job stream. Premium regions are an entitlement on higher tiers (PRD 11) so we are not paying premium-region egress on free traffic. At Phase 0 and Phase 1 there is one region (home), so there is no mirror and no egress; the topic-per-region naming and the mirror seam exist from day one so the GA multi-region rollout is additive, never a migration (PRD 12 multi-region posture).

---

## 5. Eventing model (Kafka)

This sets the contract RFC-002 details. The rules here are binding on every producer and consumer.

### 5.1 Canonical topics

| Topic | Producer | Consumer | Partition key | Purpose |
|-------|----------|----------|---------------|---------|
| `monitor.changed` | api | scheduler | org_id | live schedule updates (create/edit/enable/disable/delete) |
| `check.jobs.<region>` | scheduler | worker fleet (that region) | monitor_id | a check to run, one per (monitor, region) per tick |
| `check.results` | worker | alerting | monitor_id | the outcome of one check, region-tagged |
| `notify.events` | alerting | notifier | monitor_id (or incident_id) | a down/recovery event to deliver |
| `audit.events` | api (and any service taking an auditable action) | audit sink / api read path | org_id | append-only audit trail (PRD 13) |
| `billing.events` | api (Stripe webhook handler) | api billing consumer / entitlement invalidator | org_id | plan changes, payment events; drive entitlement cache invalidation |
| `region.health` | worker (heartbeats), region controller | alerting, scheduler | region | probe-fleet liveness feeding coverage-degraded and failover (RFC-008) |

### 5.2 Partition-key strategy

- `check.results`, `check.jobs`, `notify.events` are keyed by `monitor_id`. This is the load-bearing choice: it gives per-monitor ordering. All results for a monitor land on one partition in arrival order, so alerting processes a monitor's checks in order and the state machine sees a coherent sequence. Cross-monitor parallelism is unbounded (key space is huge); per-monitor serialization is exactly what the state machine needs.
- `monitor.changed`, `audit.events`, `billing.events` are keyed by `org_id` so an org's events stay ordered relative to each other (a create-then-edit cannot reorder).
- `region.health` is keyed by `region`.

### 5.3 Delivery semantics and idempotency

Delivery is at-least-once everywhere. Exactly-once is achieved in effect through idempotent consumers, not Kafka transactions, because transactions add coordination cost and our consumers can be made naturally idempotent:

| Consumer | Redelivery hazard | Idempotency rule |
|----------|-------------------|------------------|
| worker (check.jobs) | run the same check twice | the worker only emits to Kafka; the scheduler stamps `checked_at`/a job id so a redelivered job re-emits the same `check.results` event, which the control-plane consumer dedups on `(org_id, monitor_id, region, checked_at)` |
| alerting / control-plane consumer (check.results) | re-apply the same result, double-open/close | the durable `check_results` upsert is keyed by `(org_id, monitor_id, region, checked_at)` with a unique constraint, so a redelivered result upserts the same row (no-op); in the same transaction the state machine transition is made idempotent: an incident open is conditioned on "no open incident for this monitor" (the partial unique index already enforces one open incident), a close is conditioned on the incident still being open, and alert-counter updates are keyed to the triggering result id so reprocessing the same result is a no-op (section 8) |
| notifier (notify.events) | deliver the same alert twice | each notify event carries a stable dedup id = `hash(incident_id, event_type)` (one down, one up per incident, PRD 6.4). notifier records delivered dedup ids in Redis (and a Postgres backstop); a duplicate is suppressed |

The contract every producer must honor: include a stable idempotency key in the event so the consumer can dedup. The contract every consumer must honor: be safe to run twice on the same event. RFC-002 specifies the exact event schemas and key fields.

Note the API has its own request-level idempotency, separate from Kafka's: the public API accepts an optional `Idempotency-Key` header on unsafe writes, and api remembers the key and its response for at least 24 hours so a client retry returns the original result rather than creating a duplicate (PRD-005). That key space (HTTP write dedup) is distinct from the event dedup ids above (consumer redelivery safety); RFC-012 owns the HTTP one, RFC-002 the event one.

---

## 6. Data and multi-tenancy strategy

### 6.0 Entity inventory (what RFC-001 must model)

The schema spans all seven sub-PRDs. Every org-owned entity carries `org_id`. The set, with the lifecycle states that matter for the architecture:

| Entity | Owner | Notable lifecycle / fields |
|--------|-------|----------------------------|
| User | global (not org-scoped) | status active -> deletion-pending -> deleted; `primary_email` (verified) |
| UserIdentity | user | `provider` (google/github), `provider_user_id`, `email_verified`; account linking on verified-email match |
| Organization | tenant root | status active -> deletion-pending (14-day grace, locked) -> deleted; `slug` (unique, shapes status-page URL), `kind` (personal/team), `plan_id` |
| Membership | org | `role` (owner/admin/member/viewer), `seat_id`; invariant: always >=1 owner |
| Seat | org | occupancy accepted-member / reserved-invite / free; seat count = accepted members + pending invites (locked) |
| Invitation | org | state pending (7-day expiry) -> accepted/revoked/expired; signed-in email must match on accept |
| Monitor | org | + `region` list and `down_policy` from day 0; reuses v1 fields |
| CheckResult | org | + `region`; time-range partitioned (6.2) |
| Incident | org | `started_at` (first-fail of run), `close_reason` recovered/disabled/manual, `closed_by` |
| Channel | org | secret config encrypted; `*_set` redaction on read |
| StatusPage | org | `slug` (unique per org), draft/published, display-monitor list |
| ApiKey | org | role member/admin only, stored as hash, `prefix`, last-used |
| Plan / Subscription / Entitlement | org | plan catalog + concrete per-org allowances (RFC-009) |
| Region | global catalog | `code`, premium flag, cost_class, lifecycle available/deprecated/retired, health |
| AuditEvent | org | append-only; actor, action, target, when, from-where |
| OutboundWebhook | org | endpoint + signing secret (RFC-007) |

The User entity is the one global (cross-org) row; everything else is org-scoped and falls under the isolation rules below. RFC-001 owns the column-level schema.

### 6.1 Tenancy isolation model (decision)

Decision: a single shared PostgreSQL database with mandatory `org_id` row scoping enforced at a repository layer, with PostgreSQL Row-Level Security (RLS) enabled on every tenant table as defense in depth.

The hard invariant (PRD 13): a user or key from org A can never read or affect org B's data, under any endpoint, ever. Enforcement is layered so a single missed `WHERE org_id = ?` cannot leak data:

1. Application layer: the repository takes the caller's org context and every tenant query is scoped by `org_id`. No handler builds raw SQL; all tenant access goes through repository methods that require an org-scoped context.
2. Database layer: RLS policies on every tenant table key off a session variable (`SET LOCAL app.current_org`) set per transaction from the authenticated org. Even a buggy query without an explicit org filter returns only the current org's rows. RLS is the backstop that makes a missed filter fail safe instead of leaking.

How it is tested (binding requirement on RFC-001): a cross-tenant test suite that, for every tenant-scoped repository method, asserts org A's credentials cannot read, list, update, or delete org B's rows, and that RLS blocks a deliberately org-unfiltered query. This suite must pass before any release.

Reasoning and rejected alternatives at 50k orgs:

| Model | Verdict | Why |
|-------|---------|-----|
| Shared DB + row org_id + RLS (chosen) | chosen | One schema, one migration path, one connection pool. Scales to 50k orgs trivially (org_id is just an indexed column). RLS gives defense in depth without per-tenant operational cost. Cross-org joins for our own analytics stay possible |
| Schema-per-tenant | rejected | 50k schemas means 50k copies of every table and index, migration runs across 50k schemas, and Postgres catalog bloat. Connection/prepared-statement management gets ugly. The isolation win is real but row scoping + RLS already gives us the invariant |
| Database-per-tenant | rejected | 50k databases is operationally absurd at SMB scale and pricing. Reserved for a future enterprise data-residency tier (PRD 15) where a specific large customer pays for physical isolation; that is an exception, not the model |

### 6.2 High-volume check_results

check_results is the firehose (PRD scale: ~10k inserts/sec sustained, 500k monitors). Decision:

- Time-based range partitioning of `check_results` by `checked_at` (for example daily or weekly partitions). New data lands in the current partition; retention cleanup (PRD: 7/30/90/180 days per plan) becomes a partition DROP instead of a mass DELETE, which is orders of magnitude cheaper and avoids vacuum churn.
- Hourly rollups (per monitor, per region) computed by a background job into a `check_rollups` table, so uptime math and history charts read aggregates instead of raw rows. Raw rows age out by partition drop; rollups persist for the uptime window shown on status pages (PRD 12). This keeps status-page and history reads fast at retention scale.
- Read replicas absorb the read-heavy paths (history, status pages, dashboards). Writes go to the primary. The api read path and status-page serving prefer replicas and tolerate small replica lag (eventually consistent reads, section 8).

### 6.3 Migrations

Decision: a forward-only versioned SQL migration set applied by an init job before rolling out new service versions, with a `schema_migrations` table tracking applied versions. This carries forward the v1 hand-rolled approach in spirit but runs as a Kubernetes pre-deploy job rather than at service start, so five services do not race to migrate. RFC-001 picks the exact tool (the choice is between the same minimal embedded runner and a standard library such as golang-migrate; the deciding factor is partition management and RLS policy DDL, which RFC-001 owns).

RFC-001 owns the full schema, partition mechanics, RLS policies, backup/restore, and the cross-tenant test suite.

---

## 7. Identity propagation and service-to-service trust

### 7.1 Who verifies identity

Decision: api is the only service that authenticates external principals. It verifies the OIDC login, issues the RS256 JWT, verifies JWTs and API keys on incoming requests, and resolves the request to an `(org_id, user_id_or_key_id, role)` tuple. Internal services do not re-verify user JWTs.

Internal events carry org_id and the acting principal as data, not as a token to re-verify. When api publishes `monitor.changed`, the event body includes `org_id` and the actor; scheduler/alerting/notifier trust that data because it came from api over an authenticated internal channel. This is the standard "authenticate at the edge, propagate identity as data inward" model. Workers and alerting never make an authorization decision about a user; they act on already-authorized work.

Reasoning: re-verifying a user JWT at every internal hop buys nothing once the edge has authorized the action and recorded org_id on the work item, and it would force every service to hold JWKS and handle token expiry mid-pipeline (a check job that outlives its triggering token must still run). Authorizing once at the edge and carrying org_id as immutable data on the event is simpler and correct.

JWKS: api publishes its public key at `/.well-known/jwks.json`. This exists so external clients and the SPA can verify tokens if needed and so a future service that does need to verify a user token (none in v1) can fetch the key. RS256 (asymmetric) is chosen over HS256 specifically so the private signing key lives only in api and verification never requires sharing a secret. ADR candidate (section 15).

### 7.2 Service-to-service trust (v1 stance)

Decision for v1: rely on Kubernetes NetworkPolicy to restrict which services can reach which (api and the Kafka/Postgres/Redis endpoints, workers only to Kafka and outbound), plus TLS on every infra connection (Postgres, Redis, Kafka all TLS), and treat the cluster network as the trust boundary. We do not deploy a service mesh with mTLS in v1.

Reasoning: the internal call graph is small and mostly goes through Kafka, not direct service-to-service HTTP. NetworkPolicy + TLS-to-infra covers the realistic threat (a compromised pod reaching a store it should not) without the operational weight of a mesh. Rejected alternative - Istio/Linkerd mTLS from day one: real benefit (pod-to-pod identity) but heavy for five services that barely call each other directly; revisited when the service count or compliance posture (SOC 2, PRD 13) demands cryptographic pod identity. The decision is recorded so RFC-011 provisions NetworkPolicies and TLS, and so the mesh question has a clear "later, when X" trigger.

### 7.3 Where authz is evaluated

- RBAC (the PRD 4 role matrix) is evaluated in api, in `internal/authz`, against the caller's active-org membership role, on every request. Read it as: api maps request -> required capability -> role check.
- Entitlement checks (PRD 11) happen in api on write and in scheduler on dispatch, via `internal/entitlements` (section 2.6, 12).
- No authz happens in worker/alerting/notifier; they act on pre-authorized work.

RFC-003 owns OIDC flow, token lifetimes, refresh, key rotation, API key format and hashing, and revocation. Constraints from the PRDs that bind RFC-003: sessions persist across restarts and work multi-device, with revocation (logout, role change, removal) taking effect within the access-token refresh window, implemented as short-lived access JWT + longer refresh. API keys are bearer tokens (`Authorization: Bearer pulse_sk_<secret>`), shown once, stored as a hash, carry a non-secret prefix and a last-used timestamp, are role-scoped to member or admin only (never owner, so billing/ownership cannot be automated by a leaked key), and revoke immediately. Account linking auto-links only on a matching verified email and a manual link requires an active session (cannot attach an identity to a stranger's account). Outbound webhook signing uses `X-Pulse-Signature: t=<ts>,v1=<hmac>` over timestamp + raw body with a per-webhook secret and a replay window. These surfaces are owned by RFC-003/RFC-007/RFC-012; this RFC just fixes that they live in api.

---

## 8. Consistency, ordering, idempotency

| Concern | Guarantee | Mechanism |
|---------|-----------|-----------|
| Monitor/channel/incident/billing writes | strongly consistent | single Postgres primary, transactional writes |
| Per-monitor result ordering | ordered per monitor | Kafka partitioning by monitor_id (section 5.2); one consumer per partition handles a monitor's results in sequence |
| Alert state machine under redelivery | exactly-once in effect | idempotent transitions (below) |
| Status caches, dashboards, history reads | eventually consistent | Redis cache + read replicas; bounded staleness acceptable per PRD (status is a derived view) |
| Cross-region aggregation | eventually consistent | results mirror home, alerting aggregates within a short window; the down-policy verdict tolerates results arriving slightly apart |

The critical case is the alerting state machine under redelivery. The reused `internal/alerting.Apply` is already a pure function that takes (monitor, result, current state) and returns a Decision; it reads no clock and does no I/O. That purity is what makes idempotency tractable. The distributed wrapper around it must:

1. Read current alert state and any open incident from Postgres in the same transaction it will write.
2. Apply the pure decision.
3. Persist conditionally: open-incident only if no open incident exists (enforced by the partial unique index `uniq_open_incident WHERE ended_at IS NULL` carried from v1), close-incident only if the incident is still open, and write the new alert counters keyed/guarded by the triggering result id so reprocessing the same result id is a no-op.
4. Emit the notify event with a stable dedup id derived from `(incident_id, event_type)`.

So a redelivered `check.results` event re-runs the pure function and lands on the same conditional writes, which are no-ops the second time, and re-emits a notify event with the same dedup id, which notifier suppresses. One down, one up per incident holds under redelivery. RFC-006 owns the distributed alerting design; this is the binding correctness contract.

---

## 9. Observability

Three pillars, set as standards here; RFC-010 owns the detail (SLIs/SLOs, error budgets, dashboards, alerting on ourselves).

### 9.1 Metrics (Prometheus)

Every service exposes `/metrics`. Standard per-service SLIs:

| Service | Key SLIs |
|---------|----------|
| api | request rate, error rate (4xx/5xx split), p50/p95/p99 latency per route class (read vs write), entitlement-cache hit ratio, rate-limit rejections |
| scheduler | scheduling lateness (dispatched_at - scheduled_at) histogram, jobs published/sec per region, leader-election state, schedule size |
| worker | check.jobs consumer lag, checks/sec, check duration histogram, SSRF blocks, result-emit failures, per-region health |
| alerting | check.results lag, verdict latency (result -> decision), incidents opened/closed, redelivery/no-op count |
| notifier | notify.events lag, delivery latency per channel type, delivery success/failure, dedup suppressions |

### 9.2 Logging

Structured logging with `log/slog` (already the codebase standard). Every log line carries a correlation/trace id. The trace id is propagated through Kafka by stamping it into a message header on produce and restoring it into the consumer's context and logger on consume, so one check can be followed scheduler -> worker -> alerting -> notifier across service and region boundaries. Secrets are never logged (reused redaction discipline).

### 9.3 Tracing (OpenTelemetry)

OTel spans across the pipeline, with context propagated over Kafka headers (same mechanism as the correlation id). A trace for one check spans the scheduler dispatch, the cross-region transport, the worker execution, the alerting decision, and the notifier delivery. An OTel collector in the control plane exports to the tracing backend.

### 9.4 SLOs (from PRD 12) and how measured

| SLO | Target | Measured by |
|-----|--------|-------------|
| Scheduling accuracy | dispatch within 5s of schedule, p99 | scheduler lateness histogram |
| Result-to-decision | alert state updated within 5s of result, p99 | alerting verdict-latency histogram (event timestamp to decision) |
| Notification delivery | sent within 30s of triggering check, p99 (excl. third-party) | end-to-end span from check.checked_at to notifier send |
| API latency | reads p99 < 300ms, writes p99 < 500ms | api per-route-class latency histograms |
| Control-plane + pipeline availability | 99.9% monthly | synthetic checks of api + the check/alert pipeline (we monitor ourselves, PRD 14) |

---

## 10. Security architecture (cross-cutting)

Summary here; the security deep design and SSRF specifics are RFC-005 (worker/SSRF) and the deployment/security RFC (RFC-011 plus a dedicated security pass).

| Concern | Decision |
|---------|----------|
| Tenant isolation | shared DB + repository org scoping + Postgres RLS (section 6.1); cross-tenant test suite that must pass before any release |
| Secret encryption | reuse `internal/crypto` AES-256-GCM, per-value nonce, for channel secrets, secret headers, API key material is stored as a hash not encrypted. The 32-byte key comes from a Kubernetes secret sourced from a cloud KMS/secret manager (Terraform-provisioned), not an env var baked into an image. Key rotation is an RFC-011 concern; the crypto contract (LoadKey, Encrypt, Decrypt) is unchanged |
| SSRF | on by default, not customer-disableable (PRD 13). Resolution-time validation reused from `internal/checker/ssrf.go`: pre-resolve and refuse loopback/link-local/cloud-metadata/RFC1918, plus the dialer `Control` callback that re-checks the connected IP to beat DNS rebinding, with redirects re-validated per hop. Regional workers additionally run least-privilege with network egress controls so a bypass still cannot reach internal services (PRD 13) |
| Rate limiting | per-API-key, plan-tiered (PRD 9, 11), counters in Redis, standard `X-RateLimit-*` headers and 429 + Retry-After. Enforced in api |
| Audit logging | per-org append-only audit trail for sensitive actions (PRD 13 list), emitted as `audit.events` and stored in Postgres; visible to owner/admin |
| Encryption in transit | TLS everywhere: client->nginx, nginx->api, and every service->infra (Postgres/Redis/Kafka) |
| Data export/delete | GDPR export and 14-day-grace org deletion (PRD 13, locked in consistency review) handled in api against Postgres |

Open question for product is captured in section 13's RFC-001 box if any tenancy edge needs a product call; none blocks this RFC.

---

## 11. Deployment and environments

Decisions here; RFC-011 owns the full design (DR, capacity, CI/CD detail).

### 11.1 Kubernetes layout

- Namespaces: `pulse-system` (shared infra clients, observability), `pulse-control` (api, scheduler, alerting, notifier in the home region), and one `pulse-region-<code>` per data-plane region (workers only). Environments are separate clusters or at least separate namespaces per env (dev/staging/prod), decided in RFC-011 toward separate clusters for prod isolation.
- Ingress/TLS: nginx ingress terminates TLS, serves SPA static assets, proxies `/api`, and serves status pages (cache-first). Wildcard TLS for `{org-slug}.pulse.app` status-page subdomains (PRD 8 decision); custom-domain status pages (phased) bring managed per-domain certs.
- HPA targets: api on CPU + in-flight/p95; worker/alerting/notifier on their respective Kafka consumer lag; scheduler not horizontally scaled (singleton).

### 11.2 Scheduler leader election (decision)

Decision: Kubernetes Lease-based leader election via `client-go`'s `leaderelection` package (the lease-lock backed by a `coordination.k8s.io/Lease` object).

Reasoning:
- The scheduler already runs in Kubernetes, so the Lease API is right there with no new dependency. `client-go` leaderelection is the well-trodden, battle-tested path for "exactly one active replica" in Kubernetes; the renew/acquire/observe loop and failover semantics are handled for us.
- It avoids making Redis a correctness dependency for singleton-ness. If we used a Redis lock, a Redis blip could cause either two leaders (split brain) or none; the k8s Lease ties leadership to the same control plane that schedules the pods.

Rejected alternative - Redis lock (SET NX PX + renewal): viable and we already run Redis, but it puts the singleton guarantee on a cache that we otherwise treat as fail-open, and red-lock-style correctness under failover is fiddly. We keep Redis for coordination that tolerates a brief lapse (check-now locks, dedup), not for the one guarantee that must never split. ADR candidate (section 15).

### 11.3 Managed vs self-run infra (stance)

Decision: managed PostgreSQL, Redis, and Kafka (cloud-managed offerings) for v1. Reasoning: the 99.9% SLA (PRD 12) is reachable with managed redundancy without us running stateful clustering. We are a small team selling monitoring, not running a Kafka platform. Self-hosting these is revisited only if cost or a residency requirement forces it. RFC-011 picks vendors and sizes.

### 11.4 CI/CD and IaC

- IaC: Terraform for cloud resources (managed Postgres/Redis/Kafka, Kubernetes, DNS, TLS, KMS) and Helm for in-cluster workloads (per-service charts under an umbrella chart).
- CI/CD: build per-service images, run the test suite (including the cross-tenant isolation suite and the alerting table test), run the migration job, then roll out. The OpenAPI spec drives a CI job that regenerates the public docs site on GitHub Pages (PRD 9, 10).

---

## 12. Cross-cutting entitlement enforcement (architecture contract)

Restated as a binding architecture-level contract; RFC-009 owns the model and PRD 11 owns the behavior.

Two enforcement points, neither trusting the other:

1. api on write. Every write touching a metered limit (monitor cap, interval floor, seat cap, region selection, status-page count) checks the org's entitlement and rejects over-limit writes with the standard per-field error plus an upsell.
2. scheduler on dispatch. The scheduler independently respects the org's interval floor and region entitlement on every dispatch, so a monitor created under a higher plan cannot keep running faster or in richer regions after a downgrade.

The metered limits and their rejection codes (from PRD-006, binding on api so the SPA and API clients can render upsells consistently): `monitor_limit_reached`, `interval_below_plan_floor`, `interval_below_hard_floor` (the 30s hard floor overrides any plan), `region_not_in_plan`, `region_count_exceeded`, `seat_limit_reached`, `status_page_limit_reached`, `custom_domain_not_in_plan`, `api_write_not_in_plan` (Free is read-only API), `api_rate_limited`. These return inside the standard error envelope (`code`/`message`/`fields`) with an upsell. Downgrades that would exceed the lower plan block the owner to bring usage under the limit first (no silent delete), except interval floor and region set which the scheduler simply clamps on dispatch.

The cache contract:
- Entitlements are read through `internal/entitlements`, which serves from Redis with Postgres as source of truth. The hot paths (api write, scheduler dispatch) never pay a Postgres read per request or per check.
- Invalidation is event-driven: a plan change (Stripe webhook -> `billing.events`, or an internal admin change) invalidates the org's cached entitlement so the next read repopulates from Postgres. The cache key is per org.
- Fail-closed on entitlement writes: if entitlements cannot be determined (cache miss and Postgres unavailable), the api write is rejected rather than allowed, so a downgrade can never be bypassed by knocking over the cache. The scheduler, if it cannot read entitlements, holds the last known snapshot (it rebuilt from Postgres on boot) rather than dispatching wide-open.

---

## 13. RFC index

The thirteen sub-RFCs, each with scope, the key contract or decision it owns, and its dependencies on other RFCs. These seams are stable; sub-RFCs can be written in parallel against them.

### RFC-001 - Data Model and Multi-Tenancy
Scope: the full PostgreSQL schema (all entities from PRD-001..007 with org_id), tenant isolation, RLS policies, check_results time-range partitioning, rollups, read replicas, migrations, backup/restore.
Owns: the shared-DB-plus-RLS tenancy implementation, the partitioning and rollup scheme, the cross-tenant isolation test suite (must pass before release), the Postgres `Store` implementation replacing SQLite.
Depends on: RFC-000 (tenancy decision, partitioning decision). Depended on by: every other RFC that reads/writes data.

### RFC-002 - Eventing and Kafka Contracts
Scope: exact topic list, event schemas, partition keys, delivery semantics, per-consumer idempotency, the cross-region mirror.
Owns: the event schema registry and the idempotency-key contract for every topic in section 5.
Depends on: RFC-000 (topic/partition/idempotency decisions). Depended on by: RFC-004/005/006/007/008/009.

### RFC-003 - AuthN and AuthZ
Scope: Google/GitHub OIDC login, account linking, RS256 JWT issue/verify, JWKS, refresh + revocation, API key format/hash/role-scoping, the RBAC matrix evaluation seam.
Owns: token lifetimes and rotation, the API key model, the request -> (org, role) resolution.
Depends on: RFC-000 (identity-propagation model), RFC-001 (user/org/membership/key schema). Depended on by: RFC-012 (API), RFC-013 (frontend auth).

### RFC-004 - Scheduler
Scope: distributed scheduling, leader election, the schedule rebuild, per-(monitor, region) fan-out, region dispatch, interval-floor and region-entitlement enforcement on dispatch.
Owns: the scheduling-accuracy SLO mechanism, the dispatch idempotency (job ids / checked_at stamping).
Depends on: RFC-000 (leader-election + entitlement + topology decisions), RFC-002 (check.jobs contract), RFC-009 (entitlement lookup). Depended on by: RFC-005.

### RFC-005 - Worker / Checker
Scope: check execution at scale, SSRF enforcement, regional worker specifics, result write + emit, heartbeats.
Owns: the worker idempotent result write, the SSRF posture in the regional context, reuse of `internal/checker`.
Depends on: RFC-000 (topology, SSRF stance), RFC-002 (check.jobs / check.results), RFC-001 (check_results schema). Depended on by: RFC-006, RFC-008.

### RFC-006 - Alerting
Scope: the state machine at scale, per-monitor ordering, multi-region verdict reduction, incident lifecycle, idempotent transitions.
Owns: the distributed wrapper around `internal/alerting`, the redelivery-safety contract (section 8), notify-event emission with dedup ids.
Depends on: RFC-000 (ordering/idempotency decisions), RFC-002 (check.results / notify.events), RFC-008 (down-policy + probe health). Depended on by: RFC-007.

### RFC-007 - Notifier
Scope: channel delivery, retry/backoff, idempotency/dedup, org-level outbound webhooks, delivery-outcome recording.
Owns: the notify dedup-id contract, reuse of `internal/notify`, webhook signing.
Depends on: RFC-000, RFC-002 (notify.events), RFC-001 (channels schema), RFC-003 (webhook secrets). Depended on by: none downstream.

### RFC-008 - Multi-Region and Probe Fleet
Scope: control/data-plane split detail, region health detection, coverage-degraded, failover, cost-aware scheduling, cross-region Kafka mirror.
Owns: probe-fleet health, the failover policy, the regional Kafka topology and egress cost.
Depends on: RFC-000 (messaging-shape decision, topology), RFC-002 (region.health, mirror), RFC-005 (workers), RFC-006 (verdict consumes region health). Depended on by: RFC-006 (verdict), RFC-009 (region entitlement).

### RFC-009 - Entitlements Enforcement
Scope: the plan-limits model, the two enforcement points, the Redis-cached lookup library and its invalidation.
Owns: the entitlement data model, the cache + invalidation contract (section 12), the fail-closed-on-write stance.
Depends on: RFC-000 (library-not-service decision, cache contract), RFC-001 (plan/subscription schema), RFC-002 (billing.events for invalidation). Depended on by: RFC-004, RFC-012.

### RFC-010 - Observability and SRE
Scope: metrics/logs/traces standards, SLIs/SLOs/error budgets, self-monitoring, capacity and cost.
Owns: the SLO definitions and measurement, the trace-propagation-over-Kafka standard, the dashboards/alerts.
Depends on: RFC-000 (the three-pillar standards). Depended on by: every service RFC (they expose the SLIs defined here).

### RFC-011 - Deployment and Infra
Scope: Kubernetes topology, Docker images, Helm/Terraform, CI/CD, environments, DR, ingress/TLS, secret/KMS, NetworkPolicy.
Owns: the leader-election runtime, managed-infra choices, the migration job, TLS/wildcard cert handling, the service-to-service trust runtime (NetworkPolicy + TLS).
Depends on: RFC-000 (deployment decisions). Depended on by: every service at deploy time.

### RFC-012 - API Design and OpenAPI
Scope: REST conventions, versioning, pagination, error envelope, rate limits, and the OpenAPI 3 spec as single source of truth + Swagger UI.
Owns: the public API contract and the spec-to-docs CI pipeline.
Depends on: RFC-000, RFC-003 (auth/keys), RFC-009 (entitlement errors on write). Depended on by: RFC-013 (frontend consumes it).

### RFC-013 - Frontend
Scope: Lit SPA architecture, routing, state, nginx serving, auth/token handling, org switcher.
Owns: the SPA, how it carries the JWT, how it serves behind nginx, status-page rendering.
Depends on: RFC-000, RFC-003 (auth), RFC-012 (API contract). Depended on by: none downstream.

---

## 14. Reuse and migration map

| Package | Status | What changes |
|---------|--------|--------------|
| `internal/domain` | reused, extended | add `OrgID` to every owned entity; add `Region` to `CheckResult` and the check job; add new entities (User, Org, Membership, Invitation, ApiKey, Plan/Subscription, StatusPage, Region) as additive structs. Still imports nothing internal |
| `internal/crypto` | reused unchanged | AES-256-GCM Encrypt/Decrypt/LoadKey carry forward verbatim; only the key source moves to KMS-backed secret (section 10) |
| `internal/checker` | reused unchanged | the HTTP check, assertion priority, SSRF resolve + dialer Control all carry forward; workers wrap it. SSRF goes from opt-in to always-on by config, not code change |
| `internal/alerting` | reused unchanged | the pure `Apply` state machine is the heart of RFC-006. Its purity is exactly what makes distributed idempotency work (section 8). The multi-region verdict is computed before `Apply`, feeding it a single healthy/unhealthy input (PRD 6.4), so `Apply` itself is untouched |
| `internal/notify` | reused unchanged | the Manager fan-out + retry/backoff and the four notifiers carry forward; notifier wraps it and adds the dedup-id check and outbound-webhook signing |
| `internal/store` | replaced | the SQLite implementation is swapped for a Postgres implementation of the same-shaped `Store` interface, extended with org scoping, RLS session-var setting, partition-aware result writes, and the new entities. The partial unique index on open incidents and the consecutive_fails/first_fail_at columns carry forward conceptually |
| `internal/auth` | replaced/extended | the v1 single-admin + password + cookie-session model is replaced by `internal/authn` (OIDC, RS256 JWT, JWKS, API keys) and `internal/authz` (RBAC). bcrypt-style hashing carries forward for API key hashing |
| `internal/config` | reused, extended | per-service config; new vars for Kafka/Redis/Postgres DSNs, region code, OIDC client creds |
| new: `internal/bus` | new | Kafka producer/consumer wrappers, topic+key helpers, header-based trace propagation |
| new: `internal/redis` | new | cache, locks, rate-limit, dedup helpers |
| new: `internal/entitlements` | new | the cached entitlement library (section 2.6, 12) |
| new: `internal/region` | new | region registry, probe-fleet health, verdict reduction |
| new: `internal/obs` | new | Prometheus metrics, slog setup, OTel wiring |
| `web/` | reused | the Lit SPA carries over, now served by nginx instead of embedded; auth shifts from cookie-only to JWT handling (RFC-013) |

Net: the monitoring mechanics (checker, alerting, notify, crypto) are unchanged proven code. The runtime (single process -> five services), the store (SQLite -> Postgres), and identity (single admin -> multi-tenant OIDC/RBAC) are what change. Every change is additive at the domain level (org_id, region) so the proven contracts hold.

---

## 15. Architecture Decision Records (to be written)

Load-bearing decisions that warrant a standalone ADR. One-line rationale each; the ADR files come later under `docs/adr/`.

| ADR | Decision | One-line rationale |
|-----|----------|--------------------|
| ADR-0001 | Shared-DB row-level tenancy + RLS | one schema scales to 50k orgs; RLS is defense in depth so a missed filter fails safe |
| ADR-0002 | PostgreSQL + pgx as the datastore/driver | Postgres replaces SQLite for the SaaS; pgx for performance and Postgres feature access (RFC-001 confirms driver) |
| ADR-0003 | Kafka client choice | pick one Go Kafka client (franz-go vs sarama vs confluent-kafka-go) for the bus; weighs idempotent-producer support and no-cgo (RFC-002) |
| ADR-0004 | Scheduler leader election via k8s Lease | singleton guarantee tied to the control plane, not to a fail-open cache |
| ADR-0005 | RS256 JWT + JWKS | private signing key stays only in api; verification needs no shared secret |
| ADR-0006 | Multi-region messaging shape (regional Kafka + results mirrored home) | local consume for workers, bounded egress for results, contained blast radius |
| ADR-0007 | check_results time-range partitioning + rollups | retention is a partition drop, not a mass delete; rollups keep history/uptime fast |
| ADR-0008 | Entitlements as a library + Redis cache, not a service | sub-ms hot-path lookup, no over-fragmentation, fail-closed on write |
| ADR-0009 | At-least-once Kafka + idempotent consumers (not EOS transactions) | exactly-once-in-effect without transaction coordination cost |
| ADR-0010 | NetworkPolicy + TLS-to-infra for v1 service trust, mesh deferred | covers the realistic threat without mesh weight; mesh revisited at SOC 2 / scale |

---

## Open questions for product

These are product gaps or conflicts surfaced while writing the architecture. They are not decided here.

1. Status-page resilience target. PRD 12 says status-page serving should be "especially resilient" and stay up even if write paths degrade, but does not give it a separate SLO. The architecture serves status pages cache-first off replicas so it can outlive a primary write outage; product should confirm whether status pages get an explicit availability target above the 99.9% control-plane number, since that would justify further isolation.
2. Acting-principal in internal events. Audit (PRD 13) wants "who did what." The architecture carries the actor in `monitor.changed` / `audit.events`, but the PRD does not specify whether automated (API key) actors and system actions are distinguished in the audit trail from human actors. RFC-001/RFC-003 need that distinction defined. The consistency review also notes login-event audit volume (PRD-001 D5) may want its own retention stream separate from people-changes; that is a product call that affects how `audit.events` is partitioned/retained.

3. Create-organization capability is not in the master RBAC matrix. PRD-001 marks "create a new organization" as open to any role but the master matrix (PRD 4) has no row for it and no endpoint is defined. The architecture treats org creation as a per-user action (any authenticated user can create an org and becomes its owner, matching the personal-org-at-signup model), but product should add the row so RFC-012 can define the endpoint without guessing.

The following were genuinely open in the sub-PRDs but are now locked by the consistency review and are treated as settled by this RFC: 14-day Team trial (PRD-006 canonical), 14-day org-deletion grace, incident-duration-based uptime (PRD-002 canonical), quorum denominator = healthy-reporting regions `R`. A minor terminology nit (define "check" vs "probe" in one glossary place) is non-blocking doc polish. Everything else needed for the architecture is decided in the PRDs.

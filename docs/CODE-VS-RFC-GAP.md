# Code vs RFC Gap Analysis - Pulse

Status: HISTORICAL BASELINE (pre-implementation). The analysis below describes the
repo when it was still the v1 monolith, before any v2 work. It is kept as the
"before" picture. Since then the foundation and the first vertical slice have
landed, so the numbered gaps below are partly closed. See the update note next,
then README and `IMPLEMENTATION-PLAN-foundation.md` for the current state.
Author: architecture review
Scope: the `pulse/` Go module and `web/` SPA at baseline, checked against `RFC-000` and the thirteen sub-RFCs.

## Update: what has been built since this baseline

This doc was written before implementation. What now exists (see README,
`IMPLEMENTATION-PLAN-foundation.md`):

- **Foundation:** the five service binaries (`cmd/api|scheduler|worker|alerting|notifier`) plus `cmd/schema`, each booting via a shared `internal/runtime` bootstrap with health/metrics and graceful shutdown. New packages `internal/{obs,config,store,kv,bus,runtime}`. The v1 spine (`internal/store` SQLite, `internal/auth`, `internal/web` embed, `cmd/pulse`) was removed; the leaf packages (`checker`, `alerting`, `notify`, `crypto`, `domain`) carry forward as planned.
- **Datastore:** Postgres via pgx, connected and pinged; multi-tenant RLS backbone (`organizations`, `monitors`, `check_results` with `org_id` + RLS policies and the `WithOrg` helper), proven by an integration test. Schema is applied wholesale from `schema.sql` (reset-based; no versioned migrations yet, deliberately early).
- **Eventing / cache:** Kafka via franz-go (`internal/bus`) and Redis via go-redis (`internal/kv`) connected, with a testcontainers integration test round-tripping a keyed message and exercising locks/cache.
- **Phase-0 slice step 1:** the scheduler dispatches per-(monitor, region) jobs to Kafka; the worker consumes them, runs `internal/checker`, and persists results. Proven end to end against live Postgres + Redpanda + a real HTTP target.
- **Dev experience:** a dev-only auth + sample-data stub (`internal/devapi`, behind `PULSE_DEV_AUTH`) so the SPA is browsable before real auth lands.
- **Check-now + live per-region state:** check-now is now async per RFC-004 §9: the api enqueues one job per region to `check.jobs.<region>` (the same shape the scheduler produces) and returns `202`; the worker runs it through the normal pipeline. On top of that, every check (scheduled or manual) drives a continuous per-`(monitor, region)` live state in Redis (`internal/checkstate`): the scheduler marks a region `scheduled` on dispatch, the worker marks it `running` then `done`/`failed` with the outcome. The frontend reads it page-wide via `GET /orgs/{orgId}/monitor-region-states` (one chip per region). The manual-check rate limit is a per-monitor burst cooldown plus a per-org sustained window, returning `429` with `Retry-After` and an actionable envelope (PRD-006). `MonitorListItem` now carries the effective (plan-floored) `interval_seconds` and `next_check_at`, both derived from persisted state so they survive a restart. The scheduler also seeds its next-run from the persisted last check, so a restart (frequent on CD) does not cause a check storm. Still deferred: the per-monitor in-flight worker lock (RFC-004 §8.2/§9) is not built; the burst cooldown serves as the guard for now, and a multi-region check-now can in a rare window overlap a scheduled tick.

- **Transactional email consolidation (RFC-019):** all outbound mail now goes through the notifier. The api publishes semantic intents on `email.events` (`MagicLinkRequested`, `InvitationRequested`, `ChannelTestRequested`) and no longer sends mail inline; the notifier is the only sender and mints the magic-link / invite token at send time, so no usable credential ever rides the bus. From routing segments reputation by subdomain (account vs alerts). The magic-link Redis record contract moved to `internal/maglink` (shared by the notifier's mint and the api's verify); `invitations.token_hash` is now nullable (migration 00008). The Team-email channel test is sent to the clicker only; other channel types still test synchronously in the api.

Still not built (the bulk of the numbered list below remains accurate): real auth (RFC-003), the public REST API and OpenAPI (RFC-012), alerting + notifier business logic (RFC-006/007, slice steps 2-3), status pages, billing/entitlements, multi-region verdict logic, observability SLIs, and deployment (Helm/Terraform/k8s). Several "none exists" lines below are now "scaffolded, logic pending."

## TL;DR (baseline, as originally written)

The code in this repo is the **v1 single-binary monolith** described in `docs/archive/ARCHITECTURE_v1_monolith.md`. The RFC set describes the **v2 distributed multi-tenant SaaS**. These are not the same system, and the gap is not "drift that crept in" that you reconcile edit-by-edit. It is the planned predecessor: RFC-000 explicitly supersedes the v1 architecture for runtime and keeps only a handful of leaf packages (RFC-000 section 14). So the honest framing is "what exists today is the starting material for a near-greenfield rebuild," not "the code wandered away from the design."

Two facts set the baseline:

1. The whole thing is one binary, `cmd/pulse/main.go`, and even that is a stub: it loads config and prints "pulse config loaded", then exits. There is no HTTP server, no scheduler loop, no api layer in the code at all (no `internal/api`, `internal/server`, or `internal/scheduler`).
2. The packages that do exist (`checker`, `alerting`, `notify`, `crypto`, `store`, `auth`, `domain`, `config`) implement the v1 monolith's mechanics against SQLite with a single env-var admin. They reference "ARCHITECTURE section 6", "PRD 11.2", "ARCHITECTURE 3.2" in their comments, i.e. the archived v1 docs.

So the RFCs are a forward design and the code is the thing they are migrating from. Everything below is "what the v2 design needs that the v1 code does not have", grouped by architectural dimension. None of it is a bug in the code; it is scope that is not built yet.

## What actually carries forward (RFC-000 section 14 is accurate)

Before the gaps, the reuse claims hold up. These packages are real, tested, and match what RFC-000 says it will keep:

- `internal/domain` - the plain structs and enums (Monitor, Channel, CheckResult, Incident, AlertState, FailureReason, Status). Imports nothing internal, as RFC-000 promises.
- `internal/checker` (`checker.go`, `ssrf.go`, `statuscodes.go`) - the HTTP check, the six failure reasons in the exact priority order the PRDs and RFC-005 cite, and the SSRF resolve + dialer guard. RFC-005 reuses this unchanged.
- `internal/alerting` - the pure state machine. RFC-006's whole idempotency story depends on this purity, and it is genuinely pure here.
- `internal/notify` - the Manager fan-out plus the four channel senders (slack/discord/webhook/smtp) and the rendered payloads.
- `internal/crypto` - AES-256-GCM per-value encryption. RFC-000 keeps the contract and only moves the key source to KMS.
- `internal/store` schema shapes - the partial unique index `uniq_open_incident WHERE ended_at IS NULL` and the `consecutive_fails` / `first_fail_at` columns carry into RFC-001's Postgres design verbatim in spirit.

The reuse map is the part of the plan the code actually supports today. The rest is greenfield.

## Architectural misalignments

### 1. Process topology: one stub binary vs five services + regional data planes

- Code: a single `cmd/pulse/main.go` (a stub), one `go.mod` named `pulse`, no service binaries.
- RFC-000 section 2: five services (`cmd/api`, `cmd/scheduler`, `cmd/worker`, `cmd/alerting`, `cmd/notifier`) from one module, plus a control-plane / regional-data-plane split (section 4).
- Gap: none of the five `cmd/` binaries exist; there is no service decomposition at all. This is the single biggest structural difference and everything else hangs off it.

### 2. Datastore: SQLite vs Postgres

- Code: `modernc.org/sqlite`, a `*sql.DB`, `TEXT` timestamps, one `0001_init.sql` migration, integer autoincrement ids.
- RFC-001: PostgreSQL via `pgx`/`pgxpool`, time-range partitioned `check_results`, hourly `check_rollups`, read replicas, RLS policies, `golang-migrate`, BIGINT identity.
- Gap: the entire datastore is the wrong engine and shape. `go.mod` has no `pgx` and `go.sum` has no Postgres driver. None of partitioning, rollups, replicas, or RLS exists. This is a `internal/store` rewrite, which RFC-000 section 14 already labels "replaced".

### 3. Eventing: none vs Kafka

- Code: no message bus. The check loop, alerting, and notify are in-process calls.
- RFC-002: seven Kafka topics (`monitor.changed`, `check.jobs.<region>`, `check.results`, `notify.events`, `audit.events`, `billing.events`, `region.health`), franz-go, partition-key + idempotency contracts, a regional-to-central mirror.
- Gap: there is no `internal/bus`, no Kafka client, no topics, no event schemas, no idempotency keys. The whole asynchronous backbone the v2 correctness model rests on (per-monitor ordering, at-least-once + idempotent consumers) does not exist.

### 4. Cache and coordination: none vs Redis

- Code: no Redis. No caching, no distributed locks, no rate-limit counters, no dedup sets.
- RFC-000 / RFC-004 / RFC-009: Redis for the entitlement cache, the per-monitor "check now" lock, rate-limit counters, notify dedup ids, and (rejected for leader election in favor of k8s Lease, but used elsewhere).
- Gap: no `internal/redis`. Several v2 mechanisms (check-now exclusion across workers, entitlement hot path, rate limiting, dedup) have nowhere to live yet.

### 5. Tenancy: single-admin single-tenant vs multi-tenant orgs

- Code: there is no tenant concept. No `org_id` on any table or domain struct. One `admin` row (`CHECK (id = 1)`). Monitors, channels, incidents are global.
- RFC-001 / RFC-003 / master PRD: every owned row carries `org_id`, with repository scoping plus Postgres RLS as defense in depth, and the cross-tenant isolation test suite that must pass before release.
- Gap: this is foundational and touches every table and every query. The hard "org A can never see org B" invariant has no expression in the code because there are no orgs.

### 6. Identity and auth: env admin + cookie sessions vs OIDC + JWT + API keys

- Code: `internal/auth` is one env-var admin (`PULSE_ADMIN_USER` / `PULSE_ADMIN_PASSWORD`), bcrypt password hash in a single `admin` row, opaque random session token in an httpOnly cookie stored in a `sessions` table. No users, no providers.
- RFC-003: Google/GitHub OIDC login, account linking on verified email, RS256 JWT + JWKS, refresh-token rotation with families and reuse-detection, per-org API keys (`pulse_sk_`, SHA-256 at rest, member/admin only). The v1 bcrypt path survives only as the self-host bootstrap admin.
- Gap: the SaaS identity model is absent. `internal/authn` and `internal/authz` do not exist; there is no OIDC, no JWT, no JWKS, no API keys, no RBAC matrix evaluation. The current `auth` package is exactly the one piece RFC-003 says becomes self-host-only.

### 7. Domain model: missing the multi-tenant and multi-region fields and entities

- Code `domain.go` has Monitor, Channel, CheckResult, Incident, AlertState. It is missing:
  - `OrgID` on every entity (tenancy).
  - `Region` on CheckResult and the check job, and `Regions` + `DownPolicy` on Monitor (multi-region, which the PRDs say is a day-0 field).
  - `type` on Monitor (the phased monitor-type field PRD-002 wants from day one).
  - The `manual` close reason on Incident (code has only `recovered` / `disabled`; PRD-002 and the RBAC matrix add owner/admin manual close).
  - Whole entities that do not exist at all: User, UserIdentity, Organization, Membership, Seat, Invitation, Plan, Subscription, Entitlement, StatusPage, ApiKey, OutboundWebhook, AuditEvent, Region, RegionHealth.
- Gap: the domain vocabulary is the v1 set. RFC-000 calls the additions "additive at the domain level", which is true for `OrgID` / `Region`, but the dozen-plus new entities are net-new modeling.

### 8. Multi-region: none vs control/data-plane split with quorum and probe health

- Code: no region anywhere. The check loop would run wherever the binary runs.
- PRD-007 / RFC-008: per-region check attribution, `down_policy` (any/quorum/all) reduction over healthy reporting regions `R`, probe-fleet health, coverage-degraded, regional failover, the regional Kafka + mirror topology.
- Gap: no `internal/region`, no region catalog, no heartbeats, no verdict reduction, no coverage-degraded. This is the v2.1 headline capability and it has zero code footprint.

### 9. Entitlements and billing: none vs plan tiers + two-point enforcement + Stripe

- Code: no plans, no limits, no metering. `RetentionDays` is a single global env int (default 30), not a per-org tier.
- PRD-006 / RFC-009: four plan tiers, ten entitlement error codes, enforcement at the api on write and the scheduler on dispatch (neither trusting the other), Redis-cached entitlements, Stripe in phase 2.
- Gap: no `internal/entitlements`, no plan/subscription/entitlement tables, no enforcement points, no error codes. Nothing limits anything today.

### 10. Scheduler / worker / notifier as services

- Code: the scheduler does not exist (main is a stub; there is no scheduling loop). `checker` is a library that would be called in-process. `notify.Manager` fans out in-process.
- RFC-004 / RFC-005 / RFC-007: a leader-elected distributed scheduler dispatching per-(monitor, region) jobs to Kafka; stateless regional workers consuming `check.jobs.<region>`; a notifier service consuming `notify.events` with dedup. Plus the flagged RFC-005/RFC-006 decision that the worker emits results to Kafka and the alerting transaction persists them.
- Gap: the three pipeline services do not exist as services. The reusable leaf logic (`checker`, `notify`) is present, but the distributed shells around them are not.

### 11. Status pages, public API, OpenAPI, outbound webhooks

- Code: none of these exist. No status-page serving, no `/api/v1`, no OpenAPI spec (`api/openapi/` is not present), no Swagger UI, no outbound webhooks.
- PRD-004 / PRD-005 / RFC-012: public status pages, a versioned REST API, OpenAPI 3 as the single source of truth served at `/api/docs`, org-level outbound webhooks with `X-Pulse-Signature`.
- Gap: entire product surfaces are unbuilt. The `api/openapi/` directory RFC-000 section 3 lists does not exist in the tree.

### 12. Frontend serving: embedded in the Go binary vs nginx-served SPA

- Code: `internal/web/embed.go` plus `internal/web/dist/` means the built SPA is embedded into the Go binary. The Lit + Vite + TS source is under `web/src/` (login-view, app-root, app-nav, status-badge, router, session state, a small api client).
- RFC-000 section 3 / RFC-013: the SPA is no longer embedded; nginx serves the static assets and proxies `/api`, with status pages served cache-first by nginx. Auth shifts from cookie-only to JWT handling.
- Gap: the serving model is inverted (embedded vs nginx) and the SPA itself targets the v1 single-admin cookie world, not the multi-org JWT world (no org switcher, no OIDC login buttons wired to providers, the api client speaks the v1 shape).

### 13. Observability: traces + core metrics built; logs/dashboards pending

- Code: `internal/obs` has the logger, the per-service Prometheus registry served at
  `/metrics` (OpenMetrics, so exemplars are exposed), and the OTel tracer setup. Traces
  are browser-rooted (RFC-021) and run end to end: the api edge span, DB spans (otelpgx),
  and the full check pipeline as one trace (`schedule.dispatch -> check.execute ->
  verdict.apply -> notify.deliver`) joined over the bus `traceparent` rail with
  producer/consumer spans, so Tempo draws the service graph. The three SLO histograms
  (`pulse_schedule_dispatch_lag_seconds`, `pulse_verdict_latency_seconds`,
  `pulse_pipeline_notify_latency_seconds`) carry trace exemplars, plus the per-service
  RED/lifecycle counters (jobs dispatched/consumed, check results + duration, incidents
  opened/closed, notifications, dedup suppressions, redelivery no-ops). The dev stack
  (`observability/`, `make up-obs`) and the k3s stack (`deploy/observability/`) both run
  Collector + Tempo + Prometheus + Grafana.
- RFC-010: `/metrics` per service, Prometheus client, OTel traces over Kafka headers, Loki
  logs, the committed SLOs and dashboards.
- Logs pillar built: every service ships its slog lines over OTLP (the otelslog bridge ->
  the collector -> Loki), carrying `trace_id` as structured metadata, so a log links to its
  trace and a span links to its logs in Grafana, both ways. Only `service_name` is a Loki
  index label (cardinality discipline); per-check detail is debug, business events
  (incident opened/closed, notification delivered) are info. Dev (`observability/`) and k3s
  (`deploy/observability/`) both run Collector + Tempo + Prometheus + Loki + Grafana.
- Still pending: the SLO dashboards (beyond the starter overview board) and the
  Alertmanager rules, the §2.4 DLQ counter (no DLQ yet), and a few §2.5 metrics that wait
  on unbuilt features: SSRF-block and notification-retry counters (need a hook in checker /
  the notify Manager), `pulse_coverage_degraded_total` (multi-region verdict), and the
  scheduler leader/rebuild metrics (no leader election yet).

### 14. Deployment: single binary vs Kubernetes multi-region

- Code: no `deploy/` directory, no Dockerfiles, no Helm, no Terraform.
- RFC-011: per-service distroless images, an nginx image, Helm umbrella chart, Terraform for managed Postgres/Redis/Kafka, GitOps, cert-manager, multi-region clusters, DR with PITR.
- Gap: the entire deployment substrate is unbuilt. The repo has no infra-as-code at all.

### 15. SSRF posture: opt-in (default off) vs on-by-default, not disableable

- Code: `config.BlockPrivateNetworks` defaults to **false** (`PULSE_BLOCK_PRIVATE_NETWORKS`, default false). SSRF blocking is opt-in, which is the v1 self-host stance.
- PRD-013 / RFC-005: in the hosted SaaS, SSRF protection is on by default and not customer-disableable, because the multi-tenant threat model inverts the v1 reasoning.
- Gap: the default is the wrong way around for SaaS. The `checker/ssrf.go` mechanism exists and is reused, but the policy that turns it on always is config-level and currently defaults off. RFC-000 section 14 notes this becomes "always-on by config, not a code change", so it is a small change, but today the safe default is not set.

### 16. Config and secrets: single-process env vs per-service + KMS

- Code: `internal/config` is one flat env set for one process (SQLite path, listen addr, single admin creds, base64 secret key from `PULSE_SECRET_KEY` env var, worker count, retention days).
- RFC-000 section 10 / RFC-011: the 32-byte key comes from a cloud KMS-backed Kubernetes secret, not an env var baked into an image; config is per-service with Kafka/Redis/Postgres DSNs, region code, OIDC client creds.
- Gap: config is shaped for one process and sources the encryption key from an env var, which RFC-011 explicitly moves to KMS. Per-service config and the new DSN/region/OIDC vars do not exist.

## How to read this

The mismatch is total at the system level and expected at the package level. The RFC authors planned for exactly this: a v1 monolith whose proven leaf logic (check, alert, notify, crypto, the domain vocabulary, two load-bearing schema shapes) is lifted into a new distributed, multi-tenant, multi-region runtime. Nothing here suggests the code "diverged from" the RFCs by accident. It is the before-picture.

Practical implication: there is no in-place reconciliation to do between the code and the RFCs. The work is to build the v2 services against the RFC seams and graft in the five reusable packages, which is the production order PLANNING.md already states (step 8: "resume implementation, reusing the already-built Go packages"). The useful output of this review is the concrete per-dimension list above of what the reuse does and does not cover, so the build does not assume more carries forward than actually does. The honest carry-forward set is section "What actually carries forward"; everything in the numbered list is new construction.

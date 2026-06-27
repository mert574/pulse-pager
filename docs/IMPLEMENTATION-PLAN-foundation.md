# Implementation Plan - Multi-Tenant Multi-Service Foundation

Status: implemented. The foundation described here is built and tested (see README to run it). One deviation landed during the build: instead of versioned migrations we apply a single `schema.sql` that drops-and-recreates (reset-based), so golang-migrate, the `migrations/` dir, and `schema_migrations` below are not used yet. Versioned migrations return once the data model settles. The doc is kept as the record of the plan, with that one change folded in.
Scope: the groundwork only. Stand up the five services, wire them to Postgres / Redis / Kafka, add the logging+metrics package, prove the multi-tenant isolation backbone, and land one integration test that exercises the whole foundation. No product features.

This follows the reuse map in `RFC-000` section 14 and the production order in `PLANNING.md` step 8: keep the proven leaf packages, replace the spine, build the service shells around stable seams. Tech choices below are the ones the RFCs already decided; this plan just lands them as scaffolding.

## Goal and non-goals

In scope (the barebones):
- One Go module, five service binaries that boot, read per-service config, set up the logger, connect to the infra they need, expose `/healthz` + `/readyz` + `/metrics`, and shut down gracefully.
- `internal/obs` (slog + Prometheus + OTel wiring), `internal/config` (per-service), `internal/store` (pgxpool + schema apply), `internal/kv` (Redis), `internal/bus` (Kafka via franz-go).
- The multi-tenant backbone: an org-scoped repository helper plus a Postgres RLS policy, proven by a test that org A cannot read org B's rows.
- Local infra via docker-compose (Postgres, Redis, Redpanda) and one integration test (testcontainers) that applies the schema, pings every dependency, round-trips one Kafka message, and asserts RLS isolation.
- Makefile targets and a CI job that runs unit + integration tests.

Out of scope (explicitly not now): OIDC/JWT/API keys, the RBAC matrix, entitlements enforcement, the real scheduler/worker/alerting/notifier logic, multi-region verdict reduction, status pages, the public REST API surface, the OpenAPI spec, the SPA rework, Helm/Terraform/k8s, leader election. Those are later work packages; the shells just need to exist and connect.

## 1. Module and dependencies

Keep the module name `pulse` and the Go version. Change `go.mod`:

- Remove `modernc.org/sqlite` (the SQLite store is replaced).
- Add, each pinned to the decision in the cited RFC:
  - `github.com/jackc/pgx/v5` (+ `pgxpool`) - Postgres driver, RFC-001 / ADR-0002.
  - `github.com/twmb/franz-go` - Kafka client, RFC-002 / ADR-0003.
  - `github.com/redis/go-redis/v9` - Redis client (RFC-000 section 2.6 / RFC-009 cache + locks).
  - `github.com/prometheus/client_golang` - metrics, RFC-010.
  - `go.opentelemetry.io/otel` (+ OTLP exporter) - tracing, RFC-010.
  - (no migration library yet) - the schema is applied wholesale from an embedded `schema.sql`; golang-migrate returns when the data model settles (RFC-001 section 8).
  - `github.com/testcontainers/testcontainers-go` (+ postgres, redis, redpanda modules) - integration test harness (test-only).
- Keep `golang.org/x/crypto` (bcrypt survives for the self-host bootstrap admin only; AES-GCM is `internal/crypto`).

## 2. Target repository layout

```
pulse/
  go.mod  go.sum                      # module pulse, sqlite removed, infra clients added
  Makefile
  docker-compose.yml                  # local postgres + redis + redpanda
  cmd/
    api/main.go                       # control-plane HTTP edge
    scheduler/main.go                 # leader-elected singleton (election deferred; boots as single instance)
    worker/main.go                    # regional check executor
    alerting/main.go                  # verdict + state machine consumer
    notifier/main.go                  # delivery consumer
  internal/
    # --- kept unchanged from v1 (leaf packages) ---
    domain/                           # + OrgID, Region added additively (struct fields only)
    crypto/                           # unchanged
    checker/                          # unchanged
    alerting/                         # unchanged (pure Apply)
    notify/                           # unchanged
    # --- new foundation packages ---
    obs/                              # slog JSON logger, Prometheus registry, OTel setup, health server
    config/                           # per-service env config (replaces the single-process one)
    store/                            # pgxpool, schema apply (ApplySchema), org-scoping helper, RLS
    store/schema.sql                  # single source-of-truth schema, drop-and-recreate (reset)
    kv/                               # Redis client wrapper + Ping + a lock/cache helper stub
    bus/                              # franz-go producer/consumer wrappers, topic+key helpers
    # --- deleted ---
    # store/ (sqlite impl), auth/ (env admin), web/ (embed), cmd/pulse/  -> removed
  test/
    integration/foundation_test.go    # the one barebones end-to-end wiring test
```

`cmd/pulse/`, the SQLite `internal/store`, `internal/auth`, and `internal/web` are removed. The Lit SPA under `web/` is parked for the RFC-013 work package and is not touched here.

## 3. Foundation packages (barebones content)

### internal/obs

- `Logger(service string) *slog.Logger` - JSON handler to stdout, level from config, a base attribute `service=<name>`. Loki-friendly, low-cardinality (RFC-010 section 2.2).
- Correlation id: a context helper that carries a request/trace id and a `slog` middleware that stamps it. The Kafka header propagation (RFC-010 section 1.2) is stubbed with the header names reserved (`traceparent`, `pulse-correlation-id`) but full OTel span plumbing is a later package; here we just create the tracer provider and a no-op-friendly exporter switch.
- `Metrics() *prometheus.Registry` plus a tiny set of process/build-info collectors. Real SLI metrics come with each service later.
- `HealthServer(addr string, checks ...ReadyCheck)` - serves `/healthz` (liveness, always 200 if the process is up), `/readyz` (runs the dep checks), `/metrics`. Every service mounts this.

### internal/config

- One `Load(service string) (*Config, error)`, fail-closed on missing required vars (carry the v1 discipline).
- Fields the foundation needs: `PostgresDSN`, `RedisAddr`, `KafkaBrokers`, `Region` (default `home`), `LogLevel`, `HealthAddr`, `OTelEndpoint` (optional), and the AES `SecretKey` (still base64 of 32 bytes, validated by `crypto.LoadKey`; RFC-011 moves the source to KMS later, the shape is unchanged).
- A service only requires the deps it uses (worker does not need `PostgresDSN` in the barebones; api/scheduler/alerting/notifier do).

### internal/store

- `Open(ctx, dsn) (*Pool, error)` over `pgxpool`, with a `Ping`.
- `ApplySchema(ctx, pool)` runs the embedded `schema.sql` (via `//go:embed`) on the simple query protocol. The script drops-and-recreates the tables, so re-running it resets the schema (no version tracking yet). Run via `make schema` or `cmd/schema`; later this becomes a k8s pre-deploy Job with versioned migrations (RFC-011 section 9).
- The org-scoping helper, which is the heart of the multi-tenant backbone (section 4): `WithOrg(ctx, pool, orgID, fn)` opens a transaction, runs `SET LOCAL app.current_org = $orgID`, and calls `fn(tx)`. Every tenant query goes through this so RLS has the session var set.

### internal/kv (Redis)

- `Open(addr) (*Client, error)` over go-redis with a `Ping`.
- One thin helper each for the patterns the foundation will need so the shape exists: `SetNX`-based lock acquire/release (check-now lock, RFC-004) and a get/set cache helper (entitlement cache, RFC-009). Bodies can be minimal; the point is the package and signatures exist.

### internal/bus

- `NewProducer(brokers)` and `NewConsumer(brokers, group, topics)` over franz-go.
- `Produce(ctx, topic, key, value, headers)` and a consume loop callback. Topic + partition-key helpers per RFC-002 section 5.2 (`monitor.changed`->org_id, `check.results`->monitor_id, etc.) as constants so producers key consistently.
- Header-based correlation-id propagation hook (stamps `pulse-correlation-id` on produce, restores on consume), matching RFC-010.
- No DLQ / idempotency machinery yet; that lands with the real consumers. Barebones is "can produce and consume one message reliably."

## 4. Multi-tenant backbone (the one piece of real logic in the groundwork)

This is what makes it "multi-tenant" groundwork and not just "connected to a database." It lands the isolation invariant (RFC-001 section 5/6.1, master PRD 13) as a working, tested pattern, with one example tenant table so we are not yet modeling the real schema.

Migration `0001_foundation.up.sql`:
- `organizations (id BIGINT GENERATED ALWAYS AS IDENTITY PK, name TEXT, created_at TIMESTAMPTZ)`.
- One example tenant table, e.g. `widgets (id ..., org_id BIGINT NOT NULL REFERENCES organizations(id), label TEXT)`, standing in for the real org-owned entities. It exists only to prove the pattern.
- RLS on `widgets`: `ALTER TABLE widgets ENABLE ROW LEVEL SECURITY;` and a policy `USING (org_id = current_setting('app.current_org')::bigint)`.
- A non-superuser app role that RLS applies to (RLS is bypassed by the table owner / superuser, so the app connects as a restricted role; the migration role gets BYPASSRLS, RFC-011 section 9.1).

The `WithOrg` helper (section 3) plus this policy give the two-layer isolation RFC-000 section 6.1 specifies: the repository scopes by `org_id` and RLS fails safe if a query forgets. When the real schema lands (RFC-001), every tenant table follows this exact pattern.

## 5. Service skeletons

Each `cmd/<service>/main.go` is the same shape, so factor a small shared bootstrap (in `obs` or a tiny `internal/runtime`):

1. Load config for the service.
2. Build the logger and tracer.
3. Connect to the deps this service uses, each registered as a `/readyz` check:
   - `api`: Postgres + Redis (+ a Kafka producer for `monitor.changed`).
   - `scheduler`: Postgres + Redis + Kafka producer (`check.jobs.<region>`); boots as a single instance, leader election is a later package (a `// TODO RFC-004` marks it).
   - `worker`: Kafka consumer (`check.jobs.<region>`) + Redis; reuses `internal/checker` but the real consume loop is later, here it just connects and idles.
   - `alerting`: Postgres + Kafka (consume `check.results`, produce `notify.events`) + Redis.
   - `notifier`: Postgres + Kafka (consume `notify.events`) + Redis; reuses `internal/notify` later.
4. Start the health server (`/healthz`, `/readyz`, `/metrics`).
5. Block on a context cancelled by SIGINT/SIGTERM, then shut down deps with a timeout.

Barebones acceptance: each binary builds, starts, reports `/readyz` 200 once its deps are reachable, and exits cleanly on signal. No business logic.

## 6. Local infrastructure

`docker-compose.yml` for local dev: Postgres 16, Redis 7, Redpanda (Kafka-API compatible, single container, far lighter than full Kafka+ZooKeeper for local work; production is managed MSK/Confluent per RFC-011, the client code is identical). A `.env.example` with the DSNs/addresses the services read.

Recommendation note: Redpanda for local and tests; managed Kafka in prod. The Kafka API is the same so `internal/bus` does not change between them.

## 7. Integration test (the "simple" one)

`test/integration/foundation_test.go`, behind an `//go:build integration` tag, using testcontainers-go to start Postgres + Redis + Redpanda in-process (works with the colima socket env vars already in the team setup). It asserts the whole foundation is wired:

1. `store.ApplySchema` runs clean against a fresh Postgres, and re-running it resets cleanly.
2. `pgxpool` ping succeeds; insert two orgs and a `widget` for each.
3. **RLS isolation**: connecting as the app role with `app.current_org` set to org A returns only org A's widget; a query under org A cannot see org B's row. This is the multi-tenant invariant proven.
4. Redis ping succeeds; `SetNX` lock acquire then release works.
5. Kafka round-trip: produce one message to a test topic keyed by `org_id`, consume it back, assert key/value/headers (including the correlation-id header) survive.
6. Boot one service's health server and assert `/readyz` returns 200 once deps are up.

Green means the foundation holds end to end. CI runs it; locally `make test-integration`.

## 8. Makefile and CI

- `make build` (all five binaries), `make test` (unit), `make test-integration` (tagged, testcontainers), `make schema` (apply schema to the compose Postgres), `make up` / `make down` / `make reset` (compose), `make lint` (gofmt/vet, golangci-lint).
- CI: build, unit tests, then the integration job with the testcontainers Docker setup. The cross-tenant isolation assertion in the integration test is the seed of the release-blocking isolation suite RFC-001 mandates.

## 9. Build order and acceptance per step

Each step is independently verifiable, so it can be reviewed before the next.

1. **Module + layout**: update `go.mod`, remove sqlite/auth/web/cmd-pulse, create empty package dirs. Accept: `go build ./...` compiles (with stub mains).
2. **obs**: logger + health server + metrics registry. Accept: a throwaway main serves `/healthz` and `/metrics`.
3. **config**: per-service `Load`. Accept: unit test for required/default/parse behavior (mirror the existing `config_test.go` style).
4. **store + schema.sql + WithOrg + RLS**: Accept: `WithOrg` works; the schema applies (and re-applies) cleanly in the integration test.
5. **redis**: client + lock/cache helpers. Accept: ping + lock in the integration test.
6. **bus**: producer/consumer + topic/key helpers. Accept: round-trip in the integration test.
7. **five service skeletons**: shared bootstrap + per-service dep wiring + graceful shutdown. Accept: each `/readyz` goes green against compose.
8. **docker-compose + Makefile + integration test + CI**: Accept: `make test-integration` passes locally and in CI.

After step 8 the foundation is done: five services that boot, connect, expose health/metrics, log structured lines, prove tenant isolation, and pass one end-to-end wiring test. Feature work (auth, scheduler, pipeline, API, multi-region) then builds on these packages against the RFC seams.

## Decisions worth a nod before building

- Redpanda for local/test Kafka (lighter; same API as managed Kafka in prod). If you'd rather mirror prod exactly, swap in the Confluent/Kafka compose images; the code is unaffected.
- testcontainers for the integration test (hermetic, CI-friendly, matches the team's existing colima setup) over a shared docker-compose the test assumes is already up.
- Leader election for the scheduler is deferred; the barebones scheduler runs as a single instance with a TODO. This is safe for groundwork since nothing dispatches yet.

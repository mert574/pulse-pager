# Pulse Pager

Multi-tenant SaaS uptime monitoring, built as distributed Go services on
Postgres + Redis + Kafka. See `docs/` for the product PRDs and technical RFCs.

This README is a brief dev guide for the current state of the repo. It will grow
as features land.

## Current state (read this first)

The repo is at the **foundation** stage. The five services boot, connect to their
dependencies, expose health/metrics, and shut down cleanly. The multi-tenant
isolation backbone (Postgres row-level security) is in place and tested. The
proven v1 leaf packages (`checker`, `alerting`, `notify`, `crypto`, `domain`)
carry forward.

What is **not** built yet: the HTTP API, authentication, the scheduler/worker/
alerting/notifier business logic, status pages. The service `main`s are skeletons
with a `TODO(RFC-xxx)` marking where each one's real logic goes.

Practically: you can run the services and watch them connect and report healthy.
You cannot log in to the web app yet (see "Frontend" below).

## Prerequisites

- Go (see `go.mod` for the version)
- Docker (this project uses colima, not Docker Desktop, see "Tests")
- Node + npm (for the frontend dev server)

## Quick start (backend)

Bring up local infra (Postgres, Redis, Redpanda):

```sh
make up
```

Load env and apply the schema:

```sh
cp .env.example .env
set -a; source .env; set +a
make schema
```

There are no migrations yet (we are early; `make schema` drops and recreates the
tables, so it doubles as a reset). To wipe the data entirely, `make reset` (down
with volumes, then up) and `make schema` again.

Run a service (each reads the same env; override the health port per service):

```sh
PULSE_HEALTH_ADDR=:8080 go run ./cmd/api
```

In another shell, check it is up:

```sh
curl -s localhost:8080/healthz   # -> ok
curl -s localhost:8080/readyz    # -> {"status":"ready"} once Postgres/Redis/Kafka are reachable
curl -s localhost:8080/metrics   # Prometheus metrics
```

The other services run the same way on different ports, for example:

```sh
PULSE_HEALTH_ADDR=:8081 go run ./cmd/scheduler
PULSE_HEALTH_ADDR=:8082 PULSE_REGION=home go run ./cmd/worker
PULSE_HEALTH_ADDR=:8083 go run ./cmd/alerting
PULSE_HEALTH_ADDR=:8084 go run ./cmd/notifier
```

Shut infra down (with volumes):

```sh
make down
```

## Frontend (and getting past login in dev)

The Lit SPA lives in `web/`. Real authentication (Google/GitHub OIDC) is not
built yet (RFC-003). To browse the app in development there is a **dev-auth
mode**: the `api` service runs a self-contained stub that fakes the session and
serves sample data. It needs no Postgres/Redis/Kafka.

Two terminals:

```sh
# 1. dev API on :8080 (fake auth + sample monitors/channels/incidents)
PULSE_DEV_AUTH=true go run ./cmd/api

# 2. the SPA dev server, which proxies /api, /auth, /healthz to :8080
cd web && npm install && npm run dev   # http://localhost:5173
```

Open http://localhost:5173, click "Sign in with Google" (or GitHub). The stub
sets the session cookie, redirects you back, and the app loads with a few sample
monitors (one up, one down, one disabled). Creating/editing monitors and channels
works in-memory for the session.

This is dev-only and clearly throwaway: it bypasses real auth and never touches a
database. It exists so the UI is browsable before RFC-003 (auth) and RFC-012 (the
real REST API) land. Do not run it in production.

## Tests

Unit tests:

```sh
make test
```

Integration test (spins up Postgres, Redis, Redpanda with testcontainers and
asserts the whole foundation: schema apply, RLS isolation, Redis, Kafka, health).
This project runs Docker via colima, so export the socket overrides:

```sh
export DOCKER_HOST=unix:///Users/$USER/.colima/default/docker.sock
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
make test-integration
```

## Layout

```
api/openapi/    v1.yaml: the API contract, single source of truth (RFC-012)
cmd/            the five services (api, scheduler, worker, alerting, notifier) + schema
internal/
  apigen/       generated from v1.yaml (oapi-codegen): models + strict server interface
  devapi/       dev stub implementing the generated contract with sample data
  obs/          logger, Prometheus registry, tracing, health server
  config/       per-service env config
  store/        pgxpool, schema.sql + ApplySchema, the WithOrg org-scoping helper (RLS)
  kv/           fast key-value store (Redis): lock/cache helpers
  bus/          Kafka (franz-go) producer/consumer + topic/key helpers
  runtime/      shared service bootstrap
  domain/ checker/ alerting/ notify/ crypto/   reused v1 leaf packages
web/            Lit SPA; web/src/api/schema.d.ts is generated from v1.yaml
docs/           PRDs, RFCs, planning, and the implementation plan
```

## API contract (generated)

The REST API is **contract-first** (RFC-012): `api/openapi/v1.yaml` is the single
source of truth. Both the Go server types/stubs (`internal/apigen`) and the
TypeScript client types (`web/src/api/schema.d.ts`) are generated from it, so the
backend and frontend cannot drift. The dev stub implements the same generated
`StrictServerInterface` the real api will, so it is contract-conformant, not
throwaway. In dev, the spec is served at `/api/v1/openapi.json` and Swagger UI at
`/api/docs`.

```sh
make gen          # regenerate Go + TS after editing v1.yaml
make gen-check    # CI: spec lints + generated files match (no drift)
```

See `docs/IMPLEMENTATION-PLAN-foundation.md` for what the foundation covers and
`docs/CODE-VS-RFC-GAP.md` for what is still to build.

## License

Copyright (c) 2026 Mert Yildiz.

Pulse Pager is source-available under the Elastic License 2.0 (ELv2). The full
text is in [LICENSE](LICENSE). In short: you can use, copy, modify, and
self-host it, but you may not offer it to others as a hosted or managed service,
and you may not bypass the plan/license gating. For a commercial license outside
these terms, contact the copyright holder.

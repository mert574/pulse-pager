# Pulse Pager

Developer-first uptime monitoring that pages you the moment something breaks.
Know before your customers do.

Multi-tenant SaaS, built as five distributed Go services on Postgres, Redis, and
Kafka (Redpanda locally), with a Lit single-page app in `web/`. The REST API is
contract-first: `api/openapi/v1.yaml` is the single source of truth and both the
Go and TypeScript types are generated from it.

Source-available under the Elastic License 2.0. See [License](#license).

## Current state

The whole product loop runs end to end: register an endpoint, check it on a
schedule from one or more regions, detect failures, open and close incidents,
notify, recover. You can sign in, create monitors, watch per-region checks, get
alerted on real channels, publish public status pages, manage your org, and stay
within your plan's limits.

What works:

- **Auth and orgs** (RFC-003): Google/GitHub OAuth, JWT sessions with refresh,
  API keys, multi-tenant orgs with roles (owner/admin/member/viewer) and
  invitations. Postgres row-level security means one org can never read another's
  rows, even if an app-level filter is missed.
- **Monitors and checks** (PRD-002, RFC-004/005): HTTP/TCP checks with
  interval, timeout, expected status codes, latency and body assertions;
  per-region scheduling; manual check-now with a rate limit; live per-region
  state; and recent-check history grouped one row per run.
- **Pipeline** (RFC-005/006/007): scheduler then worker then alerting then
  notifier, over Kafka. Workers run per region, alerting opens and closes
  incidents, the notifier delivers.
- **Channels** (PRD-003, RFC-007a): nine integrations (Slack, Discord, webhook,
  email/SMTP, Telegram, PagerDuty, Opsgenie, Microsoft Teams, Twilio), each with
  a config schema and a test-send. Secret config is encrypted at rest.
- **Plans and entitlements** (PRD-006): free/starter/team/business with monitor
  caps, interval floors, region sets, seat caps, status-page caps, and per-plan
  channel access, all enforced server-side, plus a usage-vs-caps screen.
- **Status pages** (PRD-004): public per-org status pages with monitors,
  incidents, and a quiet "Powered by Pulse Pager" credit.
- **Multi-region** (RFC-013): a monitor can run from several regions; the UI
  groups a run's regions into one row and shows a live chip per region.

Still early or not built yet:

- Multi-region verdict aggregation (the `down_policy`/quorum window) sits behind
  a single-region seam in alerting: today each region's result is its own
  verdict, the cross-region reduce is not wired yet.
- Billing is usage and plan-catalog display only. Payments (Stripe) are not
  wired, so plans are set by an operator for now.
- No versioned migrations. `make schema` drops and recreates the tables, which
  also doubles as the dev reset.
- Deployment (Helm/Terraform/k8s), SLO dashboards, enterprise SSO, and extra
  check types (SSL-expiry, heartbeat) are not built.

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

There are no migrations yet. `make schema` drops and recreates the tables, so it
doubles as a reset. To wipe data entirely, `make reset` (down with volumes, then
up) and `make schema` again.

Run the control-plane API:

```sh
PULSE_HEALTH_ADDR=:8080 go run ./cmd/api
```

Run the check pipeline (each service on its own health port):

```sh
PULSE_HEALTH_ADDR=:8082 go run ./cmd/scheduler
PULSE_HEALTH_ADDR=:8083 PULSE_REGION=home go run ./cmd/worker
PULSE_HEALTH_ADDR=:8084 go run ./cmd/alerting
PULSE_HEALTH_ADDR=:8085 go run ./cmd/notifier
```

For multi-region, run another worker with a different `PULSE_REGION` (for example
`eu-west`) on its own port. It consumes that region's jobs from
`check.jobs.<region>`, so adding a region to a monitor starts checking from it
with no redeploy.

Check a service is up:

```sh
curl -s localhost:8080/healthz   # -> ok
curl -s localhost:8080/readyz    # -> {"status":"ready"} once Postgres/Redis/Kafka are reachable
curl -s localhost:8080/metrics   # Prometheus metrics
```

Shut infra down (with volumes):

```sh
make down
```

## Frontend (and getting past login in dev)

The Lit SPA lives in `web/`. Two easy ways to browse it without setting up OAuth:

```sh
# Option A: dev-auth stub. Self-contained fake session + sample data, no infra.
PULSE_DEV_AUTH=true go run ./cmd/api

# the SPA dev server, which proxies /api, /auth, /healthz to :8080
cd web && npm install && npm run dev   # http://localhost:5173
```

Option B is the real api against Postgres/Redis/Kafka, with one of:

- a configured OAuth provider (`PULSE_GOOGLE_*` or `PULSE_GITHUB_*` in `.env`),
  then sign in with that provider, or
- `PULSE_DEV_LOGIN=true`, which enables a guarded `POST /auth/dev/login` so you
  can sign in locally without an OAuth provider.

The dev-auth stub is dev-only and never touches a database. Do not run it in
production.

## Tests

Unit tests:

```sh
make test
```

Integration tests spin up Postgres, Redis, and Redpanda with testcontainers and
exercise the real stack: schema + RLS isolation, the auth flows, the API
(monitors, channels, members, status pages, entitlements), and the check
pipeline end to end. This project runs Docker via colima, so export the socket
overrides:

```sh
export DOCKER_HOST=unix:///Users/$USER/.colima/default/docker.sock
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
make test-integration
```

Frontend tests (web-test-runner, in `web/`):

```sh
cd web && npm test          # run once
npm run typecheck           # tsc --noEmit
```

## Layout

```
api/openapi/    v1.yaml: the API contract, single source of truth (RFC-012)
cmd/            the five services (api, scheduler, worker, alerting, notifier) + schema
internal/
  api/          control-plane HTTP server implementing the generated contract
  apigen/       generated from v1.yaml (oapi-codegen): models + strict server interface
  devapi/       dev stub implementing the same contract with sample data
  authn/        oauth login, jwt issue + jwks, refresh rotation, api keys, middleware
  authz/        role model and Can checks
  entitlements/ per-plan caps, floors, and feature access (PRD-006)
  store/        pgxpool, schema.sql + ApplySchema, the WithOrg org-scoping helper (RLS)
  scheduler/    dispatches per-(monitor, region) check jobs
  worker/       runs checks for its region and emits results
  alerting/     opens/closes incidents from results, emits notify events
  notify/       channel providers (slack, discord, smtp, pagerduty, opsgenie, ...)
  checkstate/   live per-(monitor, region) state in Redis
  bus/          Kafka (franz-go) producer/consumer + topic/key helpers
  kv/           Redis: lock/cache helpers
  config/       per-service env config       runtime/  shared service bootstrap
  obs/          logger, Prometheus registry, tracing, health server
  domain/ checker/ crypto/   dependency-light leaf packages
web/            Lit SPA; web/src/api/schema.d.ts is generated from v1.yaml
test/           testcontainers integration suite
docs-site/      static, source-available docs site (GitHub Pages)
```

## API contract (generated)

The REST API is contract-first (RFC-012): `api/openapi/v1.yaml` is the single
source of truth. Both the Go server types/stubs (`internal/apigen`) and the
TypeScript client types (`web/src/api/schema.d.ts`) are generated from it, so the
backend and frontend cannot drift. The dev stub implements the same generated
`StrictServerInterface` the real api does, so it stays contract-conformant. In
dev, the spec is served at `/api/v1/openapi.json` and Swagger UI at `/api/docs`.

```sh
make gen          # regenerate Go + TS after editing v1.yaml
make gen-check    # CI: spec lints + generated files match (no drift)
```

Never hand-edit `internal/apigen/apigen.gen.go` or `web/src/api/schema.d.ts`.
Change the spec and regenerate.

## License

Copyright (c) 2026 Mert Yildiz.

Pulse Pager is source-available under the Elastic License 2.0 (ELv2). The full
text is in [LICENSE](LICENSE). In short: you can use, copy, modify, and
self-host it, but you may not offer it to others as a hosted or managed service,
and you may not bypass the plan/license gating. For a commercial license outside
these terms, contact the copyright holder.

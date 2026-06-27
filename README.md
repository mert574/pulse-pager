<p align="center">
  <img src="docs-site/assets/og.png" alt="Pulse Pager — developer-first uptime monitoring" width="720">
</p>

# Pulse Pager

Developer-first uptime monitoring that pages you the moment something breaks.
Know before your customers do.

Multi-tenant SaaS, built as a set of distributed Go services (a five-stage monitoring
pipeline plus a billing service) on Postgres, Redis, and a Kafka-compatible event bus
(Redpanda locally), with a Lit single-page app in `web/`. The REST API is
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

- **Auth and orgs**: Google/GitHub OAuth and passwordless magic-link login, JWT
  sessions, API keys, and multi-tenant orgs with roles and email invites. Postgres
  row-level security isolates each org's data even if an app-level filter is missed.
- **Monitors and checks**: HTTP and SSL-expiry checks with per-region scheduling,
  status/latency/body assertions, rate-limited manual check-now, live per-region
  state, and recent-check history.
- **Pipeline**: scheduler → worker → alerting → notifier over a pluggable bus
  (`PULSE_BUS`: Kafka, or Redis Streams for a single node). The notifier is the only
  thing that sends mail (alerts plus invite and magic-link), minting tokens at send
  time so none ride the bus.
- **Channels**: ten types (Slack, Discord, webhook, SMTP, Team email, Telegram,
  PagerDuty, Opsgenie, Teams, Twilio), each with a test-send and secrets encrypted at
  rest; platform email can split account vs alert mail across From subdomains.
- **Plans and entitlements**: Free/Hobby/Professional/Custom caps (monitors, interval,
  regions, seats, status pages, channels), enforced server-side, with a usage screen.
- **Status pages**: public per-org status pages with monitors and incident history.
- **Multi-region**: a monitor can run from several regions, with a live chip per region.

Still early or not built yet:

- Multi-region verdict aggregation (the `down_policy`/quorum window) sits behind
  a single-region seam in alerting: today each region's result is its own
  verdict, the cross-region reduce is not wired yet.
- Billing (RFC-018): the provider-agnostic core, the plan catalog, and the
  subscription + trial state synced from a provider's webhooks are built, with a stub
  provider that exercises the whole sync path without an account. Live Paddle checkout
  and the customer portal are not wired yet, so an operator sets plans for now.
- Deployment (Helm/Terraform/k8s), SLO dashboards, enterprise SSO, and the
  cron/heartbeat check type are not built yet.

## Prerequisites

- Go (see `go.mod` for the version)
- Docker (this project uses colima, not Docker Desktop, see "Tests")
- Node + npm (for the frontend dev server)

## Quick start (backend)

Bring up local infra (Postgres, Redis, Redpanda):

```sh
make up
```

Set up your env and the database:

```sh
cp .env.example .env
make schema     # bootstrap a fresh db: the frozen baseline + every migration
```

`make schema`, `make migrate`, and `make run` source `.env` for you. `make schema`
bootstraps an empty database (baseline + all migrations) and refuses to run against
an already-initialized one. Day-to-day schema changes go through goose migrations in
`internal/store/migrations/`: `make migrate` applies pending ones to a real database
(forward-only, never drops data), and `make migrate-create name=<snake_case>` scaffolds
a new one. To wipe a dev db and start clean, `make reset` then `make schema`.

Run the control-plane API (`make run` sources `.env` and serves on `:8080`):

```sh
make run
# or, raw: set -a; . ./.env; set +a; go run ./cmd/api
```

Run the check pipeline (each service on its own health port):

```sh
PULSE_HEALTH_ADDR=:9082 go run ./cmd/scheduler
PULSE_HEALTH_ADDR=:9083 go run ./cmd/worker
PULSE_HEALTH_ADDR=:9084 go run ./cmd/alerting
PULSE_HEALTH_ADDR=:9085 go run ./cmd/notifier
```

For multi-region, run another worker with a different `PULSE_REGION` (for example
`us-west`) on its own port. It consumes that region's jobs from
`check.jobs.<region>`, so adding a region to a monitor starts checking from it
with no redeploy.

The services talk over Kafka by default. For a single node with no broker, set
`PULSE_BUS=redis` and they use Redis Streams instead (see `.env.example`).

Check a service is up:

```sh
curl -s localhost:9080/healthz   # -> ok
curl -s localhost:9080/readyz    # -> {"status":"ready"} once Postgres/Redis/Kafka are reachable
curl -s localhost:9080/metrics   # Prometheus metrics
```

Shut infra down (with volumes):

```sh
make down
```

## Frontend (and getting past login in dev)

The Lit SPA lives in `web/`. Two easy ways to browse it without setting up OAuth:

```sh
# Option A: dev-auth stub. Self-contained fake session + sample data, no infra.
make dev   # serves on :8080 to match the SPA dev-server proxy

# the SPA dev server, which proxies /api, /auth to :8080 and /healthz to :9080
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
api/openapi/    v1.yaml: the API contract, single source of truth
cmd/            five pipeline services (api, scheduler, worker, alerting, notifier), the billing service, + utilities (schema, migrate)
internal/
  api/          control-plane HTTP server implementing the generated contract
  apigen/       generated from v1.yaml (oapi-codegen): models + strict server interface
  devapi/       dev stub implementing the same contract with sample data
  authn/        oauth login, jwt issue + jwks, refresh rotation, api keys, middleware
  authz/        role model and Can checks
  entitlements/ per-plan caps, floors, and feature access
  billing/      provider-agnostic recurring payments (RFC-018): stub + Paddle adapter
  store/        pgxpool, schema.sql + ApplySchema, the WithOrg org-scoping helper (RLS)
  scheduler/    dispatches per-(monitor, region) check jobs
  worker/       runs checks for its region and emits results
  alerting/     opens/closes incidents from results, emits notify events
  notify/       channel providers (slack, discord, smtp, pagerduty, opsgenie, ...)
  checkstate/   live per-(monitor, region) state in Redis
  bus/          event bus: Kafka (franz-go) or Redis Streams, selected by PULSE_BUS
  kv/           Redis: lock/cache helpers
  config/       per-service env config       runtime/  shared service bootstrap
  obs/          logger, Prometheus registry, tracing, health server
  domain/ checker/ crypto/   dependency-light leaf packages
web/            Lit SPA; web/src/api/schema.d.ts is generated from v1.yaml
test/           testcontainers integration suite
docs-site/      static, source-available docs site (GitHub Pages)
```

## API contract (generated)

The REST API is contract-first: `api/openapi/v1.yaml` is the single
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

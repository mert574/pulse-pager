# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Pulse is multi-tenant SaaS uptime monitoring, built as five distributed Go services on Postgres + Redis + a pluggable event bus (Kafka/Redpanda by default, or Redis Streams via `PULSE_BUS`). The repo is at the foundation stage: the services boot, connect, and report healthy; much business logic is still skeletons marked `TODO(RFC-xxx)`. The web app is a Lit SPA in `web/`.

Authoritative design lives in `docs/` (PRDs and RFCs). Code comments reference these by id (e.g. `RFC-003`, `PRD-006`); follow the cited doc when changing that area.

## Commands

```sh
make build              # build all five services into bin/
make test               # go unit tests (./...)
make test-integration   # testcontainers suite (needs Docker, see below)
make lint               # gofmt -l . and go vet ./...
make migrate            # apply pending migrations (forward-only, never drops data)
make migrate-create name=add_widget   # scaffold a new goose migration
make schema             # bootstrap an EMPTY db (baseline + migrations); refuses on a populated db
make up / make down     # docker compose infra up / down (with volumes)
make reset              # down -v then up (wipe the docker dev db)
```

## Database changes go through migrations (mandatory)

All schema changes are migrations. Never edit `internal/store/schema.sql` to change the
schema, and never hand-run DDL against a real database. The workflow:

1. `make migrate-create name=<snake_case>` scaffolds a goose SQL file in
   `internal/store/migrations/` with `-- +goose Up` / `-- +goose Down` sections (wrap
   functions/`DO`/dollar-quoted blocks in `-- +goose StatementBegin`/`StatementEnd`).
2. Write the forward change in Up and its reverse in Down.
3. `make migrate` applies pending migrations to `PULSE_POSTGRES_DSN`. It is forward-only
   and never drops data; goose records applied versions in `goose_db_version`.

`schema.sql` is the FROZEN baseline (the from-empty starting point) plus `migrations/`
on top: `ApplySchema` (used by tests and `make schema` on a fresh db) applies the
baseline then every migration. `make schema` is for standing up a brand-new empty
database only and refuses to run against an initialized one (it would drop tables);
`PULSE_FORCE_RESET=true make schema` is the explicit, destructive dev rebuild. There is
no quick "drop everything" command for a real database by design.

Run one Go test: `go test ./internal/api/ -run TestName -v`
Run one integration test: `go test -tags integration -run TestName ./test/integration/`

Frontend (in `web/`): `npm run dev` (Vite on :5173, proxies /api,/auth to the api on :8080 and /healthz to the health server on :9080), `npm test` (web-test-runner), `npm run typecheck`, `npm run build`.

Service ports (canonical): app ports default `:8080` (api 8080, billing 8081), health/metrics default `:9080` (api 9080, billing 9081, scheduler 9082, worker 9083, alerting 9084, notifier 9085). Defaults are uniform (8080/9080) so any one service runs without overrides; override `PULSE_API_ADDR` / `PULSE_BILLING_ADDR` / `PULSE_HEALTH_ADDR` per service to run several on one host.

### Integration tests need colima socket overrides

Docker here is colima, not Docker Desktop. Export these on the same command (the README repeats this):

```sh
export DOCKER_HOST=unix:///Users/$USER/.colima/default/docker.sock
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock
make test-integration
```

**Gotcha: `make test` (`go test ./...`) does NOT compile the `//go:build integration`
files in `test/integration/`.** The build tag excludes them, so a change to a shared
type (a `bus.Record` field, an `api`/`store`/`config`/`events` signature) can break the
integration suite's compilation while the unit gate stays green. After touching shared
types, run `go vet -tags integration ./...` (or `./test/integration/`) to catch compile
breaks, then `make test-integration` (with the colima env above) to actually run them.
Each test spins its own testcontainers Postgres; the full suite is ~46s.

## Contract-first API (do this, do not hand-edit generated files)

`api/openapi/v1.yaml` is the single source of truth (RFC-012). Both the Go server types/stubs (`internal/apigen/apigen.gen.go`) and the TypeScript client types (`web/src/api/schema.d.ts`) are generated from it, so backend and frontend cannot drift.

After editing `v1.yaml`:

```sh
make gen        # regenerate Go (oapi-codegen) + TS (openapi-typescript)
make gen-check  # CI check: spec lints (spectral) + generated files match a fresh regen
```

Never edit `apigen.gen.go` or `schema.d.ts` by hand; change the spec and regenerate. Handlers implement the generated `StrictServerInterface`.

## Architecture

### Services and shared bootstrap

The five binaries live in `cmd/{api,scheduler,worker,alerting,notifier}` (plus `cmd/schema`). Each `main` is thin: it calls `runtime.Setup` then `runtime.Run`. `internal/runtime` (`runtime.go`) is the shared bootstrap: it loads config, builds the logger + Prometheus registry, connects only the deps that service needs (each registered as a `/readyz` check and a reverse-order shutdown closer), serves health/metrics, and shuts down on SIGINT/SIGTERM.

`internal/config` declares per-service dependency needs (`serviceNeeds` map): a service fails closed at boot if a required env var is missing, so e.g. the worker does not need a Postgres DSN if it does not use one. All config is `PULSE_*` env vars (see `.env.example`).

### Multi-tenant isolation (two layers, both required)

Tenant isolation is app-level org scoping **plus** Postgres row-level security (RFC-001 6.1). Every tenant query goes through `store.Pool.WithOrg(ctx, orgID, fn)` (`internal/store/store.go`), which opens a transaction, sets `app.current_org` via `set_config(..., true)`, and runs `fn(tx)`. RLS policies key off that session var, so a missed app-level filter fails safe instead of leaking another org's rows. When adding tenant data access, use `WithOrg`; do not query tenant tables on the bare pool.

`internal/store` owns the pgxpool, the frozen baseline `schema.sql` + goose `migrations/` (`ApplySchema` for a fresh db, `MigrateUp` for a real one), and one file per entity. Secret columns (e.g. secret monitor headers) are encrypted at rest via a `secretCipher` wired with `SetCipher` (`internal/crypto`); nil cipher = stored as-is (dev/test).

### Event pipeline (pluggable bus: Kafka or Redis Streams)

`internal/bus` wraps the producer/consumer and defines the canonical topics (`bus.go`): `monitor.changed`, `check.jobs.<region>` (per-region, via `CheckJobsTopic`), `check.results`, `notify.events`, `audit.events`, `billing.events`, `region.health`. Payloads are JSON structs in `internal/events` (kept dependency-light). Data flow: scheduler emits `CheckJob` (carries full monitor config so workers never hit Postgres on the hot path) → worker runs the check, emits `CheckResultEvent` → alerting opens/closes incidents, emits `NotifyEvent` → notifier delivers. `internal/kv` is the Redis layer (locks/cache).

The transport is pluggable behind a backend interface, selected by `PULSE_BUS`: `kafka` (default, `kafka.go`, franz-go against any Kafka-compatible broker) or `redis` (`redis.go`, Redis Streams, a single-node mode that reuses Redis instead of a separate broker). Both keep the same at-least-once contract (a handler that returns an error leaves the message unacked for redelivery). The runtime picks the backend in `ConnectBus{Producer,Consumer}`; services are unaffected since they depend on their own small `Producer`/`Consumer` interfaces.

### Identity / auth / authz (api service only)

- `internal/authn` is the auth machinery: OAuth login (Google/GitHub via go-oidc), JWT issue + JWKS, refresh-token rotation, cookies, API-key verification, and the `Authenticator` middleware (`Identify` for authenticated routes, `RequireOrg` for `/orgs/{orgId}` routes). The principal is read with `authn.FromContext(ctx)`.
- `internal/authz` is the role model and `Can` checks; actor kinds (`ActorHuman`, etc.).
- `internal/api` is the real control-plane HTTP server implementing the generated contract. `api.Build`/`New` wire it; `router.go` mounts hand-wired auth-plane routes (`/auth/*` and `/.well-known/jwks.json`, which are redirects/non-JSON, kept out of the spec) and the generated JSON resource routes behind the right middleware. Errors go out as the localizable `{code, message}` envelope (helpers at the bottom of `api.go`).
- `internal/entitlements` covers per-plan limits (seats, monitor caps, interval floor, feature flags). Handlers take resolver interfaces that default to sane per-plan resolvers when nil.

### Dev-auth mode (browse the SPA without infra)

`PULSE_DEV_AUTH=true go run ./cmd/api` runs `internal/devapi`: a self-contained stub that fakes the session and serves sample data, no Postgres/Redis/Kafka. It implements the same generated `StrictServerInterface` as the real api, so it stays contract-conformant. Dev-only, never in production.

### Reused leaf packages

`internal/{domain,checker,alerting,notify,crypto}` are dependency-light packages carried forward from v1. `checker` runs the actual HTTP/TCP checks (with an SSRF guard in `ssrf.go`, on when `PULSE_BLOCK_PRIVATE_NETWORKS`). `notify` has one file per provider (slack, discord, pagerduty, twilio, smtp, etc.) plus a registry/catalog.

### Web (Lit SPA)

`web/src` is a Lit + TypeScript SPA: `components/` (custom elements, each with a colocated `.test.ts`), `state/` (session + `can` permission checks via `@lit/context`), `api/client.ts` (typed against the generated `schema.d.ts`), `router.ts`, `i18n.ts`. There is a separate public status page entry (`web/src/status/`, `web/status.html`).

## Conventions

- Use the dedicated file tools (Read/Edit/Glob/Grep) over shell `cat`/`grep`/`sed`.
- Match the existing comment style: comments explain *why* and cite the RFC/PRD, in plain language. No em-dashes.
- `make lint` must be clean (`gofmt`, `go vet`) before committing. Only commit/push when explicitly asked.
- Adding a tenant entity: write a migration (`make migrate-create`) with the table + its RLS policy, a `store/<entity>.go` accessor using `WithOrg`, the spec in `v1.yaml`, then `make gen`. Do not edit `schema.sql`.

## Parallel work safety

Other agents are often working in this tree at the same time. Never run commands that throw away or overwrite files you did not write in this session: no `git checkout HEAD -- <file>`, `git reset --hard`, `git stash`, or copying a saved snapshot back over a file. These wipe in-flight work from other agents, and a backup/restore dance is not safe because minutes pass between the steps. To commit only your own change, stage just your hunks with `git add -p` (or `git apply --cached` of a patch you built); leave every other file and every other hunk untouched.

# Contributing

Pulse Pager is a Go backend with a Lit/TypeScript frontend. Start with the
[README](README.md) for setup.

## Local development

1. Copy `.env.example` to `.env` and use local development values.
2. For a frontend preview with synthetic data and no external services, use
   `make dev` and run `npm ci && npm run dev` in `web/`.
3. For backend development, bring up local Postgres, Redis, and Redpanda as
   described in the README. Use a fresh disposable database.

The dev-auth API uses synthetic data; it does not establish that a production
deployment contains no personal data. Never point development tools at a live
customer database or payment account.

## Changes and checks

- Describe the behavior being changed, its scope, and how to verify it.
- Edit `api/openapi/v1.yaml` first for API changes and run `make gen`; do not
  hand-edit generated clients/server types.
- Make schema changes through new migrations; leave the frozen baseline alone.
- Run `make lint` and `make test`. For shared types, also run
  `go vet -tags integration ./...` and the relevant integration tests.
- In `web/`, run `npm run typecheck`, `npm test`, and `npm run build` for frontend
  behavior changes. Browser tests need a supported browser.
- Update the relevant RFC, README, and roadmap when behavior or scope changes.

## Rights and data

Original project contributions use the MIT License in `LICENSE`. Keep the
licenses and notices of dependencies and third-party fonts. Only contribute code
and assets you have the right to share. Use synthetic records in examples and
tests. Keep secrets out of commits, screenshots, logs, and issue reports.

# Architecture Decision Records

Short, standalone records of the load-bearing decisions behind Pulse. Each captures one decision already made in the RFCs: context, options considered, the decision, and consequences. The RFCs hold the full reasoning; these are the index of what was decided and why.

| ADR | Title | Status |
|-----|-------|--------|
| [ADR-0001](ADR-0001-shared-db-rls-tenancy.md) | Shared-database row-level multi-tenancy with Postgres RLS | Accepted |
| [ADR-0002](ADR-0002-postgres-pgx-golang-migrate.md) | PostgreSQL via pgx + goose | Accepted |
| [ADR-0003](ADR-0003-kafka-client-franz-go.md) | Kafka client is franz-go | Accepted |
| [ADR-0004](ADR-0004-scheduler-leader-election-k8s-lease.md) | Scheduler singleton via Kubernetes Lease | Accepted |
| [ADR-0005](ADR-0005-rs256-jwt-jwks.md) | RS256 JWT access tokens with JWKS, identity-only token | Accepted |
| [ADR-0006](ADR-0006-multi-region-messaging-shape.md) | Multi-region messaging shape (regional Kafka, results mirrored home) | Accepted |
| [ADR-0007](ADR-0007-check-results-partitioning-rollups.md) | check_results range partitioning with rollups | Accepted |
| [ADR-0008](ADR-0008-entitlements-library-not-service.md) | Entitlements as a library with Redis cache, not a service | Accepted |
| [ADR-0009](ADR-0009-at-least-once-idempotent-consumers.md) | At-least-once delivery with idempotent consumers | Accepted |
| [ADR-0010](ADR-0010-networkpolicy-tls-no-mesh.md) | NetworkPolicy plus TLS-to-infra for service trust, mesh deferred | Accepted |
| [ADR-0011](ADR-0011-worker-emits-results-no-postgres-write.md) | Worker emits results only; control-plane consumer persists | Accepted |
| [ADR-0012](ADR-0012-api-key-sha256-redis.md) | API key authentication via SHA-256 and a Redis cache | Accepted |
| [ADR-0013](ADR-0013-bigint-pk-prefixed-external-ids.md) | BIGINT primary keys with prefixed external ids | Accepted |
| [ADR-0014](ADR-0014-single-leader-scheduler-heap.md) | Single leader-elected in-memory scheduler heap for v1 | Accepted |
| [ADR-0015](ADR-0015-event-serialization-json-envelope.md) | JSON event serialization with a versioned envelope | Accepted |
| [ADR-0016](ADR-0016-ssrf-always-on.md) | SSRF protection always-on, not customer-disableable | Accepted |
| [ADR-0017](ADR-0017-frontend-stack-lit-tailwind.md) | Frontend stack (Lit light DOM, Tailwind, owned Swiss tokens, uPlot, TanStack Table) | Accepted |
| [ADR-0018](ADR-0018-icu-messageformat-localizable-string.md) | ICU MessageFormat and the localizable-string API convention | Accepted |
| [ADR-0019](ADR-0019-server-i18n-go-i18n-v2.md) | Server-side i18n library for Go (go-i18n v2) | Accepted |
| [ADR-0020](ADR-0020-frontend-i18n-lit-localize.md) | Frontend i18n library (@lit/localize) | Accepted |
| [ADR-0021](ADR-0021-controller-processor-gdpr-control-set.md) | Data controller/processor model and the GDPR control set | Accepted |
| [ADR-0022](ADR-0022-data-residency-approach.md) | Data residency approach (account PII in the home region) | Accepted |
| [ADR-0023](ADR-0023-deletion-cascade-backup-exception.md) | Deletion cascade with the backup-retention exception | Accepted |
| [ADR-0024](ADR-0024-build-vs-buy-enterprise-sso.md) | Build-vs-buy enterprise SSO is buy, with an in-house escape hatch | Accepted |
| [ADR-0025](ADR-0025-sso-support-set-saml-oidc.md) | Enterprise SSO support set is SAML 2.0 plus OIDC | Accepted |
| [ADR-0026](ADR-0026-scim-provisioning-model.md) | SCIM 2.0 provisioning with JIT fallback and fast deprovisioning | Accepted |

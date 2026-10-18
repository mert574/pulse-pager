# Pulse Pager roadmap

The distributed check pipeline, org/auth flows, HTTP and SSL-expiry checks,
notification channels, status pages, entitlement checks, billing core, and
observability configuration are implemented. This is a working development
plan, not a claim that every item in the PRDs/RFCs has shipped.

Recheck scope against the current implementation before starting an item and
remove completed work from this roadmap.

## Next priorities

| Work | Done when |
| --- | --- |
| Multi-region verdict aggregation | `down_policy` reduces results across a bounded window; tests cover disagreement, stale/missing regions, recovery, and duplicate delivery without duplicate incidents. |
| Heartbeat monitoring | A monitor has an authenticated ingest URL, expected period and grace window; late/missing/recovered events flow through incident and notification handling; tests use a controlled clock. |
| Check-result retention and rollups | A migration and bounded job enforce retention; rollups preserve historical uptime; org isolation and empty/time-boundary cases have integration coverage. |
| Notification-channel setup guides | Each supported channel has a configuration guide and a synthetic test-send example; fields match the channel registry and help links reach the correct guide. |

These priorities focus on monitoring behavior and reproducible development.

## Later

- Evaluate regional worker health and failover after verdict aggregation.
- Complete the remaining infrastructure layout in RFC-011; distinguish it from
  the observability configuration that is already present.
- Validate live payment-provider flows separately with provider sandbox accounts.
- Revisit enterprise SSO and SCIM after the core monitoring work.

## Review and validation

Keep the implementation, OpenAPI contract, tests, and matching docs in sync.
Run unit tests and frontend type checking/builds for affected code. Shared backend
types also need the integration-tag compilation check and relevant container tests.
Use generated/synthetic tenants, endpoints, identities, and payment events in
examples and development. Do not copy production records or credentials into
fixtures, logs, issues, or this repository.

Design context: [PLANNING](docs/PLANNING.md), [PRD](docs/PRD.md),
[backlog](docs/BACKLOG.md), and the RFCs linked from those documents.

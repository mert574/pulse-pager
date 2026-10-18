# Pulse Pager roadmap

## Next priorities

| Work | Done when |
| --- | --- |
| Multi-region verdict aggregation | `down_policy` combines regional results within a bounded window. Tests cover disagreement, stale or missing regions, recovery, and duplicate delivery. |
| Heartbeat monitoring | Monitors have authenticated ingest URLs, expected periods, and grace windows. Late, missing, and recovered heartbeats drive incidents and notifications, with controlled-clock tests. |
| Check-result retention and rollups | A migration and bounded job enforce retention while rollups preserve historical uptime. Integration tests cover org isolation and time boundaries. |
| Notification-channel setup guides | Each supported channel has a configuration guide and a synthetic test-send example matching its current fields. |

## Later

- Regional worker health and failover after verdict aggregation.
- Remaining deployment infrastructure from RFC-011.
- Payment-provider checkout and portal validation with sandbox accounts.
- Enterprise SSO and SCIM.

## Validation

Keep implementation, API contracts, tests, and docs in sync. Run backend tests,
frontend checks, and integration tests for affected behavior. Use synthetic data
and disposable development services.

Design context: [PLANNING](docs/PLANNING.md), [PRD](docs/PRD.md),
[backlog](docs/BACKLOG.md), and their linked RFCs.

# ADR-0011: Worker emits results only; control-plane consumer persists

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-005 section 5.3, RFC-006 section 5.4, RFC-000 sections 2.3 and 4.1, CONSISTENCY-REVIEW C-1, ADR-0006, ADR-0009

## Context
Workers live in regional data planes; Postgres lives in the control plane. The topology rule is that nothing in a regional data plane holds durable state and workers consume locally so a region-to-home partition does not stall checking. A worker writing check_results directly to control-plane Postgres would open a cross-region database connection on every check (10k/sec times regions) and couple in-region checking to home-region DB health. The consistency review flagged that RFC-000 2.3 and master PRD 6.6 literally said the worker writes Postgres, which RFC-005 and RFC-006 deliberately changed.

## Options considered
- Worker emits check.results only; a control-plane consumer does the idempotent upsert, folded into the alerting transaction - no new cross-region path (results are already mirrored home), in-region checking survives a home-DB blip.
- Worker writes Postgres directly and emits the event after (literal RFC-000 2.3) - a synchronous cross-region DB write per check, high egress and latency, and a home-DB blip stalls checking in every region.
- A stateful store per region - breaks the single-source-of-truth tenancy model and the no-durable-state-in-data-plane invariant for no benefit at control-plane-in-one-region scale.

## Decision
Ratify the RFC-005/RFC-006 model: the worker emits check.results to its regional Kafka and does not write the check_results row to Postgres. A single control-plane consumer (result-persister, next to Postgres) consumes the mirrored check.results, performs the idempotent upsert keyed (org_id, monitor_id, region, checked_at), and the alerting verdict is applied in the same transaction. RFC-000 section 2.3 / 5.3 and master PRD 6.6 are amended to match.

## Consequences
The durable row and the alerting trigger share one write path and cannot diverge under partial failure, which was RFC-000's original concern, and the worker does not write the check_results row at all (alerting owns that write). The worker does keep a small per-monitor snapshot in Postgres (last-failure and cert info), so the rule is specifically that the check_results firehose stays off the worker's Postgres path, not that the worker touches no Postgres. No new cross-region transport is added because check.results is already mirrored home for alerting to consume. A home-region DB blip backs up the persister consumer (Kafka buffers it) instead of stalling checking in every region. The cost is one extra control-plane consumer and that result_id is assigned at persist time, not at worker time. Redelivery stays safe via the unique-key upsert (ADR-0009). This supersedes the literal worker-writes-Postgres wording across the doc set.

# ADR-0009: At-least-once delivery with idempotent consumers

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 sections 5.3 and 8, RFC-002 section 5, RFC-006, ADR-0003, ADR-0011

## Context
Every Kafka topic in Pulse can redeliver an event after a consumer crash. The hazards are running a check twice, re-applying a result so an incident double-opens or double-closes, and delivering the same alert twice. We need exactly-once behavior in effect without paying the coordination cost of Kafka transactions.

## Options considered
- At-least-once delivery with idempotent consumers - our consumers can be made naturally idempotent, so a redelivered event lands on no-op writes. No transaction coordinator.
- Kafka exactly-once semantics (transactions) - the broker guarantees it, but transactions add coordination cost and tie consume-process-produce into transactional units we do not otherwise need.

## Decision
Delivery is at-least-once everywhere; idempotency lives in the consumers. The durable check_results upsert is keyed by a unique (org_id, monitor_id, region, checked_at), so a redelivered result upserts the same row as a no-op. In the same transaction the alerting transition is idempotent: an incident open is conditioned on no open incident existing (enforced by the partial unique index on open incidents), a close is conditioned on the incident still being open, and alert-counter updates are guarded by the triggering result id. notify.events carry a stable dedup id of hash(incident_id, event_type) that notifier records in Redis with a Postgres backstop. Every producer includes a stable idempotency key; every consumer is safe to run twice. The franz-go idempotent producer (ADR-0003) prevents broker-side double-append on produce retry.

## Consequences
We get one-down, one-up per incident under redelivery without a transaction coordinator. The contract is binding on every producer (include a stable key) and every consumer (be safe to run twice), so new topics inherit a clear rule. The cost is that each consumer must carry its own dedup mechanism (unique constraint, conditional write, or dedup-id set) rather than leaning on the broker. This is separate from the HTTP-level Idempotency-Key the public API remembers for 24h, which solves write dedup, not consumer redelivery. Revisit only if a future pipeline genuinely needs transactional exactly-once that idempotency cannot express.

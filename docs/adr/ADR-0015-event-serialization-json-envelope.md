# ADR-0015: JSON event serialization with a versioned envelope

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 5, RFC-002 sections 4.1 and 5, ADR-0003, ADR-0006

## Context
Every Kafka event needs a wire format. The firehose is check.results at ~10k/sec, each a few hundred bytes. Events cross the regional-to-control-plane mirror, and the debug story for results and notifications matters in operations. We want evolution to stay safe without standing up new stateful infrastructure for v1.

## Options considered
- JSON with a versioned envelope, additive-only evolution, no schema registry - human-readable across the mirror and in logs, zero extra infra, version field gives a clean upgrade path.
- Avro plus a Confluent schema registry - strongest governance, but adds a stateful service to run and reach across the mirror boundary and makes every event unreadable without the registry.
- protobuf - compact and typed, but binary on the wire hurts the results and notify debug story.

## Decision
Events are JSON (UTF-8) with a common envelope on every message: schema, version, event_id (ULID), occurred_at (RFC3339 UTC), org_id, followed by a topic-specific payload. Evolution is additive only; an absent value is JSON null, never an omitted key. No schema registry, no Avro, no protobuf in v1. internal/bus stamps the envelope uniformly.

## Consequences
Events stay readable across the mirror and in logs with no extra infrastructure to run or reach, which keeps the operational and debug story simple. At our volume the throughput cost of JSON is acceptable (single-digit to low-tens MB/sec is well within franz-go and a managed broker). The version field plus additive-only rule gives a clean path to a registry later with no data migration. The cost is no enforced schema governance, so additive-only discipline is on producers. Revisit (Avro plus registry) when the schema count grows or external consumers need formal governance.

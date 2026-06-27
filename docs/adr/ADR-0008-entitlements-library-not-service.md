# ADR-0008: Entitlements as a library with Redis cache, not a service

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 sections 2.6 and 12, RFC-009, ADR-0001

## Context
Entitlement checks happen on two hot paths: api on every metered write and scheduler on every dispatch. Both need a fast "what is org X allowed" lookup. Entitlement data is small and changes rarely (only on plan change), and the two enforcement points must not trust each other.

## Options considered
- Shared internal/entitlements library backed by a Redis cache with Postgres as source of truth - sub-millisecond library call on the hot path, no extra hop, no over-fragmentation.
- Standalone entitlements service - independent deploy cadence and isolation, but adds a network hop and a new failure mode to the two most latency-sensitive paths, for a component that does one cached read.

## Decision
Entitlements is a shared internal/entitlements library used by api (enforce on write) and scheduler (enforce on dispatch), serving from Redis with Postgres as the source of truth. It is not a standalone service. Invalidation is event-driven and per-org: a plan change (Stripe webhook to billing.events, or an internal admin change) invalidates the org's cached entitlement. Two enforcement points: api on write fails closed (if entitlements cannot be determined, the write is rejected so a downgrade can never be bypassed by knocking over the cache); scheduler on dispatch holds the last known snapshot it rebuilt from Postgres on boot rather than dispatching wide-open.

## Consequences
The hot paths never pay a Postgres read per request or per check, and we avoid a sixth service that would add ceremony and a network failure mode. The two enforcement points stay independent, so a monitor created under a higher plan cannot keep running faster or in richer regions after a downgrade. The cost is that cache invalidation correctness now lives in the library and its callers rather than behind a service boundary. The library interface is the seam: if a future need appears (for example a complex usage-metering engine), a service can sit behind the same interface without changing callers. Revisit when entitlement logic needs independent deploy cadence or holds its own large dataset.

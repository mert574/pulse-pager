# ADR-0006: Multi-region messaging shape (regional Kafka, results mirrored home)

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 sections 4.1 and 4.2, RFC-002 section 7, RFC-008, ADR-0011

## Context
The control plane runs in one home region; each operated region runs only a worker fleet. Check jobs are born in the scheduler and must reach the workers that run them, and results must flow back to the control plane where all aggregation happens. Workers must keep draining their job queue even if the link to home is briefly slow, and a regional broker incident must not take down the central bus that api, alerting, and notifier depend on.

## Options considered
- Regional Kafka cluster per data-plane region; check.jobs.<region> produced into the regional cluster and consumed locally; check.results and region.health mirrored back to the central cluster via MirrorMaker 2 - local low-latency consume, contained blast radius, bounded egress.
- Single central Kafka, region-keyed partitions, workers consume cross-region - every worker poll crosses the region boundary; latency and egress heavy, and a home-region blip stalls checking everywhere.
- Region-scoped topics on one central cluster consumed remotely - same cross-region consume problem; topic naming alone does not move the broker closer to the worker.
- Full active-active Kafka mesh - over-built for control-plane-in-one-region; operational cost not justified yet.

## Decision
Regional Kafka clusters per data-plane region. The control plane writes a region's jobs into that region's cluster, regional workers consume locally, and check.results plus region heartbeats are mirrored from each regional cluster back to the central control-plane cluster (MirrorMaker 2 or the managed equivalent) where alerting consumes them.

## Consequences
Consume is always local for workers, so a region-to-home partition does not stall in-region checking, and a regional broker incident does not take down the central bus. Results tolerate a short mirror delay, which is comfortably within the 5s decision-latency SLO. The cost is cross-region egress to mirror results home, bounded by mirroring only check.results and heartbeats (not the full job stream) and kept off free traffic because premium regions are a higher-tier entitlement. At Phase 0 and 1 there is one region so there is no mirror and no egress, but the topic-per-region naming and the mirror seam exist from day one so multi-region GA is additive, never a migration. Revisit toward an active-active mesh only when a multi-region control plane is on the table.

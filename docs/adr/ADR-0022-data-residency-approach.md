# ADR-0022: Data residency approach

Status: Accepted
Date: 2026-06-22
Deciders: Architecture
Related: RFC-015 sections 5, RFC-008 sections 2, 3.4, 11, ADR-0006

## Context
Pulse runs checks from regional data planes but keeps account state in a control-plane home region. GDPR and enterprise buyers ask where PII lives and whether running checks in other regions moves customer PII across borders. The answer has to be concrete about what actually crosses regions, and an EU data-residency option for account data is a phased enterprise ask that should not force re-architecture.

## Options considered
- Keep all account/identity PII in the control-plane home region, move no customer PII across regions, and treat EU residency as a phased additive variant (chosen). Only `check.results` and `region.health` mirror home, and neither carries customer PII; the larger job payload that carries the monitor's secret header is produced into the target region and never leaves it. Regional data planes hold no durable product state.
- Enforce per-customer data residency in v1 (results stay in the customer's region, account data per-region) - rejected for v1. It re-architects the control-plane / regional-data-plane split and the home-region aggregation before there is enterprise demand, for a phase-3 ask. The substrate (region attribution on every result, the plane split) already supports building it later by changing what mirrors home.
- Replicate account PII into every region for latency - rejected. It multiplies the PII surface and the cross-border transfer exposure for no product benefit, since account data is not on a latency-sensitive regional path.

## Decision
v1/GA does not enforce data residency: results flow home to the central Postgres for aggregation regardless of customer. All account/identity PII, billing identity, and the audit log live in the control-plane single home region. No customer PII crosses regions: `check.jobs` (config plus a secret header) is produced into the target region's Kafka and never leaves it, and only `check.results` (`monitor_id`, `region`, `checked_at`, `healthy`, `failure_reason`) and `region.health` (status/liveness) mirror home, neither carrying customer PII. An EU data-residency option for account data is phased (master section 15, phase 3, alongside enterprise); it is an additive variant that changes what mirrors home, not the topology, built on the region attribution and the control-plane / regional-data-plane split already in place.

## Consequences
Multi-region check execution does not move customer PII across borders today, which is a clean answer for GDPR and enterprise data-flow questions: PII stays in the control-plane home region. The phased EU-residency option does not require re-architecting the messaging shape, only changing what mirrors home, because the region attribution and the plane split are already the substrate. The cost is that v1 has no per-customer residency guarantee (results aggregate centrally), so a customer with a hard EU-only requirement is a phase-3 conversation, not a v1 capability. International transfers rely on the subprocessor list and SCCs (legal steps named in ADR-0021), not on technical residency, until the residency variant ships.

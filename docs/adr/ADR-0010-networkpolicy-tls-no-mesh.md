# ADR-0010: NetworkPolicy plus TLS-to-infra for service trust, mesh deferred

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 7.2, RFC-011, RFC-005 section 8, ADR-0016

## Context
The internal call graph is small and mostly flows through Kafka rather than direct service-to-service HTTP. The realistic threat is a compromised pod reaching a store or a network it should not, including a worker being used for SSRF. We need a v1 trust model that covers that without the operational weight of a service mesh.

## Options considered
- NetworkPolicy (default-deny) plus TLS on every infra connection - covers the realistic threat, no new runtime, treats the cluster network as the trust boundary.
- Service mesh with mTLS from day one (Istio/Linkerd) - gives cryptographic pod-to-pod identity, but heavy for five services that barely call each other directly.

## Decision
For v1, rely on Kubernetes NetworkPolicy with a default-deny posture to restrict which services reach which (api and the infra endpoints; workers only to Kafka and controlled outbound), plus TLS on every infra connection (Postgres, Redis, Kafka). Treat the cluster network as the trust boundary. Do not deploy a service mesh. Worker pods get egress NetworkPolicy controls so an SSRF bypass still cannot reach internal services (ADR-0016).

## Consequences
The realistic threat is covered without the weight of running a mesh, and identity is propagated as data on events (org_id on the work item) rather than as a token re-verified at every hop. The cost is that we do not have cryptographic pod identity in v1, so trust rests on network reachability and TLS to infra. The mesh question has a defined trigger: revisit when the service count grows or when the compliance posture (SOC 2) demands cryptographic pod identity. RFC-011 provisions the NetworkPolicies and TLS.

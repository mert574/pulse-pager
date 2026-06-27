# ADR-0004: Scheduler singleton via Kubernetes Lease

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 11.2, RFC-004 section 2, RFC-011, ADR-0014

## Context
The scheduler must run as exactly one active replica so two schedulers never double-dispatch checks. This is a must-never-split guarantee. It already runs in Kubernetes, with warm standbys that acquire leadership on failover and rebuild the in-memory heap from Postgres.

## Options considered
- Kubernetes Lease via client-go leaderelection - the lease-lock backed by a coordination.k8s.io/Lease object; well-trodden acquire/renew/observe loop, no new dependency, leadership tied to the same control plane that schedules the pods.
- Redis lock (SET NX PX with renewal) - we already run Redis, but it puts the singleton guarantee on a cache we otherwise treat as fail-open; a Redis blip could cause two leaders or none, and red-lock-style correctness under failover is fiddly.

## Decision
Leader election uses client-go's leaderelection package with a Kubernetes Lease lock. Run 2 or 3 replicas; standbys keep the election loop trying to acquire and build the heap only on promotion.

## Consequences
The one guarantee that must never split is tied to the Kubernetes control plane, not to a fail-open cache. The renew/acquire/observe loop and failover semantics are handled for us. Redis stays for coordination that tolerates a brief lapse (check-now locks, dedup), never for singleton-ness. The cost is a failover gap of roughly LeaseDuration plus heap rebuild (on the order of 15 to 20 seconds), during which some checks are delayed but none lost because next-run is recomputed from interval and last dispatch. Revisit if the scheduler is ever sharded (ADR-0014), in which case each shard runs its own Lease.

# ADR-0014: Single leader-elected in-memory scheduler heap for v1

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 2.2, RFC-004 sections 3.3 and 3.6, ADR-0004

## Context
The scheduler decides which checks are due and fans out one job per (monitor, region). v1 scale is 500k monitors and roughly 10k region-jobs/sec. The dispatch work is light (pop, clamp, produce to Kafka); the heavy work is the HTTP checks on the worker fleet, which scale horizontally. The v1 design is a single goroutine owning a min-heap keyed by next-run, which carries forward in shape.

## Options considered
- One leader-elected scheduler holding one heap of all monitors - cheap to reason about, no cross-shard coordination, comfortably handles 500k monitors on one core.
- N scheduler shards from the start, each leader-elected, partitioning by hash(monitor_id) % N - more headroom, but N Leases, N rebuild paths, and N failover windows for a problem we do not have yet.
- Per-monitor tickers - a goroutine or timer per monitor does not scale to 500k.
- DB-poll or a Redis-backed schedule - poll granularity adds built-in lateness and pushes the schedule onto a store, losing the cadence-stable in-memory timer.

## Decision
Run one leader-elected scheduler with one in-memory min-heap for all monitors in v1, not sharded. A concrete trigger defines when to shard: dispatch-loop CPU sustained above ~70% on one core, the scheduling-lateness p99 histogram breaching 5s under normal (non-failover) load, or the monitor count crossing a memory milestone (for example more than 2M monitors). When triggered, shard by hash(monitor_id) % N (not org-hash, so a single large org does not land on one shard), matching the check.jobs partition key; each shard is an independent leader-elected heap identical in code with a shard-index filter on the boot query and on monitor.changed.

## Consequences
At 500k monitors the heap is ~20 MB of items plus a config side map (~0.5 to 1 GB) on a few-GB pod, and heap ops are trivial for one core, so one heap holds comfortably. We avoid the operational cost of N Leases and N failover windows before we need it. The cost is that throughput sits on a single goroutine, so the trigger thresholds must be watched (the schedule-size, busy-ratio, and lateness metrics feed them). The shard-by-hash path is defined now so sharding later is additive, not a redesign. Revisit when any trigger fires.

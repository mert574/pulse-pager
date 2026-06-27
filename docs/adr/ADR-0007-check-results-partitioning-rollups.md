# ADR-0007: check_results range partitioning with rollups

Status: Accepted
Date: 2026-06-21
Deciders: Architecture
Related: RFC-000 section 6.2, RFC-001 (partitioning and rollups), ADR-0002

## Context
check_results is the firehose: ~10k inserts/sec sustained across 500k monitors, with per-plan retention of 7, 30, 90, or 180 days. Status pages and history views read against this table on the hot path, and retention cleanup at this volume must not turn into vacuum churn from mass deletes.

## Options considered
- Monthly RANGE partitioning by checked_at with partition-drop retention, plus hourly rollups - retention is a cheap DDL drop, reads hit aggregates, partition count stays manageable.
- Weekly or daily partitions - finer drop granularity but many more partitions to manage and plan over.
- One big table with mass-DELETE retention - simplest schema but DELETE at this volume causes heavy vacuum churn and bloat.

## Decision
Range-partition check_results by checked_at on a monthly boundary. Retention is a partition DROP, not a DELETE. A background job computes hourly rollups per monitor per region into check_rollups. Status-page uptime is derived from incident durations plus rollups, never from raw check_results rows on the read path; raw rows age out by partition drop while rollups persist for the displayed uptime window. Read replicas absorb the read-heavy history, status-page, and dashboard paths.

## Consequences
Dropping a month is orders of magnitude cheaper than deleting rows and avoids vacuum churn. Status-page and history reads stay fast at retention scale because they read aggregates, not the firehose. The cost is partition lifecycle management (creating ahead, dropping behind) and a background rollup job that must keep up. Because uptime reads never touch raw rows, the read path is decoupled from the firehose write path. Revisit the partition granularity if monthly partitions grow too large to manage or if a plan needs retention finer than the partition boundary allows.

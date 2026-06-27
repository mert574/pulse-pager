# RFC-002 - Eventing and Kafka Contracts

Status: DRAFT for review
Author: Principal Architecture
Audience: every service author who produces or consumes a Pulse event
Owns (per RFC-000 section 13): the event schema registry and the idempotency-key contract for every topic in RFC-000 section 5.
Parent: `docs/rfc/RFC-000-architecture-overview.md` (sections 4 topology, 5 eventing, 8 consistency/ordering/idempotency, ADR-0003 Kafka client, ADR-0006 multi-region messaging, ADR-0009 at-least-once + idempotent consumers).
Product source: `docs/prd/PRD-002` (check/alert flow), `PRD-003` (notify payloads), `PRD-005` (outbound webhooks vs internal events), `PRD-006` (billing.events), `PRD-007` (region dimension, region.health).

House style: all timestamps are RFC3339 UTC on the wire. No em-dashes. Tables and code blocks over prose.

---

## 1. Overview and scope

This RFC turns the binding eventing rules in RFC-000 section 5 into exact, copy-pasteable contracts. It is the single source of truth for:

1. The Kafka client and the `internal/bus` wrapper API every service uses.
2. The topic catalog: name, producers, consumers, partition key, partition count, retention, cleanup policy, ordering, and per-region vs global placement.
3. The exact event payload schema for every topic (JSON example plus a field table).
4. The wire format and the schema-evolution rules.
5. The delivery semantics (at-least-once everywhere) and the per-consumer idempotency token that makes redelivery safe.
6. The cross-region mirror that carries `check.results` and `region.health` home.
7. Failure handling: lag, dead-letter, retry, rebalancing.
8. Capacity sizing against PRD-012 targets.

### 1.1 What this RFC owns vs delegates

| This RFC owns | Delegated to |
|---------------|--------------|
| The schema of every event and the version field | the producing service RFC builds the producer |
| The idempotency key carried by every event | the consuming service RFC implements the dedup |
| Topic names, partition keys, partition counts, retention, cleanup | RFC-011 provisions the clusters and runs the mirror |
| The `internal/bus` API surface | each service wires its handlers onto it |
| The mirror seam (topic naming across clusters, ordering through the mirror) | RFC-008 owns the regional topology, failover, and egress cost |

### 1.2 Conventions used in every schema

| Rule | Value |
|------|-------|
| Encoding | JSON, UTF-8 (decision in section 5) |
| Timestamps | RFC3339 UTC string, e.g. `2026-06-21T14:00:30Z` |
| IDs | int64 numeric on the wire (the domain `int64` ids); rendered without quotes |
| Money / counters | integers |
| Optional/nullable | the JSON key is always present; absent value is JSON `null`, never an omitted key |
| Envelope | every event has `schema`, `version`, `event_id`, `occurred_at`, `org_id` (section 4.1) |
| Headers | Kafka record headers carry trace context and the dedup key (section 2.4, 6) |

Note on ids: the domain structs in `internal/domain/domain.go` use `int64` ids today. PRD payloads that surface externally show prefixed string ids like `mon_123` / `inc_456` (PRD-003 section 4.3.1). Those prefixed strings are an api-layer rendering for the public webhook and REST surfaces (RFC-005/RFC-012). On the internal Kafka bus we carry the raw `int64` so producers and consumers do not parse a prefix on the hot path. The rendering boundary is api, not the bus. This is a deliberate deviation from the PRD's externally-shown id shape and is flagged here so RFC-005/RFC-012 own the int64 -> `mon_`/`inc_` rendering.

---

## 2. Kafka client choice (resolves RFC-000 ADR-0003)

### 2.1 Decision

Use **franz-go** (`github.com/twmb/franz-go`) as the single Go Kafka client for `internal/bus`.

### 2.2 Reasoning

| Requirement (from RFC-000) | Why franz-go wins |
|----------------------------|-------------------|
| Idempotent producer (ADR-0009 needs stable producer behavior under retry) | franz-go enables the idempotent producer by default (producer id + sequence numbers), so a producer retry does not create a duplicate at the broker. No manual config dance |
| No cgo (RFC-000 section 3: every image builds with `CGO_ENABLED=0` into a distroless/static image) | franz-go is pure Go. confluent-kafka-go wraps librdkafka via cgo and would break the static distroless build and cross-compilation |
| Consumer-group ergonomics (workers/alerting/notifier scale by adding group members on lag) | franz-go's group consumer handles rebalance, partition assignment, and commit cleanly with a single client; cooperative-sticky balancing is supported, which reduces stop-the-world rebalances when an HPA adds a pod |
| Maintenance and modern protocol support | franz-go tracks current Kafka protocol versions (KIP support), is actively maintained, and supports newer features (e.g. exactly-once primitives if ever needed) without a C dependency upgrade |
| One client for produce and consume | a single library for both keeps `internal/bus` small and avoids two mental models |

### 2.3 Rejected alternatives

| Client | Why rejected |
|--------|--------------|
| sarama (`IBM/sarama`) | Pure Go and widely used, but the idempotent-producer and consumer-group code paths have historically been the rough edges (manual offset/commit handling, sharper rebalance semantics). franz-go's API is cleaner for the at-least-once commit-after-process pattern we need, and its idempotent producer is the default rather than a careful opt-in |
| confluent-kafka-go (`confluentinc/confluent-kafka-go`) | The most battle-tested protocol implementation (librdkafka under it), but it is cgo. That conflicts directly with the `CGO_ENABLED=0` static-build constraint in RFC-000 section 3, complicates distroless images and cross-compiles, and adds a C toolchain to CI. The protocol robustness does not outweigh breaking our build story |
| segmentio/kafka-go | Pure Go and ergonomic, but its idempotent-producer story and group-rebalance maturity lag franz-go. We want the strongest pure-Go idempotent producer, which is franz-go |

This is the content for ADR-0003.

### 2.4 The `internal/bus` wrapper API

`internal/bus` wraps franz-go so services never touch raw franz-go types and so the envelope, headers, keying, trace propagation, and commit-after-process discipline are uniform. Sketch:

```go
package bus

// Topic is a typed topic name. Region-scoped topics are built with For().
type Topic string

const (
	TopicMonitorChanged Topic = "monitor.changed"
	TopicCheckResults   Topic = "check.results"
	TopicNotifyEvents   Topic = "notify.events"
	TopicAuditEvents    Topic = "audit.events"
	TopicBillingEvents  Topic = "billing.events"
	TopicRegionHealth   Topic = "region.health"
	TopicWebhookDeliver Topic = "webhook.delivery"
)

// CheckJobs returns the region-scoped jobs topic, e.g. "check.jobs.eu-west".
func CheckJobs(region string) Topic { return Topic("check.jobs." + region) }

// Envelope is stamped on every message body by Produce; see section 4.1.
type Envelope struct {
	Schema     string    `json:"schema"`      // e.g. "check.results"
	Version    int       `json:"version"`     // schema version, section 5
	EventID    string    `json:"event_id"`    // ULID, producer-generated, for tracing/logging
	OccurredAt time.Time `json:"occurred_at"` // when the source fact happened
	OrgID      int64     `json:"org_id"`
}

// ProduceOpts carries the partition key and the idempotency token header.
type ProduceOpts struct {
	Key         []byte // partition key bytes, section 3.2 helpers
	DedupKey    string // section 6; stamped into the "pulse-dedup-key" header
	TraceCtx    context.Context // trace + correlation id propagated via headers
}

// Producer publishes typed payloads. It JSON-marshals body, stamps the
// envelope fields the caller did not set, sets headers, and produces with the
// franz-go idempotent producer.
type Producer interface {
	Produce(ctx context.Context, t Topic, body any, opts ProduceOpts) error
	Close() error
}

// Handler processes one message. Returning nil commits the offset; returning a
// non-nil error does NOT commit, so the message is redelivered (at-least-once).
// A poison error (section 8) is signalled by wrapping with bus.Poison so the
// consumer routes it to the DLQ instead of looping.
type Handler func(ctx context.Context, m Message) error

// Message is the decoded record handed to a Handler.
type Message struct {
	Envelope Envelope
	Body     []byte // raw JSON; caller unmarshals into the typed payload
	Key      []byte
	DedupKey string
	Headers  map[string]string
}

// Consumer joins a consumer group and runs Handler per message with
// at-least-once, commit-after-process semantics.
type Consumer interface {
	// Run blocks, polling and dispatching to handler until ctx is done.
	Run(ctx context.Context, group string, topics []Topic, handler Handler) error
	Close() error
}
```

Key behaviors the wrapper guarantees:

| Concern | Behavior |
|---------|----------|
| Trace propagation | On produce, the OTel span context and the correlation id are injected into Kafka record headers (`traceparent`, `pulse-correlation-id`). On consume, the wrapper restores them into the handler's `ctx` and the slog logger, per RFC-000 section 9.2/9.3. One check is traceable scheduler -> worker -> alerting -> notifier across regions |
| Commit-after-process | Offsets commit only after the handler returns nil. A crash mid-handle leaves the offset uncommitted, so the message redelivers. This is the at-least-once spine of ADR-0009. We do NOT use auto-commit |
| Keying | The caller passes a partition key via `ProduceOpts.Key`; `bus` exposes typed helpers (`KeyMonitor(id)`, `KeyOrg(id)`, `KeyRegion(code)`) so a producer cannot accidentally key by the wrong field |
| Dedup header | `ProduceOpts.DedupKey` is stamped into the `pulse-dedup-key` header so a consumer can read the idempotency token without parsing the body. The token is ALSO in the body (section 6) so it survives mirroring and is auditable |
| Idempotent producer | enabled at client construction; a produce retry after a transient broker error does not double-append |
| Poison routing | a handler that returns `bus.Poison(err)` causes the wrapper to publish the raw record to the topic's DLQ and commit the original offset, so one bad message cannot block a partition (section 8) |

---

## 3. Topic catalog

### 3.1 The topics

| Topic | Scope | Producer | Consumer group(s) | Partition key | Cleanup | Default retention |
|-------|-------|----------|-------------------|---------------|---------|-------------------|
| `monitor.changed` | global (control) | api | `scheduler` | `org_id` | delete | 7 days |
| `check.jobs.<region>` | per-region | scheduler | `worker-<region>` | `monitor_id` | delete | 1 hour |
| `check.results` | global (control); fed by mirror | worker | `alerting` | `monitor_id` | delete | 24 hours |
| `notify.events` | global (control) | alerting | `notifier` | `monitor_id` | delete | 24 hours |
| `audit.events` | global (control) | api (+ any service taking an auditable action) | `audit-sink` | `org_id` | delete (long) | 90 days (see note) |
| `billing.events` | global (control) | api (Stripe webhook handler) | `entitlement-invalidator`, `billing-sink` | `org_id` | delete | 30 days |
| `region.health` | per-region produced, mirrored home | worker (heartbeats), region controller | `alerting`, `scheduler` | `region` | compact + delete | compact, plus 1 day delete |
| `webhook.delivery` | global (control) | api / alerting (org webhook fan-out) | `notifier` (webhook deliverer) | `org_id` | delete | 24 hours |

Notes on scope and placement:

- `check.jobs.<region>` lives on the **regional** Kafka cluster (RFC-000 ADR-0006). Workers consume locally and low-latency. There is one topic per operated region; the `<region>` suffix is the region code (`us-east`, `eu-west`, `ap-southeast`, per PRD-007 section 2).
- `check.results` and `region.health` are produced into the **regional** cluster and **mirrored** to the central control-plane cluster, where `alerting` and `scheduler` consume them (section 7). Their consumers run in the control plane.
- Everything else (`monitor.changed`, `notify.events`, `audit.events`, `billing.events`, `webhook.delivery`) is **control-plane only**; producer and consumer are both home-region services and never cross a region boundary.
- At Phase 0/1 there is one region (home) so there is no mirror and no egress; the topic-per-region naming and the mirror consumer group exist from day one so multi-region rollout is additive (RFC-000 section 4.2).

### 3.2 Partition-key strategy and justification

| Key | Topics | Why this key |
|-----|--------|--------------|
| `monitor_id` | `check.jobs`, `check.results`, `notify.events` | Per-monitor ordering is the load-bearing requirement. All results for one monitor land on one partition in arrival order, so `alerting` processes a monitor's checks in sequence and the reused pure state machine (`internal/alerting.Apply`) sees a coherent run. Cross-monitor parallelism is unbounded because the key space is the whole monitor set; per-monitor work is serialized, which is exactly what the state machine and the one-down/one-up contract need (RFC-000 section 5.2, PRD-002 section 4.7) |
| `org_id` | `monitor.changed`, `audit.events`, `billing.events`, `webhook.delivery` | Org-scoped streams must stay ordered per org. A create-then-edit on `monitor.changed` cannot reorder; two `billing.events` for one org (e.g. upgrade then immediate downgrade) apply to the entitlement cache in the order they happened. Cross-org parallelism is unbounded |
| `region` | `region.health` | Heartbeats for one region must be observed in order so the latest liveness wins; keying by region also lets the compacted topic keep one current value per region (section 4.7) |

Why not key `notify.events` by `incident_id`: an incident belongs to exactly one monitor, so keying by `monitor_id` keeps the down event and its recovery event on the same partition and in order, and it keeps `notify.events` co-partitioned with `check.results` for the same monitor. `incident_id` would also work for ordering a single incident's two events but loses the monitor-level locality and adds nothing, so we key by `monitor_id` (RFC-000 section 5.1 lists `monitor_id (or incident_id)`; we pick `monitor_id`).

### 3.3 Partition counts (guidance; RFC-011 provisions)

Sized against PRD-012 (~10k checks/sec sustained, 500k monitors) and the fan-out multiplier. Full sizing math is in section 9.

| Topic | Partitions (per cluster) | Reasoning |
|-------|--------------------------|-----------|
| `check.jobs.<region>` | 64 per region | jobs/sec into a region is checks/sec for that region; 64 gives headroom and lets the worker group scale to 64 consumers per region |
| `check.results` | 128 (central) | the firehose; aggregates all regions after mirror. 128 lets `alerting` run up to 128 consumers and keeps per-partition throughput comfortable |
| `notify.events` | 32 | notify volume is a small fraction of result volume (only incident open/close, not every check) |
| `monitor.changed` | 16 | config edits are low volume even at 500k monitors |
| `audit.events` | 16 | moderate; bursty on bulk actions |
| `billing.events` | 8 | very low volume (plan changes) |
| `region.health` | 16 | one key per region, low cardinality; partitions sized for compaction parallelism, not throughput |
| `webhook.delivery` | 16 | org-webhook fan-out, paid-tier only, moderate |

Partition counts can only grow, and growing repartitions the key space (breaking per-key ordering during the change), so these are set with headroom up front. RFC-011 owns the final numbers per environment.

### 3.4 Retention and cleanup justification

| Topic | Cleanup | Why |
|-------|---------|-----|
| `check.jobs.<region>` | delete, 1h | a job is only useful until shortly after its scheduled tick; `interval_seconds >= timeout_seconds` (PRD-002 section 3.6) means a job is stale within minutes. Short retention keeps the regional broker small. The durable schedule is in Postgres, rebuilt by the scheduler on boot, so a dropped job topic is not data loss |
| `check.results` | delete, 24h | the durable history is the Postgres `check_results` table written by the control-plane consumer (RFC-000 section 2.3, RFC-005 section 5.3); the Kafka topic is the alerting trigger and a short replay buffer, not the archive. 24h covers an `alerting` outage + catch-up |
| `notify.events` | delete, 24h | only needed until the notifier delivers; 24h covers a notifier backlog |
| `monitor.changed` | delete, 7d | scheduler rebuilds its whole schedule from Postgres on boot, so it does not need long history; 7d is a generous replay window for debugging |
| `audit.events` | delete, 90d | audit is consumed into Postgres for the durable trail (RFC-000 section 10). The topic retention is a replay buffer, not the system of record; it is intentionally independent of (and may be shorter than) the per-tier Postgres audit retention (Business keeps 365 days, RFC-001 / PRD-006), because Postgres (`audit_events`) is the durable record, not Kafka. See open question in section 10 on per-stream retention (login-event volume vs people-changes, RFC-000 open question 2) |
| `billing.events` | delete, 30d | low volume; 30d gives a long invalidation-replay window for debugging entitlement drift |
| `region.health` | compact + delete | compaction keeps the latest heartbeat per region key so a fresh `alerting`/`scheduler` consumer can read current liveness immediately on join, while the 1d delete bound stops compacted-but-old tombstones lingering forever |
| `webhook.delivery` | delete, 24h | delivery attempts are transient; the durable record is the delivery-outcome row in Postgres (PRD-005 section 7.4 last-delivery status) |

### 3.5 Ordering guarantees summary

| Guarantee | Topics | Mechanism |
|-----------|--------|-----------|
| Per-monitor total order | `check.results`, `check.jobs`, `notify.events` | single partition per `monitor_id`, single consumer per partition |
| Per-org total order | `monitor.changed`, `billing.events`, `audit.events`, `webhook.delivery` | single partition per `org_id` |
| Latest-wins per region | `region.health` | compaction by `region` key |
| No cross-key ordering | all | Kafka gives no order across partitions; consumers must not assume it (PRD-005 section 7.2 already tells webhook receivers the same) |

---

## 4. Event payload schemas

### 4.1 The common envelope

Every event body starts with the same envelope fields, then a topic-specific payload. The envelope is what makes evolution and dedup uniform.

| Field | Type | Req | Meaning |
|-------|------|-----|---------|
| `schema` | string | yes | logical schema name, equals the topic base name, e.g. `check.results` |
| `version` | int | yes | schema version for this `schema` (section 5) |
| `event_id` | string (ULID) | yes | unique per produced record; for tracing/log correlation, NOT the dedup key |
| `occurred_at` | string (RFC3339 UTC) | yes | when the underlying fact happened (e.g. the check ran), not when produced |
| `org_id` | int64 | yes | tenant scope, carried as data per RFC-000 section 7.1 |

The **dedup key** (the idempotency token) is topic-specific and lives in the payload AND in the `pulse-dedup-key` header (section 6). `event_id` is deliberately not the dedup key: `event_id` is unique per record (so a redelivery has the same `event_id` but a re-produce after a crash may differ), while the dedup key is a stable function of the underlying fact (so two independent produces of the same fact still collapse).

### 4.2 `monitor.changed`

Producer api, consumer scheduler. Carries the live schedule edit so the scheduler updates its in-memory heap without a Postgres reload (RFC-000 section 2.2). One event per create/update/enable/disable/delete.

```json
{
  "schema": "monitor.changed",
  "version": 1,
  "event_id": "01J8Z9V2K3...",
  "occurred_at": "2026-06-21T13:59:00Z",
  "org_id": 42,
  "change": "updated",
  "actor": { "kind": "user", "id": 1001 },
  "monitor": {
    "id": 5001,
    "name": "Prod API health",
    "url": "https://api.example.com/health",
    "method": "GET",
    "headers": [ { "key": "Authorization", "value": null, "secret": true },
                 { "key": "Accept", "value": "application/json", "secret": false } ],
    "body": "",
    "expected_status_codes": "200",
    "timeout_seconds": 10,
    "interval_seconds": 60,
    "enabled": true,
    "max_latency_ms": 800,
    "body_contains": "ok",
    "failure_threshold": 3,
    "channel_ids": [9001, 9002],
    "regions": ["eu-west", "us-east"],
    "down_policy": "quorum",
    "updated_at": "2026-06-21T13:59:00Z"
  }
}
```

| Field | Type | Req | Notes |
|-------|------|-----|-------|
| `change` | enum `created`/`updated`/`enabled`/`disabled`/`deleted` | yes | the lifecycle action |
| `actor.kind` | enum `user`/`api_key`/`system` | yes | who did it (RFC-000 open question 2: human vs automated must be distinguishable) |
| `actor.id` | int64 or null | yes | user id, api-key id, or null for `system` |
| `monitor` | object | yes on all but `deleted` | full monitor snapshot so the scheduler does not re-read Postgres |
| `monitor.id` | int64 | yes | always present, including on `deleted` (then the rest may be null) |
| `monitor.headers[].value` | string or null | yes | **secret header values are null on the wire**; the worker reads decrypted secrets from the job payload path that goes through `internal/crypto`, never from `monitor.changed`. Non-secret values are present (redaction discipline, RFC-000 section 10) |
| `monitor.regions` | array of region code strings | yes | drives the scheduler fan-out (PRD-007 section 4.1) |
| `monitor.down_policy` | enum `any`/`quorum`/`all` | yes | default `quorum` (PRD-002 section 2.2) |

On `deleted`, only `monitor.id` is guaranteed; the scheduler removes it from the heap.

### 4.3 `check.jobs.<region>`

Producer scheduler, consumer the region's worker fleet. One job per (monitor, region) per tick. Carries the monitor snapshot the worker needs so the worker never reads Postgres on the hot path (RFC-000 section 2.3). The fields a worker needs are exactly the PRD-002 section 2.2 check config.

```json
{
  "schema": "check.jobs",
  "version": 1,
  "event_id": "01J8Z9W4M7...",
  "occurred_at": "2026-06-21T14:00:00Z",
  "org_id": 42,
  "job_id": "5001:eu-west:1718978400",
  "monitor_id": 5001,
  "region": "eu-west",
  "scheduled_at": "2026-06-21T14:00:00Z",
  "check": {
    "url": "https://api.example.com/health",
    "method": "GET",
    "headers": [ { "key": "Authorization", "value": "Bearer s3cr3t", "secret": true },
                 { "key": "Accept", "value": "application/json", "secret": false } ],
    "body": "",
    "expected_status_codes": "200",
    "timeout_seconds": 10,
    "max_latency_ms": 800,
    "body_contains": "ok"
  }
}
```

| Field | Type | Req | Notes |
|-------|------|-----|-------|
| `job_id` | string | yes | stable id `<monitor_id>:<region>:<scheduled_at_unix>`; the worker's idempotency anchor (section 6) and the dedup key |
| `monitor_id` | int64 | yes | partition key |
| `region` | string | yes | the target region; equals the topic suffix |
| `scheduled_at` | string RFC3339 | yes | when this tick was due; the worker stamps the result `checked_at` from the actual run, but `scheduled_at` anchors the job id so a redelivery is the same job |
| `check.*` | object | yes | the PRD-002 section 2.2 config the worker executes: `url`, `method`, `headers`, `body`, `expected_status_codes`, `timeout_seconds`, `max_latency_ms` (null = no latency assertion), `body_contains` (null = no body assertion) |
| `check.headers[].value` | string | yes | **secret header values are present and decrypted here** because the worker must send them. This is the one place secrets ride the bus. The regional `check.jobs` topic is therefore TLS in transit (RFC-000 section 10) and short-retention (1h); secret values are never logged. This is a deliberate, bounded exposure and is flagged for the RFC-005 security pass |

Deviation flag: carrying decrypted secret header values on `check.jobs` is the minimal way to keep the worker off the Postgres hot path while still sending the customer's configured auth header. The alternative (worker decrypts from Postgres per check) reintroduces the hot-path read RFC-000 section 2.3 explicitly removed. The exposure is bounded to the regional jobs topic (TLS, 1h retention, no logging). RFC-005 owns the final call and may instead pass an encrypted blob plus a per-region unwrap key if the security review requires it; the schema field stays `headers[].value` either way.

### 4.4 `check.results`

Producer worker, consumer alerting. One result per executed check, region-tagged. Mirrors the `domain.CheckResult` struct plus `org_id` and `region` (RFC-000 section 14, PRD-002 section 3.4). The durable row is written to Postgres first; this event carries the row id so alerting is a pure reaction (RFC-000 section 2.3).

```json
{
  "schema": "check.results",
  "version": 1,
  "event_id": "01J8Z9X6P1...",
  "occurred_at": "2026-06-21T14:00:30Z",
  "org_id": 42,
  "result_id": 880123,
  "job_id": "5001:eu-west:1718978400",
  "scheduled_at": "2026-06-21T14:00:00Z",
  "monitor_id": 5001,
  "region": "eu-west",
  "checked_at": "2026-06-21T14:00:30Z",
  "healthy": false,
  "failure_reason": "status_mismatch",
  "status_code": 503,
  "latency_ms": 120,
  "error_text": null
}
```

| Field | Type | Req | Notes |
|-------|------|-----|-------|
| `result_id` | int64 | yes | the Postgres `check_results` row id; alerting uses it to guard counter updates (section 6) |
| `job_id` | string | yes | carried through from the job; lets the result be tied back to its job for tracing |
| `scheduled_at` | string RFC3339 UTC | yes | the tick this result belongs to, carried through from the job; RFC-006 and RFC-008 key the per-tick multi-region aggregation window off it (`agg:{monitor_id}:{scheduled_at}`) rather than parsing it out of `job_id` |
| `monitor_id` | int64 | yes | partition key |
| `region` | string | yes | the region the check ran from (PRD-002 section 3.4, PRD-007 section 4.2) |
| `checked_at` | string RFC3339 | yes | when the check ran; part of the result write unique key |
| `healthy` | bool | yes | the PRD-002 section 3.1 all-assertions-pass decision |
| `failure_reason` | enum or null | yes | null when healthy; otherwise one of the six values below (PRD-002 section 3.2) |
| `status_code` | int or null | yes | null on `blocked_target`/`connection_error`/`timeout` |
| `latency_ms` | int or null | yes | null when no response was measured |
| `error_text` | string or null | yes | short, truncated transport detail; never a full body |

`failure_reason` enum (exact, PRD-002 section 3.2, priority order `blocked_target -> connection_error -> timeout -> status_mismatch -> latency_exceeded -> body_assertion_failed`): `blocked_target`, `connection_error`, `timeout`, `status_mismatch`, `latency_exceeded`, `body_assertion_failed`. These match `domain.FailureReason` verbatim.

The dedup key for this event is `(monitor_id, region, checked_at)` (section 6), which is also the Postgres unique key.

### 4.5 `notify.events`

Producer alerting, consumer notifier. Emitted when an incident opens (down) or closes by recovery (recovery). Disabled and manual closes do NOT emit a recovery event (PRD-002 section 4.5, 6.4; PRD-003 section 4.1). The payload carries everything the notifier needs so it does not re-read alerting state; channel config it still loads from Postgres (decrypted) per RFC-000 section 2.5. The shape mirrors the reused `notify.Event` (Monitor, Incident, Check, DurationSeconds) in `internal/notify/notify.go`.

Down event:

```json
{
  "schema": "notify.events",
  "version": 1,
  "event_id": "01J8Z9Y8R5...",
  "occurred_at": "2026-06-21T14:01:30Z",
  "org_id": 42,
  "dedup_key": "a3f5c9...e1",
  "event_type": "down",
  "monitor": { "id": 5001, "name": "Prod API health", "url": "https://api.example.com/health", "method": "GET" },
  "incident": { "id": 70001, "started_at": "2026-06-21T14:00:00Z", "ended_at": null, "cause": "status_mismatch" },
  "check": { "checked_at": "2026-06-21T14:01:30Z", "healthy": false, "failure_reason": "status_mismatch", "status_code": 503, "latency_ms": 120, "error_text": null },
  "duration_seconds": null,
  "channel_ids": [9001, 9002],
  "regions_observed_unhealthy": ["eu-west", "us-east"]
}
```

Recovery event (differences only): `event_type` `recovery`, `incident.ended_at` set, `check.healthy` true, `check.failure_reason` null, `duration_seconds` set to `ended_at - started_at` (PRD-003 section 4.2).

| Field | Type | Req | Notes |
|-------|------|-----|-------|
| `dedup_key` | string | yes | `hex(sha256(incident_id, event_type))`; the notifier dedup token (section 6, PRD-003 section 5) |
| `event_type` | enum `down`/`recovery` | yes | matches `notify.EventDown`/`notify.EventRecovery` |
| `monitor` | object | yes | `id`, `name`, `url`, `method` (PRD-003 section 4.3.1) |
| `incident.id` | int64 | yes | the open/closed incident |
| `incident.started_at` | string RFC3339 | yes | first-fail-of-run time (PRD-002 section 4.3) |
| `incident.ended_at` | string RFC3339 or null | yes | null on down, set on recovery |
| `incident.cause` | enum (failure_reason) | yes | why the incident opened |
| `check` | object | yes | the triggering result fields the notifier renders (`Reason: status_mismatch (HTTP 503)`) |
| `duration_seconds` | int or null | yes | null on down; outage length on recovery |
| `channel_ids` | array int64 | yes | the monitor's attached channels; a zero-length array still emits (the incident still opens/closes, PRD-002 section 4.8) and the notifier sends nothing |
| `regions_observed_unhealthy` | array string | yes | which regions saw it down (PRD-007 section 4.2); additive human-readable line in the body (PRD-003 section 7), not part of the locked appendix-B envelope |

Deviation flag: PRD-003 section 4.3 locks the outbound generic-webhook envelope byte-for-byte (appendix B). `notify.events` is the **internal** event, not the outbound payload; the notifier renders the locked appendix-B body from these fields at delivery time. `regions_observed_unhealthy` is carried internally and only surfaces as an extra human-readable line, never as a new locked-envelope field, exactly as PRD-003 section 7 requires.

### 4.6 `region.health`

Producer worker heartbeats and the region controller, consumer alerting and scheduler. Keyed by region, compacted so the latest liveness per region is always readable. Detailed measurement is RFC-008; this RFC fixes the wire schema so RFC-008 and the consumers agree.

```json
{
  "schema": "region.health",
  "version": 1,
  "event_id": "01J8Z9Z0T9...",
  "occurred_at": "2026-06-21T14:00:05Z",
  "org_id": 0,
  "region": "eu-west",
  "status": "healthy",
  "healthy_workers": 12,
  "reason": null,
  "lifecycle_state": "available"
}
```

| Field | Type | Req | Notes |
|-------|------|-----|-------|
| `org_id` | int64 | yes | `0` (platform/global; this is not org-scoped, but the envelope field stays present and is zero) |
| `region` | string | yes | partition + compaction key (PRD-007 section 2) |
| `status` | enum `healthy`/`degraded`/`unhealthy` | yes | drives the down-policy denominator: a region not `healthy` is excluded from `R` (PRD-007 section 5.2, 6.2) |
| `healthy_workers` | int | yes | count of live workers backing the region; lets alerting/scheduler reason about thinning coverage |
| `reason` | string or null | yes | short text when not healthy (e.g. `no_heartbeat_30s`) |
| `lifecycle_state` | enum `available`/`deprecated`/`retired` | yes | region catalog lifecycle (PRD-007 section 2.1); `retired` means stop dispatching |

Liveness is recency-based: alerting/scheduler treat a region as effectively `unhealthy` if the latest `region.health` for that region is older than a staleness bound (RFC-008 sets the bound; PRD-007 does not fix a heartbeat detection-latency SLO). Compaction guarantees a freshly-started consumer reads the current value per region on join.

### 4.7 `billing.events`

Producer api (Stripe webhook handler and internal admin changes), consumers `entitlement-invalidator` (invalidates the per-org Redis entitlement cache) and `billing-sink` (durable billing audit). Keyed by org so two changes for one org stay ordered (PRD-006 section 5.3, RFC-000 section 12).

```json
{
  "schema": "billing.events",
  "version": 1,
  "event_id": "01J8ZA12V3...",
  "occurred_at": "2026-06-21T14:05:00Z",
  "org_id": 42,
  "event_type": "subscription_updated",
  "plan": "team",
  "subscription_status": "active",
  "seats_purchased": 5,
  "source": "stripe",
  "stripe_event_id": "evt_1Px..."
}
```

| Field | Type | Req | Notes |
|-------|------|-----|-------|
| `event_type` | enum | yes | `subscription_created`/`subscription_updated`/`payment_failed`/`subscription_canceled`/`subscription_renewed`/`override` (PRD-006 section 8.1, 5.3) |
| `plan` | enum `tier1`/`tier2`/`tier3`/`tierCustom` | yes | the new plan tier after the change (PRD-006 section 3) |
| `subscription_status` | enum `active`/`past_due`/`canceled`/`trialing` | yes | drives the banner and the drop-to-free path (PRD-006 section 2.3, 10.3) |
| `seats_purchased` | int | yes | paid add-on seat count |
| `source` | enum `stripe`/`admin` | yes | so an internal override is distinguishable from a Stripe-driven change |
| `stripe_event_id` | string or null | yes | the Stripe event id when `source=stripe`, for dedup against Stripe's own at-least-once webhooks; null for `admin` |

The `entitlement-invalidator` consumer invalidates the org's Redis entitlement key on every `billing.events` record so the next entitlement read repopulates from Postgres (RFC-000 section 12). Invalidation is idempotent by nature (deleting an already-deleted cache key is a no-op), so redelivery is safe with no extra token; `stripe_event_id` additionally lets api drop a Stripe webhook it already processed before producing.

### 4.8 `audit.events`

Producer api (and any service taking an auditable action), consumer `audit-sink` (writes the append-only Postgres trail) and the api read path. Keyed by org. Append-only (RFC-000 section 10, PRD-006 section 3 retention by tier).

```json
{
  "schema": "audit.events",
  "version": 1,
  "event_id": "01J8ZA34X7...",
  "occurred_at": "2026-06-21T14:06:00Z",
  "org_id": 42,
  "actor": { "kind": "user", "id": 1001, "ip": "203.0.113.7" },
  "action": "monitor.updated",
  "target": { "type": "monitor", "id": 5001 },
  "metadata": { "fields_changed": ["interval_seconds", "regions"] }
}
```

| Field | Type | Req | Notes |
|-------|------|-----|-------|
| `actor.kind` | enum `user`/`api_key`/`system` | yes | human vs automated must be distinguishable (RFC-000 open question 2) |
| `actor.id` | int64 or null | yes | user/api-key id, null for system |
| `actor.ip` | string or null | yes | source ip when known (the "from-where" in RFC-000 section 6.0) |
| `action` | string | yes | dotted action name, e.g. `monitor.updated`, `member.role_changed`, `incident.closed_manual`, `auth.login` |
| `target.type` | string | yes | the resource kind |
| `target.id` | int64 or null | yes | the resource id |
| `metadata` | object | no | free-form action context; no secrets |

The dedup key is `event_id` itself (the producer generates one ULID per auditable action; an append of the same `event_id` is a no-op upsert on the audit table's unique `event_id`). Audit is append-only and order-per-org matters, not exactly-once-effect on a state machine, so this lighter rule suffices.

### 4.9 `webhook.delivery` (justified addition)

Producer api/alerting (the org-webhook fan-out), consumer the notifier's webhook deliverer. This is the **internal** transport for PRD-005 org-level outbound webhooks, which are distinct from per-monitor notify channels (PRD-005 section 7, RFC-000 section 2.5 "Also delivers org-level outbound webhooks (PRD 9)"). It exists so the notifier delivers org webhooks the same way it delivers channel notifications (consume, deliver with retry, record outcome) instead of api making outbound HTTP on its request path.

```json
{
  "schema": "webhook.delivery",
  "version": 1,
  "event_id": "01J8ZA56Z1...",
  "occurred_at": "2026-06-21T14:01:31Z",
  "org_id": 42,
  "webhook_id": 30001,
  "outbound_event_id": "01J8ZA56Z1...",
  "event": "monitor.down",
  "created_at": "2026-06-21T14:01:31Z",
  "data": {
    "monitor": { "id": 5001, "name": "Prod API health", "url": "https://api.example.com/health", "method": "GET" },
    "incident": { "id": 70001, "started_at": "2026-06-21T14:00:00Z", "ended_at": null },
    "check": { "checked_at": "2026-06-21T14:01:30Z", "healthy": false, "failure_reason": "status_mismatch", "status_code": 503, "latency_ms": 120, "error": null }
  }
}
```

| Field | Type | Req | Notes |
|-------|------|-----|-------|
| `webhook_id` | int64 | yes | which registered org webhook to deliver to (PRD-005 section 7.4) |
| `outbound_event_id` | string | yes | the public `event_id` the receiver dedups on (PRD-005 section 7.2); equals the envelope `event_id` |
| `event` | enum | yes | `monitor.down`/`monitor.recovery`/`incident.opened`/`incident.closed` for v1 GA (PRD-005 section 7.1); phased member/monitor lifecycle events are additive (section 5) |
| `created_at` | string RFC3339 | yes | the public envelope timestamp |
| `data` | object | yes | the resource snapshot, same field shapes as appendix B; secrets never included (PRD-005 section 7.3) |

The notifier signs the outbound HTTP delivery with `X-Pulse-Signature: t=<ts>,v1=<hmac>` over timestamp + raw body using the per-webhook secret (PRD-005 section 7.2, RFC-000 section 7.3). That signing is a notifier/delivery concern, not part of the Kafka payload; the Kafka event carries `webhook_id` so the notifier loads the secret. The dedup key for the consumer is `outbound_event_id` + `webhook_id`.

---

## 5. Serialization and schema evolution

### 5.1 Decision: JSON with a documented, versioned schema; no schema registry in v1

Wire format is **JSON** (UTF-8), with the envelope `schema` + `version` fields, and the schemas documented in this RFC. We do **not** run a Confluent-style schema registry in v1, and we do **not** use Avro or protobuf in v1.

### 5.2 Reasoning

| Factor | JSON | protobuf | Avro + registry |
|--------|------|----------|-----------------|
| Debuggability | a result or notify event is human-readable in `kafka-console-consumer`, in a DLQ, in logs, and after mirroring. This matters most for an alerting/notify pipeline we will debug under incident pressure | binary; needs the schema to read | binary; needs the registry to read |
| Operational weight | none beyond the broker | codegen build step | a registry service to run, secure, back up, and make multi-region (it would have to be reachable from the mirror path too) |
| Throughput cost | JSON is larger and slower to parse than protobuf | smallest/fastest | small |
| Governance at scale | by convention + this doc + the version field | strong (compiled types) | strongest (registry enforces compatibility) |

At our volume the throughput cost of JSON is acceptable: the firehose is `check.results` at ~10k/sec, each row is a few hundred bytes, which is well within franz-go + a managed broker's comfort zone (section 9). The debuggability and zero-extra-infra of JSON win for v1. The version field gives us a clean upgrade path to a registry later without a data migration.

### 5.3 Rejected alternatives

| Option | Why rejected for v1 |
|--------|---------------------|
| Avro + Confluent Schema Registry | strongest governance, but it adds a stateful service to run and to make reachable across the regional/mirror boundary, and it makes every event unreadable without the registry. Over-built for five services and a fixed, small set of schemas. Revisit when the schema count or external consumers grow |
| protobuf | efficient and typed, but binary-on-the-wire hurts the debug-under-incident story for exactly the pipeline (results/notify) we most need to inspect, and it adds a codegen step. The size win is not needed at our volume |
| JSON with no version field | the cheapest, but it gives no controlled-evolution story; a breaking change would be uncontrolled. The version field is cheap insurance |

### 5.4 Compatibility rules (binding on every producer and consumer)

1. **Additive only within a major.** New fields are added as optional with a safe default and the JSON key always present (value `null` when absent). Producers may start emitting a new optional field without coordinating a consumer release.
2. **Consumers ignore unknown fields.** Every consumer unmarshals into a struct that tolerates extra keys (Go's `encoding/json` ignores unknown fields by default). A consumer must never fail on a field it does not recognize. This is what lets a producer roll ahead of its consumers.
3. **Never remove or repurpose a field within a version.** Removing a field, renaming it, or changing its type or meaning is a breaking change and requires a `version` bump.
4. **Version bump = dual-read window.** On a breaking change, bump `version`, and consumers learn to read both the old and new `version` for a deprecation window (covered by topic retention) before the producer stops emitting the old version. No big-bang cutover.
5. **`schema` is stable.** The `schema` string equals the topic base name and never changes; it is the routing key for "which schema is this".
6. **Enums grow forward-compatibly.** A consumer that meets an unknown enum value (e.g. a new `failure_reason` or `down_policy`) treats it conservatively rather than crashing: for `check.results.healthy=false` an unknown `failure_reason` is still a failure; an unknown `down_policy` falls back to `quorum`. New enum values are therefore additive, not a version bump, as long as the conservative fallback is safe.

A future schema registry would mechanically enforce rules 1-4; until then they are enforced in code review and by a CI check that diffs the documented schemas against the producer structs.

---

## 6. Delivery semantics and idempotency (the correctness core)

### 6.1 At-least-once everywhere

Per ADR-0009, delivery is at-least-once and we get exactly-once-in-effect through idempotent consumers, not Kafka transactions. The mechanism is uniform:

- Producers use the franz-go idempotent producer, so a produce retry does not double-append at the broker.
- Consumers commit offsets only after the handler returns nil (`internal/bus` commit-after-process), so a crash mid-handle redelivers.
- Therefore every consumer MUST be safe to run twice on the same event. The dedup token below is what makes that true for each consumer.

### 6.2 The idempotency token per topic

| Event | Dedup token (in body + `pulse-dedup-key` header) | Consumer dedup mechanism |
|-------|--------------------------------------------------|--------------------------|
| `check.jobs` (scheduled) | `job_id` = `<monitor_id>:<region>:<scheduled_at_unix>` | the control-plane result upsert is keyed by `(org_id, monitor_id, region, checked_at)` unique; a redelivered job runs the same check and re-emits the same `check.results`, which the consumer upserts as a no-op (section 6.3) |
| `check.jobs` (check-now) | `job_id` = `<monitor_id>:<region>:checknow:<now_unix>` (RFC-004 section 9.2) | same `(monitor_id, region, checked_at)` dedup as a scheduled job; the `checknow` segment and `now_unix` stamp keep a manual check from colliding with a scheduled tick's job id |
| `check.results` | `(org_id, monitor_id, region, checked_at)` | the control-plane consumer upserts the durable row on this key, then alerting applies the result against stored state with conditional writes (section 6.4) |
| `notify.events` | `hex(sha256(incident_id, event_type))` | notifier records delivered dedup ids in Redis with a Postgres backstop; a duplicate is suppressed (section 6.5) |
| `monitor.changed` | `event_id` | scheduler applies the snapshot idempotently (last-writer-wins per monitor within the org-ordered partition); re-applying the same snapshot is a no-op |
| `billing.events` | `stripe_event_id` (or `event_id` for admin) | invalidating an already-invalidated cache key is a no-op; api drops a Stripe webhook it already saw by `stripe_event_id` |
| `audit.events` | `event_id` | append is an upsert on the unique `event_id`; a duplicate append is a no-op |
| `region.health` | latest-wins by `region` (compaction) | consumers keep only the newest per region; an old or duplicate heartbeat is ignored by `occurred_at` |
| `webhook.delivery` | `outbound_event_id` + `webhook_id` | notifier records delivered (outbound_event_id, webhook_id) and the receiver also dedups on `event_id` (PRD-005 section 7.2) |

### 6.3 Worker emit and the result upsert (keyed by org_id, monitor_id, region, checked_at)

The worker emits only; the durable `check_results` upsert runs in a control-plane consumer (RFC-005 section 5.3, folded into the alerting transaction in RFC-006 section 5.4), keyed `(org_id, monitor_id, region, checked_at)`:

```
worker, on check.jobs message (job_id, monitor_id, region, scheduled_at, check):
  run the HTTP check (internal/checker), producing a CheckResult
  set checked_at on the result (the actual run time)
  emit check.results { job_id, scheduled_at, monitor_id, region, checked_at, ... }
  return nil   -> commit the job offset

control-plane consumer, on check.results message:
  INSERT INTO check_results (..., org_id, monitor_id, region, checked_at, ...)
    ON CONFLICT (org_id, monitor_id, region, checked_at) DO NOTHING
    RETURNING id            -- the result_id, assigned at persist time
  if conflict (row already existed): re-read the existing row id
```

Redelivery hazard: a redelivered job runs the check twice and re-emits `check.results`. The unique `(org_id, monitor_id, region, checked_at)` makes the consumer's second insert a no-op, so the durable row and the downstream stay safe. `checked_at` is the actual run time; two genuine runs of the same monitor in the same region are spaced by `interval_seconds >= timeout_seconds` (PRD-002 section 3.6), so two distinct ticks never collide on `checked_at`, while a redelivery of one tick reuses the same `checked_at` from the same stored row.

### 6.4 Alerting (per-monitor ordering + idempotent transitions)

Per-monitor ordering is guaranteed by `monitor_id` partitioning: one consumer handles a monitor's results in arrival order, so the reused pure `alerting.Apply` sees a coherent run. The distributed wrapper makes the transition idempotent exactly as RFC-000 section 8 binds:

```
on check.results message (result_id, monitor_id, region, checked_at, healthy, ...):
  BEGIN
    read AlertState (consecutive_fails, first_fail_at, open_incident) for monitor   -- in this txn
    -- multi-region reduce happens first: combine per-region results within the
    -- aggregation window using down_policy over healthy-reporting regions R
    -- (PRD-007 section 5.2); feed the single verdict to Apply.
    decision := alerting.Apply(monitor, reducedResult, state)   -- pure, no I/O, no clock
    if decision.Action == OpenIncident:
        INSERT incident (...) WHERE NOT EXISTS (open incident for monitor)
        -- the partial unique index uniq_open_incident WHERE ended_at IS NULL
        -- enforces one open incident; a redelivery's insert is a no-op
    if decision.Action == CloseIncident:
        UPDATE incident SET ended_at=..., close_reason='recovered'
          WHERE id = open_incident.id AND ended_at IS NULL   -- conditional
    -- counter update guarded by the triggering result id so reprocessing is a no-op
    UPDATE alert_state SET consecutive_fails=..., first_fail_at=...,
           last_applied_result_id = :result_id
      WHERE monitor_id = :m AND (last_applied_result_id IS NULL
            OR last_applied_result_id < :result_id)
  COMMIT
  if decision.Notify != nil:
      emit notify.events with dedup_key = sha256(incident_id, event_type)
  return nil   -> commit the result offset
```

How redelivery is safe:

| Hazard | Why it does not happen |
|--------|------------------------|
| double-open | the open insert is conditioned on no open incident (partial unique index `uniq_open_incident WHERE ended_at IS NULL`, carried from v1); the second attempt is a no-op |
| double-close | the close update is conditioned on the incident still being open (`ended_at IS NULL`); the second update touches zero rows |
| double-count | the counter update is guarded by `last_applied_result_id`; re-applying the same `result_id` (or an older one) updates zero rows |
| double-notify | the re-emitted notify event carries the same `dedup_key = sha256(incident_id, event_type)`, which the notifier suppresses (section 6.5) |
| out-of-order / duplicate result | per-monitor partitioning prevents cross-tick reorder within a partition; the guard on `last_applied_result_id` drops a stale replay; `Apply` is a pure function of (monitor, result, state), so applying a duplicate against the already-advanced state yields ActionNone |

One-down/one-up per incident therefore holds under redelivery (PRD-002 section 4.7), and the reused `internal/alerting.Apply` is untouched (RFC-000 section 14).

### 6.5 Notifier (dedup id = hash(incident_id, event_type))

```
on notify.events message (dedup_key, event_type, monitor, incident, check, channel_ids, ...):
  if Redis SET pulse:notify:dedup:<dedup_key> NX EX <window> == 0:   -- already present
      record "suppressed duplicate" metric; return nil   -- do not re-deliver
  for each channel in channel_ids (concurrently, independent):
      render appendix-B payload; deliver via internal/notify with retry/backoff
      record delivery outcome (incident timeline + audit/log)
  -- Postgres backstop: a row keyed by dedup_key marks the event delivered, so a
  -- Redis eviction inside the window still does not double-send
  return nil   -> commit the notify offset
```

The dedup window comfortably exceeds the at-least-once redelivery horizon and the notify topic retention (24h). One channel failing does not block the others (PRD-003 section 5); the dedup is per notify event (incident + event_type), so a duplicate `down` or duplicate `recovery` is suppressed while the legitimate one of each still fires (PRD-002 section 4.7).

### 6.6 Note: API request-level idempotency is separate

The public API's optional `Idempotency-Key` header (PRD-005 section 3.7, remembered 24h, conflicting body -> 409) is HTTP write dedup and is owned by RFC-012. It is a different key space from the event dedup tokens above. RFC-002 owns only the consumer-redelivery tokens.

---

## 7. Cross-region mirror (RFC-000 ADR-0006)

### 7.1 Shape

| Direction | Cluster | Topics | Transport |
|-----------|---------|--------|-----------|
| jobs out | scheduler -> regional cluster | `check.jobs.<region>` | produced directly into the regional cluster; workers consume locally |
| results home | regional cluster -> central cluster | `check.results`, `region.health` | mirrored by MirrorMaker 2 (or the managed equivalent) |

Workers produce `check.results` and `region.health` into their **regional** cluster (local, low-latency, survives a brief home-region partition). MirrorMaker 2 mirrors only those two topics from each regional cluster to the central control-plane cluster, where `alerting` and `scheduler` consume them. The scheduler writes a region's jobs straight into that region's cluster, so jobs are not mirrored.

### 7.2 Mirror mechanism decision

Use **MirrorMaker 2** (or the cloud provider's managed mirroring built on it). Reasoning: MM2 is the standard Kafka-native, partition-preserving mirror; it is a Connect-based, operationally well-understood tool; managed Kafka offerings expose it directly. We reject a custom consume-produce bridge (reinvents MM2's offset/partition handling and failure semantics) and reject full active-active replication (over-built for a single-region control plane, RFC-000 section 4.2).

### 7.3 Topic naming across clusters

| Posture | Naming |
|---------|--------|
| Decision: preserve the topic name across the mirror | `check.results` on the regional cluster mirrors to `check.results` on the central cluster (no source-cluster prefix) |
| Reasoning | the central `alerting`/`scheduler` consume one logical `check.results`/`region.health` regardless of which region produced it; the `region` field in the payload already carries the origin. A default MM2 setup prefixes mirrored topics with the source cluster alias (`<src>.check.results`); we configure MM2 to mirror into the unprefixed central topic (an explicit topic mapping) so consumers see one stream. The trade-off (losing the auto source-prefix) is acceptable because the `region` payload field is the authoritative origin and avoids N region-prefixed topics the consumer would have to subscribe to |

### 7.4 Ordering preservation through the mirror

MM2 preserves per-partition order within a topic, and it maps source partition to the same destination partition. Because `check.results` is keyed by `monitor_id`, a monitor's results stay on one partition end-to-end, so per-monitor order survives the mirror. Cross-region interleaving of one monitor cannot happen because a monitor's checks for a given region are produced in order in that region, and a monitor is normally checked from multiple regions whose results alerting reduces within an aggregation window (RFC-000 section 8, PRD-007 section 5.2); the down-policy reduce tolerates results arriving slightly apart, so a small mirror delay is fine within the 5s decision-latency SLO.

### 7.5 Egress and cost trade-off

Mirroring `check.results` and `region.health` home is cross-region egress, paid per region. It is bounded: one small row per check per region, and we mirror **only** these two topics, never the job stream. Premium regions are a paid entitlement (PRD-006, PRD-007 section 8) so we do not pay premium-region egress on free traffic. At Phase 0/1 there is one region (home) so there is no mirror and no egress (RFC-000 section 4.2). RFC-008 owns the cost model and failover; this RFC fixes only the mirror seam.

### 7.6 region.health flow

`region.health` is produced into the regional cluster by workers (heartbeats) and the region controller, mirrored home like `check.results`, and consumed by `alerting` (verdict reduction needs liveness, RFC-000 section 4.1) and `scheduler` (failover and dispatch decisions). The topic is compacted so a freshly-started central consumer reads the current liveness per region immediately after the mirror catches up.

---

## 8. Failure handling

### 8.1 Consumer lag monitoring (ties to RFC-010)

Lag is the primary scale and health signal. Each consumer group's per-partition lag is scraped (the broker exposes it; the worker/alerting/notifier HPAs already scale on it per RFC-000 section 11). RFC-010 owns the SLOs and alerts; the contract here:

| Group | Lag SLI | Action on sustained lag |
|-------|---------|-------------------------|
| `worker-<region>` | `check.jobs.<region>` lag | HPA adds workers; sustained lag = checks waiting to run |
| `alerting` | `check.results` lag | HPA adds consumers (up to partition count); sustained lag threatens the 5s result-to-decision SLO |
| `notifier` | `notify.events` + `webhook.delivery` lag | HPA adds consumers; sustained lag threatens the 30s notify SLO |

### 8.2 Poison message and dead-letter handling

A poison message is one that a handler can never process (malformed JSON, an invariant violation, a body that fails schema parse). Without handling, commit-after-process would loop the partition forever on it.

| Rule | Behavior |
|------|----------|
| Detection | the handler returns `bus.Poison(err)` when the message is structurally unprocessable (not a transient error) |
| DLQ | `internal/bus` publishes the raw record (bytes + original headers + a `pulse-dlq-reason` header) to `<topic>.dlq`, then commits the original offset so the partition advances |
| DLQ topics | one per source topic, e.g. `check.results.dlq`, `notify.events.dlq`; same partition count, `delete` cleanup, 14-day retention so we can inspect and replay |
| Alerting | any write to a DLQ raises an RFC-010 alert; a poisoned `check.results` or `notify.events` is a correctness event we must see |

### 8.3 Retry vs DLQ policy

| Failure kind | Policy |
|--------------|--------|
| Transient (broker hiccup, Postgres deadlock, Redis blip, downstream 5xx) | do NOT DLQ; return a normal error so `bus` does not commit, and the message redelivers. The handler relies on its idempotency token (section 6) to be safe on the retry |
| Permanent / structural (unparseable, schema-invalid, invariant-violating) | DLQ immediately via `bus.Poison`; retrying cannot help |
| Outbound delivery (notifier -> Slack/SMTP/webhook) | this is NOT a Kafka retry; the notifier retries the outbound call in-process with backoff (reused `internal/notify`, default backoff 1s/4s/9s capped at 30s), and on give-up records the failure visibly in the incident timeline + audit (PRD-003 section 5). The notify Kafka offset commits once the event is handled (delivered or recorded-as-failed), so a give-up does not loop the partition |

### 8.4 Consumer down: backlog and catch-up

If a consumer group is fully down, its topic retention is the catch-up budget: `check.results`/`notify.events` hold 24h, so `alerting`/`notifier` can be down for up to ~a day and still catch up without data loss (the durable history is in Postgres regardless). On recovery the group resumes from its last committed offset and drains the backlog; lag-based HPA scales it out for the catch-up. A region's `check.jobs` holds only 1h because a stale job is not worth running; a long worker outage means missed checks, which surface as missing results and coverage-degraded (PRD-007 section 6.3), never a false page.

### 8.5 Partition rebalancing safety

| Concern | Handling |
|---------|----------|
| Rebalance during processing | `internal/bus` uses cooperative-sticky group balancing (franz-go) so adding/removing a consumer reassigns only some partitions, not all, minimizing stop-the-world |
| At-least-once across rebalance | because offsets commit only after process, a partition reassigned mid-message is reprocessed from the last committed offset by the new owner; the idempotency token makes that safe |
| Per-monitor ordering across rebalance | a monitor's partition moves to exactly one new consumer; ordering within the partition is preserved, so the state machine still sees the monitor's run in order |
| Duplicate processing window | the brief overlap a rebalance can cause is covered by the same dedup tokens; no special handling beyond section 6 |

---

## 9. Capacity

Sized against PRD-012: ~10k checks/sec sustained, 500k monitors, multiplied by region fan-out.

### 9.1 Throughput model

| Quantity | Value | Source |
|----------|-------|--------|
| Sustained checks/sec (single-region baseline) | ~10,000 | PRD-012 |
| Region fan-out multiplier | up to the per-monitor region count (Free 1, Business up to 6) | PRD-006 section 3, PRD-007 |
| `check.jobs` produced/sec (all regions) | checks/sec x average regions per monitor | each (monitor, region) tick is one job |
| `check.results` into central/sec | same as jobs produced (one result per job, after mirror) | one result per executed check |
| `notify.events`/sec | tiny: only incident open/close, far below result rate | PRD-002 (no re-notify while down) |

The firehose is `check.results` at roughly the fan-out-multiplied check rate (order 10k-60k/sec depending on average region count). At a few hundred bytes per JSON result that is single-digit to low-tens MB/sec, well within a managed Kafka cluster and franz-go.

### 9.2 Partitions and consumer parallelism

| Topic | Partitions | Max useful consumers | Headroom rationale |
|-------|-----------|----------------------|--------------------|
| `check.jobs.<region>` | 64/region | 64 workers/region | per-region check rate divided across 64 partitions stays low per partition |
| `check.results` | 128 central | 128 alerting consumers | the firehose; 128 keeps per-partition rate modest and lets alerting scale wide on lag while preserving per-monitor order (one monitor = one partition) |
| `notify.events` | 32 | 32 notifiers | far more than notify volume needs; sized for burst on a wide outage opening many incidents at once |
| `webhook.delivery` | 16 | 16 | paid-tier fan-out |
| control topics (`monitor.changed`/`audit`/`billing`/`region.health`) | 8-16 | matches | low volume; partitions chosen for key spread and consumer count, not throughput |

### 9.3 How this scales

- Throughput scales by adding partitions and consumers; the only constraint is per-key (per-monitor / per-org) order, which is preserved because one key maps to one partition. Cross-key parallelism is bounded only by partition count, which has headroom and can grow (with a one-time reshuffle).
- Per-region clusters scale independently; adding a region adds a `check.jobs.<region>` topic and a worker group with no central change beyond a new mirror flow.
- alerting scales to the partition count (128) on `check.results`; if even that is tight, partition count grows (RFC-011) ahead of need.
- The central cluster only carries `check.results` + `region.health` from the mirror plus the control topics, so it is not also carrying the job stream, keeping central load bounded to results + control traffic.

---

## 10. Open questions and dependencies

### 10.1 Open questions

| # | Question | Owner |
|---|----------|-------|
| 1 | Secret header values on `check.jobs` (section 4.3): carry decrypted, or carry an encrypted blob + per-region unwrap key? This RFC fixes the field (`headers[].value`) and the bounded-exposure posture; the final encryption decision is the RFC-005 security pass | RFC-005 |
| 2 | `audit.events` per-stream retention: RFC-000 open question 2 notes login-event volume may want a separate retention stream from people-changes. If so, split into `audit.events` + `audit.auth` with different retention, both keyed by `org_id` | product + RFC-001 |
| 3 | Actor distinction in events: `actor.kind` enum is fixed here (`user`/`api_key`/`system`), but the full taxonomy (e.g. system sub-kinds) is RFC-000 open question 2 and needs a product call | product + RFC-003 |
| 4 | `region.health` heartbeat detection-latency SLO and the staleness bound for "effectively unhealthy" are not fixed by PRD-007; RFC-008 must set them | RFC-008 |
| 5 | Schema-registry trigger: when does the schema count or external-consumer pressure justify Avro + a registry? Record the "later, when X" trigger rather than carrying the weight now | this RFC / RFC-010 |
| 6 | DLQ replay tooling: how an operator inspects and replays a `*.dlq` topic (a small admin command in `internal/bus`) | RFC-011 |

### 10.2 Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-000 | the topic list, partition keys, idempotency decisions, mirror shape, client/serialization ADRs this RFC resolves |
| RFC-001 | the Postgres unique constraints this RFC's idempotency leans on: `(monitor_id, region, checked_at)` on `check_results`, `uniq_open_incident WHERE ended_at IS NULL`, the audit `event_id` unique, `last_applied_result_id` on alert state |
| PRD-002/003/006/007 | the field semantics behind every schema |

| Depends on this RFC | For |
|---------------------|-----|
| RFC-004 (scheduler) | `monitor.changed` consume, `check.jobs` produce, `job_id` stamping |
| RFC-005 (worker) | `check.jobs` consume, `check.results` produce, the secret-header decision |
| RFC-006 (alerting) | `check.results` consume, `notify.events` produce, the idempotent-transition contract |
| RFC-007 (notifier) | `notify.events` + `webhook.delivery` consume, the dedup-id contract |
| RFC-008 (multi-region) | the mirror seam, `region.health` schema, topic-per-region naming |
| RFC-009 (entitlements) | `billing.events` consume for cache invalidation |
| RFC-010 (observability) | the lag SLIs, DLQ alerts, trace-over-Kafka header contract |
| RFC-011 (infra) | cluster provisioning, partition counts, MM2 config, DLQ retention |

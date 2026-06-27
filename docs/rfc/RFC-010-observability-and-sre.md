# RFC-010 - Observability and SRE

Status: DRAFT for review
Author: Principal SRE / Observability
Audience: every service author (each service exposes the SLIs defined here), on-call, and RFC-011 (which deploys the stack)
Owns (per RFC-000 section 13): the SLO definitions and their measurement, the trace-propagation-over-Kafka standard, the dashboards and the alerts.
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 9 the three-pillar standards, section 12 SLOs, section 2 service catalog, section 11 deployment).
Sibling contracts: `docs/rfc/RFC-002-eventing-kafka-contracts.md` (trace context over Kafka headers section 2.4, consumer lag section 8.1, DLQ alerts section 8.2).
Product source: `docs/PRD.md` section 12 (committed SLOs and the 99.9% SLA), section 14 (self-measured SLA attainment, North Star), section 13 (security, SOC 2 path); `docs/prd/PRD-007` section 8 (per-region COGS and cost-aware scheduling).

House style: all timestamps RFC3339 UTC on the wire. No em-dashes. Tables and code over prose.

The irony is the point: Pulse is an uptime monitoring product, so it must monitor itself impeccably, and it must never trust the thing it monitors as the only witness that the thing is alive. Section 6 (meta-monitoring) is where that bites.

---

## 1. Overview, scope, and owned contracts

### 1.1 What this RFC fixes

RFC-000 section 9 set the three pillars as standards (Prometheus metrics, structured slog logs, OpenTelemetry traces) and listed per-service SLIs and the PRD-012 SLOs at a high level. This RFC turns those into exact, copy-pasteable contracts every service implements through `internal/obs` (RFC-000 section 3, 14):

1. The standard Prometheus metric set per service: concrete metric names, types, labels, and the cardinality rule.
2. The structured-logging contract: the standard fields, levels, redaction, backend, and retention.
3. The tracing contract: spans across the request path and across Kafka, the collector, the backend, sampling, and the must-trace paths.
4. The SLI/SLO/error-budget table: each PRD-012 SLO mapped to an exact metric query, target, window, and budget, plus the self-measured 99.9% pipeline SLA and how it is instrumented end to end.
5. Self-monitoring and meta-monitoring: dogfooding plus an independent external watchdog, with the "who watches the watchman" problem reasoned out.
6. Alerting on ourselves: the Alertmanager rule set, severity, routing, and runbook links.
7. The Grafana dashboard set, provisioned as code.
8. Capacity signals, headroom targets, the forecasting model, and cost observability.
9. The runbook set, on-call stance, incident review process, and change management toward SOC 2.

### 1.2 Owned contracts vs delegated

| This RFC owns | Delegated to |
|---------------|--------------|
| The SLO definitions and the exact SLI query that measures each | each service RFC emits the underlying metric |
| The trace-context-over-Kafka header standard (`traceparent`, `pulse-correlation-id`) | RFC-002 already fixed the header names in `internal/bus`; this RFC fixes the span shape and the must-trace path |
| The standard metric names, types, labels, and the cardinality rule | each service RFC instruments its own hot paths against this naming |
| The Alertmanager rule catalog and the dashboard catalog | RFC-011 deploys Prometheus, Alertmanager, Grafana, the OTel collector, and the log and trace backends |
| The capacity forecasting model and the cost-observability metrics | RFC-008 owns the per-region cost model that feeds the cost dashboards; RFC-011 sizes the clusters |
| The runbook set to author and the incident-review process | RFC-011 owns the deployment-side DR runbooks (Postgres failover mechanics, cluster recovery) |

### 1.3 Where the instrumentation lives

All three pillars are wired through one shared package, `internal/obs` (RFC-000 section 14: "Prometheus metrics, slog setup, OTel wiring"). A service author calls `obs.Init(serviceName)` once at boot, which:

- registers the standard metric set and starts the `/metrics` handler,
- configures the JSON slog handler with the standard fields,
- sets up the OTel tracer provider and the OTLP exporter to the collector,
- and installs the Kafka header propagation that `internal/bus` reads on produce and restores on consume (RFC-002 section 2.4).

Keeping it one package means the field names, metric names, label sets, and the cardinality rule are enforced in one place rather than per service. A service that wants a custom metric registers it through `obs` so the naming convention and the label allow-list are applied uniformly.

---

## 2. Metrics (Prometheus)

### 2.1 Client and endpoint

| Aspect | Decision |
|--------|----------|
| Client | `github.com/prometheus/client_golang` (the standard Go client). It is the de-facto client, pairs with the Prometheus server RFC-011 deploys, and gives the histogram and counter types we need with native exemplar support for trace linking (section 4.6) |
| Endpoint | every service binds `/metrics` on a dedicated metrics port (not the public api port), scraped by Prometheus via Kubernetes service discovery (pod annotations). The metrics port is not exposed through nginx |
| Scrape interval | 15s default. The SLO histograms are sized so a 15s scrape plus a recording rule gives stable p99 over the SLO windows in section 5 |
| Registry | one process-wide registry per service, plus the Go runtime collectors (`go_*`, `process_*`) for the USE method (section 2.7) |

### 2.2 Cardinality discipline (binding rule)

This is the load-bearing constraint for a multi-tenant product. With 50k orgs and 500k monitors, a single metric labeled by `org_id` or `monitor_id` would create up to 500k time series per metric per label combination, which would overwhelm Prometheus and make every query slow and every dashboard useless.

| Rule | Value |
|------|-------|
| Never label a metric by `monitor_id` | a per-monitor time series is 500k series per metric; that data belongs in logs and traces (which carry `monitor_id` as an attribute), not in metrics |
| Never label a metric by `org_id` at full cardinality | 50k series per metric. Per-org breakdown is a logs/traces query or an analytics query against Postgres, not a metric label |
| Safe to label by | `region` (low tens of values), `result` / `healthy` (a handful), `failure_reason` (the six PRD-002 values), `channel_type` (slack/discord/webhook/smtp), `route_class` (read/write), `method`, `status_code` family (2xx/4xx/5xx as `status`), `plan_tier_bucket` (free/starter/team/business, four values), `change` type, `event_type` (down/recovery) |
| Plan tier is a bucket, not the org | when a per-tier view is needed (for cost-per-check by tier, section 9), the label is the four-value `plan_tier_bucket`, never the org id. This keeps the series count bounded at 4 x other-labels |
| Status code is bucketed | label by `status` family (`2xx`/`3xx`/`4xx`/`5xx`) for rate and error math; the exact code (e.g. 503) lives in logs and on the trace span, not as a metric label, to avoid unbounded code values inflating series |

The decision rule for any new metric: "Will this label have more than a few hundred distinct values across the fleet?" If yes, it is not a metric label; it goes on a log line or a span attribute. RFC-002 already routes `org_id` and `monitor_id` as event data and as trace attributes, so the high-cardinality dimensions are queryable through logs and traces without paying the metric series cost.

### 2.3 RED and USE coverage

| Method | Applies to | Metrics |
|--------|-----------|---------|
| RED (Rate, Errors, Duration) | request-driven and event-driven work (api requests, each consumer's message handling) | a `_total` counter (rate), an error label or separate `_errors_total` (errors), and a `_duration_seconds` histogram (duration) for each work type |
| USE (Utilization, Saturation, Errors) | every resource (CPU, memory, goroutines, Kafka consumer lag, DB pool, Redis pool, partition assignment) | the Go runtime collectors plus the lag and pool gauges below; saturation is the lag gauge and the pool-in-use gauge |

Every service therefore has RED on its primary work and USE on its resources. The two methods together are what let on-call answer both "is the work flowing and fast" (RED) and "is a resource the bottleneck" (USE) without guessing.

### 2.4 Common metrics on every service

These are registered by `internal/obs` for all five services so the cross-service dashboards (section 8) are uniform.

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `pulse_build_info` | gauge (value 1) | `service`, `version`, `git_sha` | the running build; one series per deployed version, used to spot a stuck rollout |
| `pulse_up` | gauge | `service` | liveness from the service's own perspective (1 when serving); the external watchdog in section 6 does not trust this alone |
| `pulse_goroutines` | gauge | `service` | from the Go collector; saturation signal |
| `pulse_kafka_consumer_lag` | gauge | `service`, `group`, `topic`, `partition` | messages behind the high-water mark; the primary scale and health signal for worker/alerting/notifier (RFC-000 section 11, RFC-002 section 8.1) |
| `pulse_db_pool_in_use` / `pulse_db_pool_idle` | gauge | `service` | pgx pool saturation |
| `pulse_redis_pool_in_use` | gauge | `service` | Redis pool saturation |
| `pulse_dlq_messages_total` | counter | `service`, `topic` | a message routed to a DLQ; any increase pages (RFC-002 section 8.2) |

`service` is a static label set per process at boot. It is safe (five values).

### 2.5 Per-service SLIs

The naming convention: `pulse_<subsystem>_<thing>_<unit>`. Histograms end in `_seconds` and carry buckets sized for their SLO. Counters end in `_total`.

#### 2.5.1 api

| Metric | Type | Labels | Meaning / SLI use |
|--------|------|--------|-------------------|
| `pulse_http_request_duration_seconds` | histogram | `route`, `method`, `status`, `route_class` | the api latency SLI. `route_class` is `read` or `write` (PRD-012 splits the target); `route` is the templated path (`/api/v1/monitors/:id`), never the concrete id, so cardinality stays bounded by the route table |
| `pulse_http_requests_total` | counter | `route_class`, `method`, `status` | request rate and error rate (RED); error rate is the `status="5xx"` share |
| `pulse_entitlement_cache_hits_total` / `pulse_entitlement_cache_misses_total` | counter | (none beyond service) | entitlement-cache hit ratio (RFC-000 section 12 hot path) |
| `pulse_rate_limit_rejections_total` | counter | `plan_tier_bucket` | 429s by tier; feeds the rate-limit dashboard and the abuse signal |
| `pulse_inflight_requests` | gauge | (none) | in-flight request concurrency; the custom HPA signal (RFC-000 section 2.1) and a saturation signal |

Route cardinality note: `route` is the route-table template, a fixed and small set. It is the one place a path-like label is allowed precisely because it is templated, not per-resource.

#### 2.5.2 scheduler

| Metric | Type | Labels | Meaning / SLI use |
|--------|------|--------|-------------------|
| `pulse_schedule_dispatch_lag_seconds` | histogram | `region` | `dispatched_at - scheduled_at`; the scheduling-accuracy SLI (PRD-012: dispatch within 5s p99). Buckets centered around the 5s target (e.g. 0.1, 0.5, 1, 2, 5, 10) |
| `pulse_schedule_jobs_dispatched_total` | counter | `region` | jobs published per region per second; the input to the throughput and capacity model (section 9) |
| `pulse_schedule_size` | gauge | (none) | number of monitors in the in-memory heap; a saturation signal and a sanity check after a rebuild |
| `pulse_scheduler_is_leader` | gauge (0/1) | (none) | leader-election state; exactly one replica should report 1 (section 7 alerts on flapping or on zero/two leaders) |
| `pulse_scheduler_rebuild_duration_seconds` | histogram | (none) | time to rebuild the heap from Postgres after a leader change; bounds the schedule gap on failover (RFC-000 section 2.2) |

#### 2.5.3 worker

| Metric | Type | Labels | Meaning / SLI use |
|--------|------|--------|-------------------|
| `pulse_check_duration_seconds` | histogram | `region`, `result` | the HTTP check execution time; `result` is `healthy`/`unhealthy`/`blocked`. Used for worker performance and for the check-execution component of the end-to-end pipeline latency |
| `pulse_check_results_total` | counter | `region`, `healthy`, `failure_reason` | checks executed, split by outcome. `failure_reason` is the six PRD-002 values plus null-as-`none`; the firehose counter, the input to coverage and to per-region rate |
| `pulse_check_ssrf_blocks_total` | counter | `region` | SSRF resolution-time blocks (RFC-000 section 10, PRD-013); a security signal, a sustained spike is suspicious |
| `pulse_check_result_emit_failures_total` | counter | `region` | failures emitting `check.results` to Kafka after the Postgres write; a divergence risk signal |
| `pulse_worker_jobs_consumed_total` | counter | `region` | jobs consumed; with dispatched_total it shows the job-to-result flow per region |

Worker reuses the common `pulse_kafka_consumer_lag` (group `worker-<region>`, topic `check.jobs.<region>`) as its primary scale signal.

#### 2.5.4 alerting

| Metric | Type | Labels | Meaning / SLI use |
|--------|------|--------|-------------------|
| `pulse_verdict_latency_seconds` | histogram | (none) | `decided_at - result.checked_at`; the check-result-to-decision SLI (PRD-012: 5s p99). Measured from the result event's `checked_at`/`occurred_at` to when alerting commits the decision |
| `pulse_incidents_opened_total` / `pulse_incidents_closed_total` | counter | `region` (region that triggered), `close_reason` (on close: recovered/disabled/manual) | incident lifecycle; the business-relevant rate and a sanity check against notify volume |
| `pulse_alerting_redelivery_noops_total` | counter | (none) | results that re-applied to already-advanced state and changed nothing (RFC-002 section 6.4); a high rate signals redelivery churn |
| `pulse_coverage_degraded_total` | counter | `region` | verdicts that went coverage-degraded because too few healthy regions reported (RFC-000 section 4.1, PRD-007); separates "our region down" from "target down" |

Alerting reuses `pulse_kafka_consumer_lag` (group `alerting`, topic `check.results`) as its scale signal.

#### 2.5.5 notifier

| Metric | Type | Labels | Meaning / SLI use |
|--------|------|--------|-------------------|
| `pulse_notification_delivery_seconds` | histogram | `channel_type`, `event_type` | time from the notify event being handled to the outbound send returning. Combined with the upstream timestamps it backs the 30s end-to-end notification SLO (section 5). `channel_type` is slack/discord/webhook/smtp |
| `pulse_notifications_total` | counter | `channel_type`, `event_type`, `outcome` | deliveries by outcome (`delivered`/`failed`/`suppressed`); success rate and the dedup-suppression count |
| `pulse_notification_retries_total` | counter | `channel_type` | outbound retry attempts (the in-handler backoff, RFC-002 section 8.3); a rising retry rate is an early channel-degradation signal |
| `pulse_notify_dedup_suppressions_total` | counter | `event_type` | duplicate notify events suppressed by the dedup id (RFC-002 section 6.5) |

Notifier reuses `pulse_kafka_consumer_lag` (group `notifier`, topics `notify.events` and `webhook.delivery`).

### 2.6 Exemplars for trace linking

Histograms attach OpenTelemetry exemplars (a trace id sampled onto a histogram bucket observation) so a slow p99 bucket on a Grafana panel links straight to the trace of a slow request or a slow check. This is what turns "p99 verdict latency breached" into "here is the exact trace that was slow" without a separate correlation step. `client_golang` supports exemplars natively; `internal/obs` attaches the current span's trace id when recording the SLO histograms.

### 2.7 USE coverage detail

Utilization and saturation come from the Go runtime collectors (`go_goroutines`, `go_memstats_*`, `process_cpu_seconds_total`) plus the pool and lag gauges above. Kafka consumer lag is the saturation signal for the three lag-scaled services; the DB and Redis pool-in-use gauges are the saturation signal for api and the consumers that write Postgres. Errors at the resource level are the DLQ counter, the emit-failure counter, and the pool-exhaustion error logs.

---

## 3. Logging

### 3.1 Library and format

| Aspect | Decision |
|--------|----------|
| Library | `log/slog` with the JSON handler. It is already the codebase standard (RFC-000 section 9.2) and is in the standard library, so no new dependency |
| Format | one JSON object per line (structured), so the aggregation backend can index fields without regex parsing |
| Writer | stdout. The container runtime collects stdout; the agent ships it to the backend. Services do not write log files |

### 3.2 Standard fields on every line

`internal/obs` configures the handler so these are present without the caller restating them:

| Field | Source | Notes |
|-------|--------|-------|
| `service` | set at boot | one of the five service names |
| `level` | slog level | debug/info/warn/error |
| `ts` | slog timestamp | RFC3339 UTC |
| `msg` | the call | short, stable message; variable detail goes in fields, not interpolated into `msg`, so messages group |
| `trace_id` | OTel span context in `ctx` | the same id propagated over Kafka headers (section 4); the join key across services |
| `span_id` | OTel span context in `ctx` | links the line to a specific span |
| `org_id` | request/event context, where safe | present on org-scoped work; it is an int, not PII |
| `monitor_id` | event context, where safe | present on check/alert/notify work; this is where per-monitor debugging lives since it is banned as a metric label (section 2.2) |
| `region` | worker/job context | present on data-plane work |
| `event` | the call | a short dotted event name (`check.executed`, `incident.opened`, `notify.delivered`) so log queries group by event without parsing `msg` |

`trace_id` plus `monitor_id` is the pairing that makes one check's whole journey searchable: the metrics told you p99 broke, the trace shows the span tree, and the logs around that `trace_id` show the detail.

### 3.3 Level discipline

| Level | Use |
|-------|-----|
| error | something failed that needs attention and is not normal flow: a Postgres write failed, a DLQ route, a leader-election loss, a panic recovered. Errors should be rare and each should be actionable |
| warn | a recoverable degradation or a fail-safe path taken: rate-limit fail-open default applied, a Redis blip fell back to Postgres, an outbound channel retry. Notable but not paging on its own |
| info | normal lifecycle and significant business events at low volume: service start, leader acquired, incident opened/closed. NOT every check (that is the firehose and belongs in metrics and traces, not logs) |
| debug | off in production by default; per-request and per-check detail for debugging, switchable per service via config without a redeploy |

The hard rule: do not log per-check at info. At 10k+ checks/sec a per-check info line is a log firehose that buries the signal and costs real money. Per-check observability is metrics (counts and histograms) plus sampled traces; a check only earns a log line when it fails in a way that is not already a metric (for example an SSRF block carries a warn with the resolved IP).

### 3.4 No secrets, no PII (ties to security)

This binds to RFC-000 section 10 and PRD-013 data classification:

- Secret-class values (channel URLs, SMTP password, monitor headers flagged `secret`, API key material, webhook signing secrets) are never logged. The redaction discipline carried from v1 holds: secret header values are already null on `monitor.changed` (RFC-002 section 4.2), and the one place decrypted secrets ride the bus (`check.jobs`, RFC-002 section 4.3) is explicitly a no-log path.
- PII (user emails, names, invitation emails) is not logged in operational logs. `org_id` and `monitor_id` are integer ids, not PII, and are allowed. A user email belongs only in the audit trail (Postgres, access-controlled), never in an operational log line.
- `error_text` on a check result is already truncated transport detail, never a full body (RFC-002 section 4.4), so logging it is safe.

A CI check and code review enforce this; a leaked secret in a log is a security incident, not a bug.

### 3.5 Backend (recommendation with reasoning)

Recommendation: **Grafana Loki** as the log aggregation backend.

| Factor | Loki | Elasticsearch / OpenSearch | Managed log store (Datadog/CloudWatch) |
|--------|------|----------------------------|----------------------------------------|
| Fit with the stack | native Grafana integration; logs sit next to metrics and traces in one pane, and a `trace_id` jumps from a metric exemplar to the trace to the logs in one tool | separate UI, separate operational weight | good UI but a separate vendor and per-GB cost that scales with our log volume |
| Cost model | indexes labels only, stores log bodies cheaply in object storage; the right shape when most lines are structured and queried by `service`/`level`/`trace_id` | indexes everything, expensive at our volume | per-GB ingest pricing is the most expensive at firehose volume |
| Cardinality discipline | Loki rewards low-cardinality labels, which matches our metric rule: label streams by `service`/`level`/`region`, keep `org_id`/`monitor_id` in the line body as searchable fields, not as stream labels | full-text index tempts high-cardinality indexing | n/a |
| Operational weight | one more Grafana-family component RFC-011 already runs the rest of | a cluster to run and tune, or a second managed vendor | none to run, but vendor lock and cost |

Loki wins because it puts logs in the same Grafana pane as metrics and traces (the trace-id join is one click), its label-only indexing matches the cardinality discipline we already enforce on metrics, and its object-storage body retention is the cheapest path at firehose volume. The one caveat: Loki is weaker at full-text ad-hoc search than Elasticsearch; we accept that because our lines are structured and we query by field, and because the trace backend (section 4) carries the deep per-request detail.

Loki stream labels are kept low-cardinality on purpose: `service`, `level`, `region`. `org_id`, `monitor_id`, and `trace_id` are fields inside the JSON line, searchable but not stream labels, mirroring the metric cardinality rule.

### 3.6 Retention

| Stream | Retention | Reason |
|--------|-----------|--------|
| Operational logs (all services) | 30 days hot in Loki | enough to debug a recurring issue and to look back over a billing cycle; older than that is rarely useful operationally |
| error-level lines | 90 days | longer for incident review and trend analysis |
| The durable audit trail | not a Loki concern | audit lives in Postgres (RFC-000 section 10, RFC-002 section 4.8) with per-tier retention; operational logs are not the system of record for audit |

The audit trail and operational logs are deliberately separate: audit is a product/compliance artifact in Postgres, operational logs are an SRE artifact in Loki. Mixing them would put PII-adjacent audit data into the ops log store and break the retention and access model.

---

## 4. Tracing (OpenTelemetry)

### 4.1 The one-trace-per-check goal

A single check must be one trace from end to end, across services and across regions:

```
api (monitor edit)              [span: monitor.changed produced]
  -> scheduler (dispatch)       [span: schedule.dispatch -> check.jobs produced]
     -> (cross-region transport via regional Kafka + mirror)
        -> worker (execute)     [span: check.execute -> check.results produced]
           -> alerting (decide) [span: verdict.apply -> notify.events produced]
              -> notifier (send)[span: notify.deliver -> outbound HTTP/SMTP]
```

The join is the W3C trace context propagated over Kafka headers. RFC-002 section 2.4 already fixed that `internal/bus` injects `traceparent` and `pulse-correlation-id` into record headers on produce and restores them into the handler `ctx` and the slog logger on consume. This RFC fixes the span shape on top of that: each service starts a child span from the restored context, names it per the path above, and records `monitor_id`, `region`, `result_id`, `incident_id`, `org_id` as span attributes (the high-cardinality dimensions banned from metrics live here).

So a check that ends in a slow notification is one trace whose span tree shows exactly which hop spent the time (worker execution? mirror delay? alerting verdict? notifier outbound?), which is precisely the question the 30s notification SLO forces us to answer.

### 4.2 Library

OpenTelemetry Go SDK (`go.opentelemetry.io/otel`) with the OTLP exporter, wired in `internal/obs`. HTTP server and client spans use the OTel `net/http` instrumentation; Kafka spans are started manually around produce and consume in `internal/bus` because the trace context travels in record headers (the standard messaging-span convention), not in an HTTP header.

### 4.3 Collector

An OpenTelemetry Collector runs in the control plane (`pulse-system`, RFC-000 section 11.1). Every service exports OTLP to the collector, not directly to the backend. Reasoning:

- the collector buffers and batches, so a backend blip does not back-pressure the services on the live path,
- it applies tail sampling centrally (section 4.5), which cannot be done per service because the tail-sampling decision needs the whole trace,
- it is the single place to change the backend or add a second one without touching every service.

Workers in a data-plane region export to a collector agent in that region, which forwards to the central collector. This keeps the data-plane export local and survives a brief home-region partition, the same reasoning as the regional Kafka cluster (RFC-000 section 4.2).

### 4.4 Backend (recommendation with reasoning)

Recommendation: **Grafana Tempo**.

| Factor | Tempo | Jaeger | Managed (Datadog/Honeycomb/AWS X-Ray) |
|--------|-------|--------|----------------------------------------|
| Fit with the stack | same Grafana pane as metrics (exemplars) and logs (Loki trace-id link); one tool for all three pillars | good standalone trace UI, but a separate pane from metrics/logs | strong UI, but a separate vendor and per-span cost |
| Cost model | object-storage backed, indexes by trace id, cheap at high span volume; pairs with exemplars so we do not have to keep every trace to find the slow one | needs its own storage (Cassandra/Elasticsearch) to scale, more to run | per-span ingest pricing is expensive at our trace volume |
| Operational weight | one more Grafana-family component | a backing store to run and scale | none to run, but vendor cost and lock-in |
| Exemplar and correlation story | first-class: metric exemplar -> Tempo trace -> Loki logs is the designed-for path | possible but not the integrated path | vendor-specific |

Tempo wins for the same reason as Loki: it completes the single-pane Grafana story so the metric-to-trace-to-log hop is one click, its object-storage model is the cheapest at our span volume, and exemplars plus tail sampling mean we keep the traces that matter (errors and slow ones) without storing all of them. If a future enterprise need wants richer query, the OTLP-to-collector seam lets us add or swap a backend without touching services.

### 4.5 Sampling strategy

| Decision | Value |
|----------|-------|
| Approach | tail-based sampling at the collector, not head-based at the service |
| Why tail over head | head sampling decides at the first span before we know whether the trace is interesting; it would drop the slow or failing traces we most need. Tail sampling sees the whole trace, so we can keep 100% of error traces and 100% of SLO-breaching slow traces and sample the boring fast-and-healthy ones |
| Policy | keep all traces that contain an error span or a DLQ route; keep all traces whose end-to-end latency breaches the relevant SLO (a slow check-to-notify); sample the healthy-and-fast remainder at a low rate (start at 1%, tune by volume and cost) |
| Always-on path | the synthetic canary monitor (section 5.6) is sampled at 100% so the self-SLA trace is always present |

Head sampling at a low fixed rate is rejected because at our volume it would statistically miss most rare failures, which is the opposite of what an observability stack for a monitoring product should do. The cost trade-off (tail sampling buffers traces in the collector until the trace completes) is acceptable at our trace sizes (a handful of spans per check) and is bounded by the collector's buffer.

### 4.6 Must-trace paths

Even with sampling, these paths are always instrumented (a span is always created; tail sampling decides retention):

| Path | Why it must be traced |
|------|----------------------|
| The full check pipeline (scheduler -> worker -> alerting -> notifier) | the core product loop and the source of three of the five SLOs; this is the one-trace-per-check goal |
| api write requests | the 500ms write SLO and the place a slow or failing write is debugged |
| api read requests (status, history) | the 300ms read SLO; sampled more aggressively since read volume is high |
| OAuth callback and JWKS | auth is the front door; a slow or failing login is high-impact and rare enough to keep |
| Stripe webhook handling | billing correctness; rare, high-value, always kept |
| The synthetic canary check (section 5.6) | the self-SLA witness; 100% kept |

---

## 5. SLIs, SLOs, and error budgets

### 5.1 The contract

Each PRD-012 committed target becomes an SLO with a precise SLI (the exact metric and the query), a target, a measurement window, and an error budget. The window is monthly to match the 99.9% SLA accounting period (PRD-012). Percentile SLOs are evaluated continuously; the availability SLOs accrue over the month.

### 5.2 The SLO / SLI table

| SLO | SLI definition (exact metric / query) | Target | Window | Error budget |
|-----|---------------------------------------|--------|--------|--------------|
| Scheduling accuracy | p99 of `pulse_schedule_dispatch_lag_seconds` (= `dispatched_at - scheduled_at`), computed via `histogram_quantile(0.99, ...)` over the window | <= 5s p99 | rolling 30d | 1% of dispatches may exceed 5s (the p99 allowance); a sustained breach burns the budget |
| Check-result to decision | p99 of `pulse_verdict_latency_seconds` (= alerting `decided_at - result.checked_at`) | <= 5s p99 | rolling 30d | 1% of results may exceed 5s to a decision |
| Notification delivery (end to end) | p99 of the end-to-end span duration from `check.checked_at` to notifier outbound send, derived from the trace and recorded as `pulse_pipeline_notify_latency_seconds` (a histogram alerting stamps from the triggering result and notifier closes; excludes the third-party channel's own latency per PRD-012) | <= 30s p99 | rolling 30d | 1% of notifications may exceed 30s of our controllable budget |
| API read latency | p99 of `pulse_http_request_duration_seconds{route_class="read"}` | <= 300ms p99 | rolling 30d | 1% of reads may exceed 300ms |
| API write latency | p99 of `pulse_http_request_duration_seconds{route_class="write"}` (excludes "check now", which does network I/O, per PRD-012) | <= 500ms p99 | rolling 30d | 1% of writes may exceed 500ms |
| Control-plane availability | success ratio of api/dashboard/status-page requests = `1 - (rate(pulse_http_requests_total{status="5xx"}) / rate(pulse_http_requests_total))`, cross-checked by the external watchdog probe success (section 6) | 99.9% monthly | calendar month | 0.1% = ~43 min/month of control-plane unavailability |
| Pipeline availability (self-SLA) | the synthetic canary success ratio: a due canary check ran and, on its forced state change, the canary notification arrived end to end within the notification SLO (section 5.6) | 99.9% monthly | calendar month | 0.1% = ~43 min/month of pipeline outage |

The two availability SLOs are the customer-facing 99.9% SLA (PRD-012 section 12, "control plane" and "the check + alert pipeline"). The five latency SLOs are the responsiveness commitments. The percentile SLIs use the same histograms the services already emit (section 2.5), so there is no separate measurement pipeline.

### 5.3 Recording rules

Each SLI has a Prometheus recording rule that precomputes the p99 (or the success ratio) over the window so dashboards and burn-rate alerts read a cheap recorded series instead of recomputing the quantile on every query. The recording rules live in the RFC-011-deployed Prometheus config; this RFC owns their definitions (the queries in the table above).

### 5.4 The notification end-to-end SLI in detail

This is the only SLI that spans services, so it needs care. The 30s budget is measured from the triggering check's `checked_at` to the moment the notifier's outbound send returns, excluding the third-party channel's own latency (PRD-012). It is constructed as:

```
pipeline_notify_latency = notifier.send_returned_at - triggering_check.checked_at
```

The triggering check's `checked_at` rides through the pipeline: it is on `check.results` (RFC-002 section 4.4), alerting carries it onto `notify.events` (the `check.checked_at` field, RFC-002 section 4.5), and the notifier computes the delta when the send returns and records it as `pulse_pipeline_notify_latency_seconds`. The third-party latency is excluded by stamping at the moment our outbound call is issued, not when the remote acknowledges, matching the PRD definition. The trace (section 4.1) is the per-event witness; the histogram is the SLO aggregate.

### 5.5 Error-budget policy (what happens when budget burns)

| Budget state | Policy |
|--------------|--------|
| Healthy (budget > 50% remaining) | normal: ship features, normal change cadence |
| Burning fast (a multi-window burn-rate alert fires, section 7) | page on-call; the burn-rate alert (e.g. 2% of the monthly budget in 1 hour) is the signal that something is actively wrong, not just that the month has been mediocre |
| Budget < 25% remaining | a soft control on risky change: non-urgent deploys to the affected service slow down, the team's focus shifts to reliability work until the budget recovers; documented in the change-management process (section 10) |
| Budget run out (SLO missed for the month) | an incident review (section 10) is mandatory; a reliability-focus period follows where feature work for that service yields to fixing the cause, until the budget is back in the healthy band |

The burn-rate alerting uses the multi-window multi-burn-rate pattern (a fast window to catch acute outages, a slow window to catch slow erosion), so a brief blip does not page but a sustained or severe burn does. The policy is a control on change velocity, not a punishment: the budget is what tells the team when to trade features for reliability.

### 5.6 Self-SLA instrumentation: the synthetic canary

The pipeline-availability SLO (99.9%, "a due check runs and on a state change a notification is sent", PRD-012/PRD-014) cannot be measured from real customer monitors alone, because a customer's endpoint going down or staying up is the customer's behavior, not ours. We need a witness we control end to end.

Decision: a **synthetic internal canary monitor**.

| Aspect | Decision |
|--------|----------|
| What | a Pulse-owned monitor against a Pulse-owned controllable target endpoint. The target is a tiny internal service whose health we flip on a schedule (healthy -> unhealthy -> healthy) so the canary monitor is forced through a real down and a real recovery on a known cadence |
| What it proves | the whole loop ran: the scheduler dispatched the canary check on time, a worker executed it, alerting opened (then closed) a canary incident, and the notifier delivered the canary down and recovery to a canary channel (a dedicated sink we own), all within the latency SLOs |
| How it is measured | the canary check is traced at 100% (section 4.6) so each cycle is one trace, and the canary delivery records `pulse_canary_cycle_success` (1 when the forced state change produced the expected notification within budget, 0 otherwise) plus the end-to-end latency on the standard histograms. The pipeline-availability SLI is the success ratio of these cycles over the month |
| Per region | one canary per operated region so a single region's pipeline being broken is visible, not hidden behind the others |
| Why a forced state change | availability is "a due check runs AND on a state change a notification is sent"; a canary that only ever stays healthy never exercises the alerting and notifier legs, so it would not witness the whole promised loop. Flipping the target on a cadence forces both legs every cycle |

The canary is the internal witness. But a canary that runs inside Pulse cannot tell us Pulse is entirely down, which is exactly the meta-monitoring problem in section 6.

---

## 6. Self-monitoring (dogfooding and meta-monitoring)

### 6.1 The who-watches-the-watchman problem, stated plainly

Pulse monitors itself (dogfooding): the synthetic canary (section 5.6) and the internal Prometheus/Alertmanager stack watch the pipeline and the control plane. This is good and we lean on it. But it has a fatal blind spot if relied on alone: if Pulse is down, the part of Pulse that would notice and page is plausibly down too. A check pipeline that pages when checks fail cannot page when the pipeline itself cannot run. Prometheus and Alertmanager themselves can be down, the cluster can be down, the home region can be partitioned, or DNS for the whole control plane can fail. In every one of those cases the internal watchers are inside the failure and cannot raise a hand.

So the stance is: dogfood internally for depth and speed, but never let the thing being monitored be the only witness that it is alive. There must be at least one watcher that is independent of Pulse's own infrastructure.

### 6.2 The two layers

| Layer | Watches | Runs where | Trusts Pulse? |
|-------|---------|-----------|---------------|
| Internal (dogfooding) | the pipeline end to end (canary), per-service SLIs, infra (Postgres/Redis/Kafka), consumer lag, error budgets | inside the Pulse control plane (Prometheus, Alertmanager, the canary) | yes; deep and fast, but blind when Pulse itself is down |
| External (meta-monitoring / watchdog) | "is the Pulse control plane reachable and serving at all" and "is the internal alerting stack itself alive" | a third party, fully independent of Pulse's cluster, region, and code | no; deliberately ignorant of Pulse internals so it survives a total Pulse outage |

### 6.3 The external watchdog (decision)

Decision: a **third-party, independent uptime check** of the Pulse control plane, plus a **dead-man's-switch heartbeat** from the internal alerting stack.

| Component | What it does | Why independent |
|-----------|--------------|-----------------|
| External uptime probe | a third-party monitoring service (a competitor or a simple managed uptime checker) hits a Pulse health endpoint (`/healthz` on api, and the status-page surface) from outside our infrastructure on a tight interval and pages a separate on-call path if it fails | it does not run on our cluster, our region, our Kafka, or our code, so when all of that is down it still works. We are a monitoring product, so using an external monitor for our own front door is honest, not embarrassing |
| Dead-man's switch | the internal Alertmanager sends a periodic "I am alive and evaluating rules" heartbeat to an external dead-man's-switch service; if that service stops hearing the heartbeat, it pages | this catches the case where Pulse is up but the alerting stack that should page us is itself dead, which the internal stack by definition cannot self-report |

The two together cover the two failure shapes: the uptime probe catches "the product is down," the dead-man's switch catches "the watcher is down." Neither depends on the internal Prometheus, the internal Alertmanager, the cluster, or the home region being healthy.

Reasoning for using a third party rather than a second self-hosted Prometheus in another region: a second self-hosted stack is still our code, our deploy pipeline, our infrastructure, and a shared cause (a bad release, a credential expiry, an account-level cloud issue) can take down both. The point of meta-monitoring is to remove the shared cause, which only a genuinely independent party achieves. The external probe is cheap (one endpoint, tight interval) and its only job is the binary "is Pulse answering," so it does not need our depth.

### 6.4 Internal canaries (the depth layer)

The synthetic canary monitors (section 5.6), one per region, are the internal depth layer. They prove the whole loop works, not just that the front door answers. They feed the pipeline-availability SLO and they page on-call (through the internal Alertmanager) when a cycle fails. They are the fast, detailed witness; the external watchdog is the last-resort witness. Both exist on purpose.

### 6.5 What pages through which path

| Failure | Caught by | Pages via |
|---------|-----------|-----------|
| A service is slow or erroring, budget burning | internal Prometheus burn-rate alert | internal Alertmanager -> on-call |
| The pipeline broke (canary cycle failed) | internal canary | internal Alertmanager -> on-call |
| The whole control plane is unreachable | external uptime probe | the third-party's own paging path (independent of ours) |
| The internal alerting stack is dead | dead-man's switch | the external dead-man's-switch service's paging path |

---

## 7. Alerting on ourselves (Alertmanager)

### 7.1 Stack and routing

Prometheus evaluates the alert rules; **Alertmanager** (RFC-011 deploys it) handles grouping, deduplication, silencing, and routing. Routing is by severity and by service to the on-call paging tool (PagerDuty or equivalent, RFC-011 picks the vendor). Every alert carries a `runbook` annotation linking to the runbook (section 10) and a `dashboard` annotation linking to the relevant Grafana board, so the page is actionable, not just loud.

| Severity | Meaning | Routing |
|----------|---------|---------|
| page (critical) | customer-impacting or imminently so; wake someone | the paging tool, immediate |
| ticket (warning) | needs attention this business day, not now | a ticket queue / Slack channel, no page |
| info | awareness only | a dashboard annotation or a low-priority channel; no notification |

### 7.2 The alert catalog

| Alert | Condition | Severity | Runbook |
|-------|-----------|----------|---------|
| SLO burn (fast) | multi-window burn-rate: e.g. 2% of the monthly budget for an SLO is spent in 1 hour (and a 5-min confirmation window) | page | error-budget-burn |
| SLO burn (slow) | e.g. 5% of the monthly budget spent in 6 hours | ticket | error-budget-burn |
| Pipeline canary failing | `pulse_canary_cycle_success` 0 for N consecutive cycles in any region | page | pipeline-down |
| Consumer lag rising | `pulse_kafka_consumer_lag` for `alerting` or `notifier` sustained above a threshold that threatens the 5s/30s SLO, after the HPA has had time to react | page | kafka-lag |
| Worker lag rising | `worker-<region>` lag sustained high (checks waiting to run) | ticket (page if it threatens scheduling SLO) | kafka-lag |
| api error rate | 5xx ratio over a short window above threshold | page | api-errors |
| DLQ write | any increase in `pulse_dlq_messages_total` | page | dlq-triage |
| Postgres health | primary unreachable, replica lag high, connections near max, disk near full | page | postgres-failover |
| Redis health | unreachable or memory near max (entitlement fail-closed / rate-limit fail-open paths at risk) | page | redis-degraded |
| Kafka health | broker down, under-replicated partitions, controller issues (from the broker exporter / managed metrics) | page | kafka-health |
| Region down | `region.health` for an operated region stale or `unhealthy`, or `healthy_workers` at zero | ticket (it is a handled mode, coverage-degraded, not a customer incident per RFC-000 section 4.1; page only if it drops pipeline-availability) | region-down |
| Leader-election flapping | `pulse_scheduler_is_leader` summed across replicas is not exactly 1 for more than a short window (zero leaders = no dispatch; two = split brain), or it changes more than X times in Y minutes | page | scheduler-leader |
| Certificate expiry | a TLS cert (nginx, status-page wildcard, custom domains) within N days of expiry | ticket (page if within 48h) | cert-renewal |
| SSRF block spike | `pulse_check_ssrf_blocks_total` rate well above baseline | ticket (security) | ssrf-spike |
| Stuck rollout | `pulse_build_info` shows two versions of a service for longer than a deploy should take | ticket | rollout-stuck |

### 7.3 Why region-down is not a default page

RFC-000 section 4.1 and PRD-007 are explicit: a region going down is a handled failure mode (coverage-degraded), never a customer incident and never a false page. So the region-down alert is a ticket by default. It escalates to a page only if losing the region actually drops the pipeline-availability SLO (for example the last region for a set of monitors, or a region whose loss breaks the canary). This keeps us honest with the product promise that our own region failure does not page the customer, and it stops region blips from being noise for our own on-call.

### 7.4 Alert hygiene

Every alert is either actionable (has a runbook and a clear action) or it is deleted. A page with no runbook and no action is alert fatigue, which kills trust in paging exactly the way noisy customer alerts kill trust in the product (PRD-014 signal-to-noise). Burn-rate alerting (multi-window) is chosen specifically to avoid paging on brief blips while still catching real erosion.

---

## 8. Dashboards (Grafana, provisioned as code)

### 8.1 Provisioning

All dashboards are provisioned as code (JSON or jsonnet/grafonnet) checked into the repo and applied by RFC-011's Grafana deployment, not hand-built in the UI. A dashboard changed in the UI is lost on the next deploy; the repo is the source of truth. This is the same discipline as the alert rules.

### 8.2 The dashboard set

| Dashboard | Audience | Panels |
|-----------|----------|--------|
| Per-service (one each for api, scheduler, worker, alerting, notifier) | on-call, service owner | RED for the service's primary work, USE (CPU, mem, goroutines, lag, pools), the service's SLIs from section 2.5, build/version, recent error-log rate (Loki) |
| Pipeline end-to-end | on-call, SRE | the one-trace-per-check view: dispatch lag -> worker execution -> mirror -> verdict latency -> notify latency, each stage's p50/p99 side by side, plus the canary cycle status per region. The single board that answers "where in the loop is the time going" |
| Per-region health | SRE, support | `region.health` status, `healthy_workers`, per-region check rate, per-region check duration, coverage-degraded count, worker lag per region |
| SLO and error budget | everyone (and leadership) | each SLO's current p99 / success ratio vs target, budget remaining for the month, burn-rate over multiple windows. The board the error-budget policy (section 5.5) is read from |
| Business KPIs (PRD-014) | product, leadership | active monitors firing healthily across paying orgs (the North Star), monitors by plan tier bucket, incidents opened/closed rate, notifications delivered, self-measured pipeline-SLA attainment vs 99.9% |
| Capacity and cost | SRE, finance | the capacity signals and cost-observability panels from section 9 |
| Infra | on-call | Postgres (connections, replica lag, disk, slow queries), Redis (memory, hit ratio, evictions), Kafka (broker health, under-replicated partitions, per-topic throughput, consumer-group lag) |

The business KPI board pulls the North Star ("active monitors healthily firing across paying orgs", PRD-014) by joining the per-tier monitor counts and the healthy-check rate; the per-org breakdown it needs comes from a Postgres analytics query, not from a high-cardinality metric, consistent with the cardinality rule (section 2.2).

---

## 9. Capacity and cost

### 9.1 Capacity signals and headroom targets

| Resource | Signal | Headroom target |
|----------|--------|-----------------|
| Worker fleet (per region) | `pulse_kafka_consumer_lag` for `check.jobs.<region>`, check rate vs worker count | lag near zero in steady state; keep capacity for the 2x burst PRD-012 commits to |
| alerting | `check.results` lag, `pulse_verdict_latency_seconds` p99 vs the 5s SLO | lag near zero; p99 well under 5s so a burst has room before the SLO breaks |
| notifier | `notify.events` + `webhook.delivery` lag, delivery latency vs 30s | lag near zero; outbound retry rate flat |
| Kafka | per-topic throughput vs partition count, under-replicated partitions | partitions sized with headroom (RFC-002 section 9); never run a topic near its max-useful-consumer count |
| Postgres | write throughput (the `check_results` firehose), connection pool saturation, replica lag, disk vs partition-drop cadence | pool not saturated; replica lag bounded so read-path SLOs hold; disk headroom ahead of the next partition drop |
| Redis | memory vs max, hit ratio | memory headroom so eviction does not start dropping entitlement/rate-limit/dedup keys |

The headroom principle: every resource runs with enough slack that the committed 2x burst (PRD-012) does not breach an SLO before the HPA or a capacity add catches up. Saturation signals (lag, pool-in-use, memory) are the leading indicators; we add capacity off the leading signal, not after the SLO already broke.

### 9.2 The forecasting model

Capacity is forecast from the product's own growth dimensions, because check rate is a deterministic function of them:

```
check_rate (region-jobs/sec)
  = sum over monitors of (regions_selected / interval_seconds)

worker_count_needed (per region)
  = region check_rate x avg_check_duration / per-worker_concurrency, plus burst headroom

kafka:    check.results throughput  = check_rate x avg_result_bytes  -> partition + broker sizing
postgres: check_results inserts/sec = check_rate                     -> write capacity + partition cadence
```

So the forecast input is monitors x regions-per-monitor x frequency, which the business already tracks (active monitors is the North Star, PRD-014; region counts and interval floors are entitlements, PRD-006). As monitors grow or as plan mix shifts toward more regions and tighter intervals, the model projects the check rate and from it the worker, Kafka, and Postgres sizing. RFC-002 section 9 holds the throughput math this builds on; RFC-008 holds the per-region growth assumptions; RFC-011 turns the projected sizing into provisioned capacity. The capacity dashboard (section 8) shows current check rate vs provisioned capacity so the headroom is visible, not assumed.

### 9.3 Cost observability

Region is both an entitlement and a real COGS dimension (PRD-007 section 8, master section 11): our own regions cost us differently, premium regions cost more, and fan-out multiplies cost with the selected region count. We make that cost visible so margin stays managed.

| Cost SLI | Derived from | Use |
|----------|--------------|-----|
| Cost per check (per region) | per-region check rate (`pulse_check_results_total` by region) x the region's cost class (the `cost_class` from the region catalog, PRD-007 section 2) plus the mirror egress for that region | per-region COGS and per-region margin tracking (PRD-007 section 8) |
| Cost per check by plan tier | check rate by `plan_tier_bucket` x cost class | confirms cost-aware scheduling keeps low tiers off premium regions (PRD-007 section 8), and shows margin by tier |
| Cross-region mirror egress | the mirrored `check.results` + `region.health` volume per region (RFC-002 section 7.5) | the bounded egress cost; it should track result volume and only on paid premium regions |
| Infra cost | the managed Postgres/Redis/Kafka and the cluster cost, pulled from the cloud billing export, broken down per environment and per region | the infra cost dashboard; the denominator for cost-per-check |

The cost-observability metrics use `region` and `plan_tier_bucket` labels only, never `org_id` (cardinality rule, section 2.2); per-org cost, when needed for a specific large customer's margin, is a Postgres analytics query joining the org's monitor/region/interval config against the cost classes, not a metric. The cost-and-capacity dashboard (section 8) surfaces cost per check by region and by tier next to the capacity headroom so a margin problem (a tier running hot on premium regions) is visible. The deep cost model and cost-aware scheduling decisions are owned by RFC-008 and PRD-007; this RFC owns making the cost measurable.

---

## 10. Runbooks and incident response (SRE)

### 10.1 The runbook set to author

Each runbook is short and action-first: symptom, how to confirm, what to do, how to verify recovery, who to escalate to. Each is linked from the matching alert's `runbook` annotation (section 7).

| Runbook | Covers |
|---------|--------|
| region-down | confirm via `region.health` and `healthy_workers`; confirm it is coverage-degraded (handled), not a false-positive risk; decide on failover for plans that allow it (RFC-008); when not to page (RFC-000 section 4.1) |
| kafka-lag | confirm the lagging group; check whether the HPA scaled and why not if it did not; check the downstream (Postgres for alerting, outbound targets for notifier); the catch-up budget (24h retention, RFC-002 section 8.4) |
| postgres-failover | confirm primary loss; the managed-failover steps (RFC-011 owns the mechanics); how api fails (writes 5xx, reads to replicas, RFC-000 section 2.1); verify recovery and replica re-sync |
| scheduler-leader | confirm leader count via `pulse_scheduler_is_leader`; zero leaders (no dispatch) vs two (split brain); the k8s Lease recovery (RFC-000 section 11.2); bound the schedule gap by the rebuild duration |
| notification-backlog | confirm `notify.events`/`webhook.delivery` lag and delivery-failure rate; distinguish our backlog from a third-party channel being down; the in-handler retry vs DLQ behavior (RFC-002 section 8.3); when a backlog threatens the 30s SLO |
| dlq-triage | inspect a `*.dlq` topic, find why a message poisoned, decide fix-and-replay vs drop (replay tooling is RFC-002 open question 6 / RFC-011) |
| error-budget-burn | read the SLO board, identify which SLO and which service, apply the error-budget policy (section 5.5) |
| api-errors, redis-degraded, kafka-health, cert-renewal, pipeline-down | one each, matching the alerts in section 7 |

Owner note: this RFC authors the application-side runbooks (region-down, kafka-lag, scheduler-leader, notification-backlog, dlq-triage, error-budget-burn, pipeline-down). The infra-mechanics runbooks (postgres-failover mechanics, cluster recovery, cert renewal automation) are co-owned with RFC-011, which owns the deployment side.

### 10.2 On-call rotation stance

| Aspect | Stance |
|--------|--------|
| Rotation | a single on-call rotation across the five services for v1 (the team is small, the services share one module and one deploy). It splits into per-area rotations only when team size and paging volume justify it |
| Paging path | internal Alertmanager -> the paging tool for product-down and SLO-burn; the external watchdog and dead-man's switch (section 6) page through their own independent path so a total Pulse outage still reaches a human |
| Escalation | a primary and a secondary; the secondary is paged if the primary does not ack within a set window |
| Toil control | every recurring page must produce either a fix or a runbook improvement, so the rotation gets quieter over time rather than normalizing noise |

### 10.3 Incident review (blameless)

After every customer-impacting incident or any month where an SLO budget runs out (section 5.5), we run a blameless incident review:

| Element | Content |
|---------|---------|
| Timeline | what happened and when, reconstructed from traces, logs, and alerts (this is why the three pillars and the trace-id join exist) |
| Impact | which SLOs were affected and how much budget burned |
| Cause | the contributing causes, focused on the system and the process, not on a person |
| Actions | concrete follow-ups with owners; tracked to done |

The review is blameless on purpose: people surface the real cause only when they are not at risk for it. The output is system and process changes, and it feeds the change-management record below.

### 10.4 Change management toward SOC 2

This ties to PRD-013 (the SOC 2 path is designed in, not retrofitted) and master section 13:

| Control | How this RFC supports it |
|---------|--------------------------|
| Change management | every deploy is tracked (`pulse_build_info` shows what is running; the deploy pipeline records who shipped what when, RFC-011); the error-budget policy is the documented control on risky change |
| Availability monitoring | the SLOs, the self-SLA, and the external watchdog are the evidence that availability is monitored and measured, which a SOC 2 audit asks for |
| Incident response | the runbook set, the on-call rotation, and the blameless incident-review process are the documented incident-response control |
| Audit and access | observability data access (Grafana, Loki, Tempo) is access-controlled; secrets and PII are kept out of logs (section 3.4), which supports the data-handling controls |

The point of stating these now is the same as PRD-013's: we do not want to make an observability choice that blocks the SOC 2 path later. Keeping audit (Postgres) separate from operational logs (Loki), keeping PII out of logs, and having a documented incident-review process are the choices that keep the path open.

---

## 11. Open questions and dependencies

### 11.1 Open questions

| # | Question | Owner |
|---|----------|-------|
| 1 | The exact tail-sampling rate for healthy traces (section 4.5) and the trace-retention window in Tempo are cost-driven and need a real-volume measurement before they are fixed | this RFC + RFC-011 (cost) |
| 2 | The third-party vendor for the external uptime probe and the dead-man's switch (section 6.3) is a procurement choice; the requirement (genuinely independent of our infrastructure) is fixed here, the vendor is not | RFC-011 / procurement |
| 3 | The burn-rate alert windows and thresholds (section 5.5, 7.2) start at the standard multi-window values and need tuning against real budget-burn behavior in the first months | this RFC, after launch |
| 4 | The canary's controllable target endpoint (section 5.6): a dedicated tiny internal service vs a feature toggle on an existing one. Either works; the deploy shape is RFC-011's call | RFC-011 |
| 5 | Per-org cost attribution (section 9.3): whether the Postgres analytics query is enough or a periodic per-org cost rollup table is warranted for large-customer margin reviews | RFC-008 (cost model) + RFC-001 (rollup) |
| 6 | The paging tool vendor (PagerDuty vs alternative, section 7.1) and whether the external watchdog routes to the same tool through an independent integration or to a separate path entirely | RFC-011 |

### 11.2 Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-000 | the three-pillar standards (section 9), the SLO list (section 12), the service catalog (section 2), the deployment topology (section 11) |
| RFC-002 | the trace-context-over-Kafka header contract (`traceparent`, `pulse-correlation-id`, section 2.4), the consumer-lag SLI seam (section 8.1), the DLQ alert seam (section 8.2), the timestamps (`checked_at`) the end-to-end notification SLI rides on |
| PRD-012 | the committed SLO targets and the 99.9% control-plane and pipeline SLA |
| PRD-014 | the North Star and the self-measured SLA-attainment KPI |
| PRD-007 / RFC-008 | the per-region cost class and the cost model behind the cost-observability metrics |

| Depends on this RFC | For |
|---------------------|-----|
| Every service RFC (RFC-003..009) | the standard metric names, types, and labels their service must emit; the structured-log field contract; the span shape on their leg of the pipeline; the cardinality rule |
| RFC-011 | deploying Prometheus, Alertmanager, Grafana, the OTel collector, Loki, and Tempo; provisioning the alert rules and dashboards as code; the external watchdog and dead-man's switch; the paging-tool integration; sizing the clusters from the capacity model |
| RFC-004 (scheduler) | the scheduling-accuracy SLI (`pulse_schedule_dispatch_lag_seconds`) and the leader-state metric |
| RFC-006 (alerting) | the verdict-latency SLI and the canary-incident handling |
| RFC-007 (notifier) | the notification-delivery SLI and the canary-notification sink |

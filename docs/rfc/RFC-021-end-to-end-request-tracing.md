# RFC-021 - End-to-End Request Tracing (Frontend Origin)

Status: SHIPPED (core pipeline). The FE-to-backend trace is built end to end; remaining items are noted as future where they still apply.
Author: Engineering (platform)
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 9 observability pillars, section 14 reuse map)
Depends on: RFC-010 (the internal one-trace-per-check span tree, the collector, sampling, the `internal/obs` tracer wiring), RFC-002 (trace context over Kafka headers), RFC-012 (`api/openapi/v1.yaml` is the source of truth), RFC-013 (the SPA and its api client)
Depended on by: nothing yet

House style: timestamps RFC3339 UTC on the wire. No em-dashes. Tables and code blocks over prose.

Relationship to RFC-010: RFC-010 designed the backend trace (api inward, across Kafka). This RFC adds the piece RFC-010 does not cover, the **frontend origin**, and wires the edge glue that makes one trace id span the user's click and every service. It delegates the deep backend span tree and the collector/sampling to RFC-010 rather than redefining them.

---

## 1. Overview and scope

A user clicks something in the SPA, the api handles it, and depending on the action the work spreads across the scheduler, worker, alerting, and notifier over Kafka. One trace id starts at the frontend request and follows it through every service (section 2), so an observability tool shows the whole thing as a single trace, and a user or support engineer can take one id off a failed action and find exactly where it went.

### 1.1 What this RFC owns

| Owned | Section |
|-------|---------|
| Where the trace starts: FE-rooted via the OTel web SDK | 3 |
| The edge glue: set the global W3C propagator, add the api inbound server span | 4 |
| FE participation: mint and send `traceparent` on api calls, surface the id | 5 |
| Reconciling the existing `pulse-correlation-id` rail with the W3C trace id | 7 |
| Surfacing the id to users and support | 8 |
| Privacy of browser-side tracing | 10 |

### 1.2 What this RFC does not own

| Not owned | Owner |
|-----------|-------|
| The backend span tree (scheduler / worker / alerting / notifier spans) and their names | RFC-010 section 4.1 |
| Trace context over Kafka headers | RFC-002 section 2.4 / RFC-010 |
| The OTLP collector, the trace backend (Tempo), tail sampling, exemplars | RFC-010 / RFC-011 |
| The metrics and logs pillars | RFC-010 |

---

## 2. Current state (what is actually wired)

From the code, this is built end to end:

| Piece | State | Citation |
|-------|-------|----------|
| FE sends a trace header | built, `traceparent` on every api call | `web/src/api/client.ts` (`buildInit` `traceHeaders`), `web/src/tracing.ts` |
| api inbound server span | built, `otelhttp` server span extracts the incoming `traceparent` | `internal/api/build.go` (`chain()`, imports `otelhttp`) |
| Global `TextMapPropagator` set | built, `otel.SetTextMapPropagator` is called so inject/extract work at every boundary | `internal/obs/trace.go` |
| Tracer provider | OTLP gRPC exporter when `otlpEndpoint` is set; stdout is the local-dev fallback | `internal/obs/trace.go` |
| Backend spans (scheduler/worker/alerting/notify) | built, one trace `schedule.dispatch` -> `notify.deliver`, plus outbound client spans for checks and notifications | scheduler/worker/alerting/notify span starts |
| Cross-service id rail | the bus carries `traceparent` with producer/consumer spans | `internal/bus/bus.go`, `kafka.go`, `redis.go` |

So a user's click and every service share one trace id today. The FE roots the trace, the api continues it, and the bus carries it across the backend services.

---

## 3. Where the trace starts

### 3.1 Decision

The **FE is the root** of the trace. The SPA runs a full OTel web SDK (`@opentelemetry/sdk-trace-web` `WebTracerProvider` + `OTLPTraceExporter` + the W3C propagator, `web/src/tracing.ts`) and sends `traceparent` on every api call. The api extracts it and makes its server span a child, so the trace starts at the user's click. When a request arrives with no `traceparent`, the api starts a fresh root span instead.

The one non-FE origin is the scheduler's periodic checks: those are not a user action, so their trace starts at the scheduler (RFC-010). Everything a user triggers is FE-rooted.

The browser SDK records real browser spans (page load, resource timing, the click-to-fetch gap) and exports them over OTLP, with the privacy surface that brings (section 10).

### 3.2 Reasoning

Rooting the trace on the FE is what makes "one id from the click across every service" possible. The SPA holds the trace id it started, puts it on the request header, and the api continues it. The OTel web SDK also records browser spans (page load, resource timing) and exports them over OTLP, which brings an export path and a privacy surface (section 10).

### 3.3 Rejected alternatives

| Alternative | Why not |
|-------------|---------|
| api-edge root, FE only reads the id back | The FE is not the origin then, which is the whole point of tracing from the click. It also needs an api-echo path the FE-root design does not (the FE already holds the id it started) |
| Hand-rolled `traceparent` with no SDK | Would give the end-to-end id without browser-span depth or an export path. We shipped the full OTel web SDK instead so the trace also carries real browser timing; it brings the export path and privacy surface (section 10) |

---

## 4. The edge glue

Two small pieces carry the trace across the edge (section 2). This RFC's edge work is built.

### 4.1 Set the global propagator

`internal/obs/trace.go` sets the W3C propagator so inject/extract work at every boundary:

```go
otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{}, propagation.Baggage{},
))
```

This one call is the difference between "context travels" and "context is silently dropped at every boundary". It is load-bearing for this feature, so it lives here.

### 4.2 The api inbound server span

The handler chain in `internal/api/build.go` (`chain()`) wraps the mux with the `otelhttp` handler. For each request it:

1. extracts the incoming `traceparent` (continues the FE trace when present, section 3),
2. starts the server span (a child of the FE's `traceparent` for user-initiated requests, a fresh root only for scheduler-initiated work) named by route, with `http.method`, `http.route`, and the authenticated `org_id` / `user_id` as span attributes once the auth middleware has run,
3. puts the span context on the request `ctx` so every downstream call (DB, bus produce) and every log line carries the trace id.

The span wraps the whole mux, so it sits outside `identify` / `requireOrg`; the attributes that need the principal are set after those run (a small bit of ordering, called out for the build).

### 4.3 Carrying the trace across the bus

The api span continues into the other services because the bus produce path carries the W3C `traceparent`. The producer injects it from the request ctx and the consumer restores it, with producer/consumer spans on each hop (`internal/bus`). The api span therefore flows across the bus into scheduler/worker/alerting/notifier as one trace.

## 5. Frontend participation

| Step | Change | Where |
|------|--------|-------|
| Start and send | the OTel web SDK starts a span and `buildInit` adds its `traceparent` (`traceHeaders`) to the request headers, on every request | `web/src/api/client.ts` (`buildInit`), `web/src/tracing.ts` |
| Hold the id | the api client has the trace id from the span it started, so it has it without reading anything back. No response echo needed | `web/src/api/client.ts` (`request`) |
| Surface it | show that id on error states (toasts, the error envelope detail) and behind a debug affordance, so a user or support can copy it | RFC-013 |

CORS note: the SPA is same-origin with the api behind the Vite proxy / nginx (RFC-013), so sending the `traceparent` request header needs no cross-origin dance. If a deployment serves them cross-origin, `traceparent` is not a CORS-safelisted request header, so the api must allow it with `Access-Control-Allow-Headers: traceparent` (and the preflight that brings).

## 6. Backend span tree (delegated to RFC-010)

From the api inward, the spans follow RFC-010 section 4.1: `schedule.dispatch`, `check.execute`, `verdict.apply`, `notify.deliver`, each a child of the propagated context, each tagging `monitor_id` / `region` / `result_id` / `incident_id` as attributes. This RFC does not redefine them. Two RFC-010 items make the chain unbroken, and both are built:

| Prerequisite | State |
|--------------|-----|
| Each service starts its span from the restored context | built, so the trace continues past the api |
| The bus injects/restores W3C `traceparent` over Kafka (RFC-002 section 2.4) | built, so the trace id survives the bus hop (section 7) |

The trace runs FE through the api and across the backend services as one trace.

## 7. Reconciling the `pulse-correlation-id` rail

The bus already carries a homegrown `pulse-correlation-id` (`bus.go:31`), seeded from `obs.CorrelationID(ctx)`, but nothing seeds it. With W3C trace context, the trace id is the single id. Decision: the bus carries `traceparent` (RFC-002 section 2.4 already specifies this), and the trace id becomes *the* id on every log line (`trace_id`, RFC-010 section 3).

| Option | Decision |
|--------|----------|
| Carry `traceparent` over the bus and retire `pulse-correlation-id` | preferred. One id, the W3C trace id, no parallel rail to keep in sync |
| Keep `pulse-correlation-id` as a fallback when tracing is disabled | acceptable as a transition: seed it from the trace id when a span exists, so existing consumers keep working while the W3C path lands. Retire once `traceparent`-over-Kafka is in |

Either way there is exactly one id end to end; the open item is only whether the legacy header lingers during transition (open question 12.2).

## 8. Surfacing the id to users and support

The payoff is that the id is reachable by a human, not just in a backend tool. The FE already holds the id it minted for each request (section 5), so it shows it on failures with no response plumbing. So "this action failed" comes with an id that support can take straight into Tempo/Grafana, and a power user can self-serve. This is the user-facing reason the feature exists, distinct from the operator-facing SLO tracing RFC-010 is about.

## 9. Sampling and the exporter (delegated)

The tracer provider exports over OTLP gRPC when `otlpEndpoint` is set (`internal/obs/trace.go`); stdout is only the local-dev fallback. OTLP -> collector -> Tempo is wired, so traces reach Grafana/Tempo (RFC-010 section 4 / RFC-011). Because the FE starts the context with the sampled flag on, the central tail sampler makes the real keep/drop call; it must keep failed and slow requests (RFC-010 section 4.5 must-trace) so a surfaced id from a failure actually lands on a trace.

## 10. Privacy of browser tracing

The browser SDK is built, so mind what leaves the browser:

| Concern | Handling |
|---------|----------|
| Span names / attributes leaking user data | never put PII, tokens, or full URLs with query secrets in span names or attributes; route names and ids only |
| The RUM export path | the browser exports to our collector, not a third party; same-origin or an allowlisted collector endpoint |
| Consent | browser RUM is telemetry; confirm it fits the privacy posture (RFC-015) |

These concerns are about the browser SDK's export path, which is live. Keep span names and attributes to route names and ids so no PII or tokens leave the browser.

## 11. Failure modes

| Failure | Behavior |
|---------|----------|
| Tracing disabled (`TracingEnabled` false) | no spans, no ids; the system runs exactly as today. The feature is additive and off-switchable |
| Propagator set but a downstream service has no span yet | trace is shallow (stops at that hop), not broken (section 6) |
| FE sends a malformed `traceparent` | the api's extract ignores it and starts a fresh root; no error |
| No `otlpEndpoint` set (local dev) | traces go to stdout only; ids are valid but not queryable in Grafana until an endpoint is set (section 9) |

## 12. Open questions

1. **RUM SDK.** Resolved: the full OTel web SDK is built (`web/src/tracing.ts`), so the trace carries real browser spans and exports them over OTLP (section 3.2).
2. **Legacy `pulse-correlation-id` lifetime.** Retire immediately on the W3C cutover, or keep one release as a fallback (section 7). Leaning keep-one-release for safety.

## 13. Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-010 | the backend span tree (section 6), the OTLP collector, sampling, and the `internal/obs` tracer this RFC adds the propagator to |
| RFC-002 section 2.4 | `traceparent` over Kafka, so the FE-rooted trace survives the bus hop (section 7) |
| RFC-012 | any response-shape change (the surfaced id) goes through `v1.yaml` and `make gen` |
| RFC-013 | the SPA api client (`buildInit` / `request`) and the FE surfacing of the id |
| `internal/api/build.go` (`chain()`) | where the `otelhttp` inbound server span wraps the mux |
| `internal/obs/trace.go` | where the global propagator is set |

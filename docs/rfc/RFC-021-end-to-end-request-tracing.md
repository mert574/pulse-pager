# RFC-021 - End-to-End Request Tracing (Frontend Origin)

Status: DRAFT for review
Author: Engineering (platform)
Parent: `docs/rfc/RFC-000-architecture-overview.md` (section 9 observability pillars, section 14 reuse map)
Depends on: RFC-010 (the internal one-trace-per-check span tree, the collector, sampling, the `internal/obs` tracer wiring), RFC-002 (trace context over Kafka headers), RFC-012 (`api/openapi/v1.yaml` is the source of truth), RFC-013 (the SPA and its api client)
Depended on by: nothing yet

House style: timestamps RFC3339 UTC on the wire. No em-dashes. Tables and code blocks over prose.

Relationship to RFC-010: RFC-010 designed the backend trace (api inward, across Kafka). This RFC adds the piece RFC-010 does not cover, the **frontend origin**, and wires the edge glue that makes one trace id span the user's click and every service. It delegates the deep backend span tree and the collector/sampling to RFC-010 rather than redefining them.

---

## 1. Overview and scope

A user clicks something in the SPA, the api handles it, and depending on the action the work fans out across the scheduler, worker, alerting, and notifier over Kafka. Today none of that shares an id (section 2). The goal: one trace id that starts at the frontend request and follows it through every service, so an observability tool shows the whole thing as a single trace, and a user or support engineer can take one id off a failed action and find exactly where it went.

### 1.1 What this RFC owns

| Owned | Section |
|-------|---------|
| Where the trace starts: FE-rooted in v1 (hand-rolled id), richer FE spans in phase 2 | 3 |
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

Honest baseline, from the code, not the design docs:

| Piece | State | Citation |
|-------|-------|----------|
| FE sends a trace/correlation header | no, nothing | `web/src/api/client.ts` (no trace header in `buildInit`) |
| api inbound server span | none | `internal/api/router.go` (no otel middleware) |
| Global `TextMapPropagator` set | no (`SetTextMapPropagator` never called), so inject/extract are no-ops everywhere | `internal/obs/trace.go` |
| Tracer provider | wired but stdout exporter only, off unless `TracingEnabled` | `internal/obs/trace.go:18-41`, `internal/runtime/runtime.go:121` |
| Backend spans (scheduler/worker/...) | none started | grep: no `tracer.Start` in those services |
| Cross-service id rail | a `pulse-correlation-id` header rides Kafka/Redis, but nothing ever seeds it | `internal/bus/bus.go:31,38`, `kafka.go:30-31`, `redis.go:40-41` |

So a request flows through the system with no shared id today. The plumbing to carry one over the bus half-exists; the origin, the propagator, and the spans do not. RFC-010 designed the backend trace but it is unbuilt. This RFC is what turns it on, starting at the FE.

---

## 3. Where the trace starts

### 3.1 Decision

v1: the **FE is the root** of the trace. The SPA mints a `traceparent` (a hand-rolled generator, not a full SDK, section 3.2) and sends it on every api call. The api extracts it and makes its server span a child, so the trace starts at the user's click. When a request arrives with no `traceparent`, the api starts a fresh root span instead.

The one non-FE origin is the scheduler's periodic checks: those are not a user action, so their trace starts at the scheduler (RFC-010). Everything a user triggers is FE-rooted from v1.

Phase 2 is not a different root, it is a richer FE span. The SPA upgrades the hand-rolled id to a full OTel web SDK that records real browser spans (page load, resource timing, the click-to-fetch gap) and exports them. The trace id and origin do not move, and the api does not change; phase 2 only adds browser-side depth and the export path (with the privacy surface that brings, section 10).

### 3.2 Reasoning

Minting a `traceparent` on the FE is cheap: the trace id and span id are 16 and 8 random bytes from `crypto.getRandomValues`, a few lines, no SDK and no bundle weight. The FE exports nothing in v1, it only puts the id on the request header, so there is no RUM exporter, no CORS to the collector, and no real privacy surface (it sends a random id, not browser data). That is what makes true FE origin affordable in v1.

What the RFC defers to phase 2 is only the rich RUM: an OTel web SDK that records and exports browser spans. That is real bundle size, an export path, and a privacy review (section 10), and the end-to-end id does not need any of it. So v1 already gives the whole "one id from the click across every service" outcome; phase 2 adds browser-side timing on top.

### 3.3 Rejected alternatives

| Alternative | Why not |
|-------------|---------|
| api-edge root, FE only reads the id back | The FE is not the origin then, which is the whole point of tracing from the click. It also needs an api-echo path the FE-mint design does not (the FE already holds the id it minted) |
| Full OTel web SDK on day one | Pulls a RUM export path, CORS to the collector, and a privacy review into v1 for browser-span depth the end-to-end id does not need. The cheap hand-rolled `traceparent` gives FE origin without it; the SDK is phase 2 |

---

## 4. The edge glue

Two small pieces are missing and block everything (section 2). This RFC builds them.

### 4.1 Set the global propagator

In `internal/obs` (alongside `SetupTracing`), set the W3C propagator so inject/extract stop being no-ops:

```go
otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
    propagation.TraceContext{}, propagation.Baggage{},
))
```

This one call is the difference between "context travels" and "context is silently dropped at every boundary". It could be RFC-010's to own, but it is unbuilt and load-bearing for this feature, so it is in scope here.

### 4.2 The api inbound server span

Wrap the handler returned by `Router()` (`internal/api/router.go:27`, which returns the `http.Handler`) in an OTel server-span middleware (the `otelhttp` handler, or a thin manual equivalent). For each request it:

1. extracts the incoming `traceparent` (continues the FE trace when present, section 3),
2. starts the server span (a child of the FE's `traceparent` for user-initiated requests, a fresh root only for scheduler-initiated work) named by route, with `http.method`, `http.route`, and the authenticated `org_id` / `user_id` as span attributes once the auth middleware has run,
3. puts the span context on the request `ctx` so every downstream call (DB, bus produce) and every log line carries the trace id.

The span wraps the whole mux, so it sits outside `identify` / `requireOrg`; the attributes that need the principal are set after those run (a small bit of ordering, called out for the build).

### 4.3 Carrying the trace across the bus

The api span continues into the other services only if the bus produce path carries the W3C `traceparent`. It does not yet: today it copies a custom `pulse-correlation-id` header (`kafka.go:30`, `redis.go:40`) and nothing seeds it (section 2). Setting the global propagator (4.1) does not change this on its own, because the bus copies that header by hand rather than going through the otel propagator. Teaching the produce path to inject `traceparent` from the request ctx is RFC-002 section 2.4 / RFC-010 work this RFC depends on (section 6); section 7 reconciles the legacy header with it.

## 5. Frontend participation

| Step | Change | Where |
|------|--------|-------|
| Mint and send | `buildInit` generates a `traceparent` (random 16-byte trace id + 8-byte span id from `crypto.getRandomValues`, sampled flag on) and adds it to the request headers, on every request | `web/src/api/client.ts:157` (`buildInit`) |
| Hold the id | the api client keeps the id it minted for the request, so it has the trace id without reading anything back. No response echo needed | `client.ts:204` (`request`) |
| Surface it | show that id on error states (toasts, the error envelope detail) and behind a debug affordance, so a user or support can copy it | RFC-013 |

CORS note: the SPA is same-origin with the api behind the Vite proxy / nginx (RFC-013), so sending the `traceparent` request header needs no cross-origin dance. If a deployment serves them cross-origin, `traceparent` is not a CORS-safelisted request header, so the api must allow it with `Access-Control-Allow-Headers: traceparent` (and the preflight that brings).

## 6. Backend span tree (delegated to RFC-010)

From the api inward, the spans are RFC-010 section 4.1's design: `schedule.dispatch`, `check.execute`, `verdict.apply`, `notify.deliver`, each a child of the propagated context, each tagging `monitor_id` / `region` / `result_id` / `incident_id` as attributes. This RFC does not redefine them. It depends on two RFC-010 items being built for the chain to be unbroken:

| Prerequisite | Why |
|--------------|-----|
| Each service starts its span from the restored context | without it, the trace stops at the api |
| The bus injects/restores W3C `traceparent` over Kafka (RFC-002 section 2.4), not just `pulse-correlation-id` | the trace id must survive the bus hop (section 7) |

If those are not yet built when this RFC's edge work ships, the trace is real but shallow (FE through the api, then it stops). That is still useful, and it gets deeper as RFC-010's spans land.

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

For traces to be viewable, the stdout exporter (`internal/obs/trace.go:23`) must become the OTLP-to-collector exporter, and tail sampling must run centrally. Both are RFC-010 section 4 / RFC-011 scope. This RFC notes the dependency: until the OTLP exporter is wired, traces exist in process (stdout) but are not in Grafana, so the surfaced id has nowhere to land yet. Because the FE mints the context with the sampled flag on, the central tail sampler makes the real keep/drop call; it must keep failed and slow requests (RFC-010 section 4.5 must-trace) so a surfaced id from a failure actually lands on a trace.

## 10. Privacy of browser tracing (phase 2)

When the browser SDK lands, mind what leaves the browser:

| Concern | Handling |
|---------|----------|
| Span names / attributes leaking user data | never put PII, tokens, or full URLs with query secrets in span names or attributes; route names and ids only |
| The RUM export path | the browser exports to our collector, not a third party; same-origin or an allowlisted collector endpoint |
| Consent | browser RUM is telemetry; confirm it fits the privacy posture (RFC-015) before phase 2 ships |

v1 (hand-rolled `traceparent`) has almost none of this surface: the only thing new leaving the browser is a random id, not browser data, span names, or attributes. The concerns above are about the RUM SDK's export path, which is phase 2.

## 11. Failure modes

| Failure | Behavior |
|---------|----------|
| Tracing disabled (`TracingEnabled` false) | no spans, no ids; the system runs exactly as today. The feature is additive and off-switchable |
| Propagator set but a downstream service has no span yet | trace is shallow (stops at that hop), not broken (section 6) |
| FE sends a malformed `traceparent` | the api's extract ignores it and starts a fresh root; no error |
| OTLP exporter not yet wired | traces go to stdout only; ids are valid but not yet queryable in Grafana (section 9) |

## 12. Open questions

1. **RUM SDK in phase 2.** v1 mints the id with a hand-rolled `traceparent` generator (decided, section 3.2). The phase-2 question is only whether the richer browser spans (page load, resource timing) are worth a full OTel web SDK and its export path. Decide at phase 2.
2. **Legacy `pulse-correlation-id` lifetime.** Retire immediately on the W3C cutover, or keep one release as a fallback (section 7). Leaning keep-one-release for safety.

## 13. Dependencies

| This RFC depends on | For |
|---------------------|-----|
| RFC-010 | the backend span tree (section 6), the OTLP collector, sampling, and the `internal/obs` tracer this RFC adds the propagator to |
| RFC-002 section 2.4 | `traceparent` over Kafka, so the FE-rooted trace survives the bus hop (section 7) |
| RFC-012 | any response-shape change (the surfaced id) goes through `v1.yaml` and `make gen` |
| RFC-013 | the SPA api client (`buildInit` / `request`) and the FE surfacing of the id |
| `internal/api/router.go:27` | the `http.Handler` the inbound server span wraps |
| `internal/obs/trace.go` | where the global propagator is set |

// Browser tracing (RFC-021 phase 2). The SPA becomes the trace root: each api call
// gets a real span that is exported, so a trace's root span exists in Tempo (no more
// "root span not yet received"), and the api span is a child of the browser span.
//
// Spans go OTLP/HTTP to the collector over the same-origin /v1/traces path (dev: the
// Vite proxy forwards it to the collector; prod: the ingress routes it), so nothing
// new is cross-origin. Per RFC-021 section 10 we keep PII/tokens/query strings out of
// span names and attributes; the api client names spans by method + path only.

import { trace } from "@opentelemetry/api";
import {
  WebTracerProvider,
  BatchSpanProcessor,
  StackContextManager,
} from "@opentelemetry/sdk-trace-web";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-http";
import { resourceFromAttributes } from "@opentelemetry/resources";
import { W3CTraceContextPropagator } from "@opentelemetry/core";
import { ATTR_SERVICE_NAME } from "@opentelemetry/semantic-conventions";

// Same-origin OTLP endpoint. Dev proxies it to the collector (vite.config.ts); a real
// deployment routes /v1/traces to the collector at the edge (deferred with the rest of
// the ingress work).
const OTLP_TRACES_URL = "/v1/traces";

let started = false;

// initTracing registers the global tracer provider, context manager, and W3C
// propagator. Call once at app bootstrap, before any api request.
export function initTracing(): void {
  if (started) return;
  started = true;

  const provider = new WebTracerProvider({
    resource: resourceFromAttributes({ [ATTR_SERVICE_NAME]: "web" }),
    spanProcessors: [
      new BatchSpanProcessor(new OTLPTraceExporter({ url: OTLP_TRACES_URL })),
    ],
  });
  provider.register({
    contextManager: new StackContextManager(),
    propagator: new W3CTraceContextPropagator(),
  });
}

// The app tracer. getTracer returns a proxy that binds to whatever provider is
// registered, so reading it at module load (before initTracing) is fine.
export const tracer = trace.getTracer("web");

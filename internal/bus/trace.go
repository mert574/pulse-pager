package bus

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"pulse/internal/obs"
)

// busTracer is the instrumentation scope for the produce/consume spans.
const busTracer = "pulse/bus"

// startProduceSpan opens a PRODUCER span around a publish. The span is active while the
// backend injects the trace context, so the consumed message's parent is this span; that
// producer/consumer pair is what Tempo's service-graph processor turns into a
// service-to-service edge (RFC-010 section 4.1, 4.2). sys is the backend ("kafka"/"redis").
func startProduceSpan(ctx context.Context, sys, topic string) (context.Context, trace.Span) {
	return otel.Tracer(busTracer).Start(ctx, topic+" publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", sys),
			attribute.String("messaging.destination.name", topic),
			attribute.String("messaging.operation.name", "publish"),
		),
	)
}

// startConsumeSpan opens a CONSUMER span around handling one record. It is a child of the
// producer span restored from the message headers, so the handler's own span (e.g.
// check.execute) nests under it and the service graph draws the cross-service edge.
func startConsumeSpan(ctx context.Context, sys, topic string) (context.Context, trace.Span) {
	return otel.Tracer(busTracer).Start(ctx, topic+" process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", sys),
			attribute.String("messaging.destination.name", topic),
			attribute.String("messaging.operation.name", "process"),
		),
	)
}

// injectTrace writes the W3C trace context from ctx into a flat header map using the
// global propagator (RFC-021 section 4.3, RFC-002 section 2.4). The two backends copy
// the returned keys into their native carriers (Kafka headers, Redis stream values),
// so the FE-rooted trace continues across the bus. It also keeps the legacy
// pulse-correlation-id populated from the ctx correlation id for one transition
// release, so a consumer that has not adopted traceparent still gets an id (section 7).
func injectTrace(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if id := obs.CorrelationID(ctx); id != "" {
		carrier[correlationHeader] = id
	}
	return carrier
}

// restoreTrace rebuilds the context from a consumed message's headers: it extracts the
// W3C trace context so the handler (and any onward produce) continue the same trace,
// and sets the correlation id to the trace id so the log lines match. With no
// traceparent it falls back to the legacy correlation header (section 7).
func restoreTrace(ctx context.Context, headers map[string]string) context.Context {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(headers))
	if id := obs.TraceID(ctx); id != "" {
		return obs.WithCorrelationID(ctx, id)
	}
	if id := headers[correlationHeader]; id != "" {
		return obs.WithCorrelationID(ctx, id)
	}
	return ctx
}

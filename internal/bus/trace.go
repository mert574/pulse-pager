package bus

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"pulse/internal/obs"
)

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

package bus

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"pulse/internal/obs"
)

func w3cProp() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

// A produced message carries the W3C traceparent, and a consumed one restores the same
// trace id onto the handler ctx, with the correlation id set to the trace id (RFC-021
// sections 4.3 and 7). This is the round trip the two backends rely on.
func TestTraceRoundTripsOverBus(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	defer otel.SetTextMapPropagator(prev)
	otel.SetTextMapPropagator(w3cProp())

	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	headers := injectTrace(ctx)
	if headers["traceparent"] == "" {
		t.Fatal("injectTrace wrote no traceparent")
	}

	restored := restoreTrace(context.Background(), headers)
	if got := obs.TraceID(restored); got != traceID.String() {
		t.Fatalf("restored trace id = %q, want %q", got, traceID.String())
	}
	if got := obs.CorrelationID(restored); got != traceID.String() {
		t.Fatalf("restored correlation id = %q, want trace id %q", got, traceID.String())
	}
}

// With no traceparent (a producer that had no span, e.g. a scheduled check before
// RFC-010), restore falls back to the legacy pulse-correlation-id so an id still flows
// during the transition release (RFC-021 section 7).
func TestRestoreFallsBackToLegacyCorrelationHeader(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	defer otel.SetTextMapPropagator(prev)
	otel.SetTextMapPropagator(w3cProp())

	restored := restoreTrace(context.Background(), map[string]string{correlationHeader: "legacy-123"})
	if got := obs.CorrelationID(restored); got != "legacy-123" {
		t.Fatalf("correlation id = %q, want legacy fallback %q", got, "legacy-123")
	}
}

// A message with no headers at all restores cleanly to a context with no id, rather
// than panicking on a nil carrier.
func TestRestoreWithNoHeaders(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	defer otel.SetTextMapPropagator(prev)
	otel.SetTextMapPropagator(w3cProp())

	restored := restoreTrace(context.Background(), nil)
	if got := obs.CorrelationID(restored); got != "" {
		t.Fatalf("correlation id = %q, want empty", got)
	}
}

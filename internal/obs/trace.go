package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// SetupTracing wires an OpenTelemetry tracer provider. In the barebones it uses
// the stdout exporter when enabled; when disabled it leaves the global provider
// as the OTel default no-op, so tracing calls are cheap and harmless. The real
// OTLP-to-collector exporter (RFC-010 section 1.3) swaps in here later without
// touching callers. Returns a shutdown func to flush spans on exit.
func SetupTracing(ctx context.Context, service string, enabled bool) (func(context.Context) error, error) {
	// Set the global W3C propagator up front, even when tracing is off, so trace
	// context travels across the api edge and the bus (inject/extract are otherwise
	// silent no-ops). With tracing off the tracer is still the no-op, so this just
	// carries the ids; it records no spans (RFC-021 section 4.1).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if !enabled {
		return func(context.Context) error { return nil }, nil
	}

	exp, err := stdouttrace.New(stdouttrace.WithoutTimestamps())
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", service),
	))
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// SpanSetString sets a string attribute on the active span, if one is recording.
// It lets callers outside the otel import (e.g. the auth middleware) tag the
// request's root span without depending on otel directly (RFC-021 section 4.2).
func SpanSetString(ctx context.Context, key, value string) {
	if s := trace.SpanFromContext(ctx); s.IsRecording() {
		s.SetAttributes(attribute.String(key, value))
	}
}

// SpanSetInt sets an int64 attribute on the active span, if one is recording.
func SpanSetInt(ctx context.Context, key string, value int64) {
	if s := trace.SpanFromContext(ctx); s.IsRecording() {
		s.SetAttributes(attribute.Int64(key, value))
	}
}

// TraceID returns the active span's trace id as a hex string, or "" if there is
// no valid trace context. The api uses it as the correlation id so every log line
// carries the same id as the trace (RFC-021 section 7).
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

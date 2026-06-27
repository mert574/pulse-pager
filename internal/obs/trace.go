package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// SetupTracing wires an OpenTelemetry tracer provider. When disabled it leaves the
// global provider as the OTel default no-op, so tracing calls are cheap and harmless.
// When enabled it exports OTLP to the collector if otlpEndpoint is set (RFC-010
// section 4.3), else to stdout for local dev without the collector stack. Tail
// sampling lives at the collector, so the service exports every span it records.
// Returns a shutdown func to flush spans on exit.
func SetupTracing(ctx context.Context, service string, enabled bool, otlpEndpoint string) (func(context.Context) error, error) {
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

	exp, err := newSpanExporter(ctx, otlpEndpoint)
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

// newSpanExporter builds the OTLP gRPC exporter to the collector when otlpEndpoint is
// set, else the stdout exporter for local dev without the stack. OTLP is plaintext
// (insecure): in dev it is localhost, and in the cluster it is in-namespace traffic to
// the collector, both inside the trust boundary (RFC-011 section 7).
func newSpanExporter(ctx context.Context, otlpEndpoint string) (sdktrace.SpanExporter, error) {
	if otlpEndpoint == "" {
		return stdouttrace.New(stdouttrace.WithoutTimestamps())
	}
	return otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(otlpEndpoint),
		otlptracegrpc.WithInsecure(),
	)
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

// AddEvent records a span event (a timestamped, log-like marker) on the active span,
// if one is recording. Use it for notable steps inside a request so they show on the
// trace (RFC-010). No-op when nothing is recording.
func AddEvent(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	if s := trace.SpanFromContext(ctx); s.IsRecording() {
		s.AddEvent(name, trace.WithAttributes(attrs...))
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

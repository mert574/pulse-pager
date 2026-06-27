package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope the pipeline services start spans under.
// The per-service service.name resource attribute (set in SetupTracing) is what tells
// the services apart in the backend; this scope name is the same across them so the
// one-trace-per-check tree reads as one instrument (RFC-010 section 4.1).
const tracerName = "pulse"

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

// StartSpan starts a child span named name under the active span in ctx (or a root
// when ctx carries a restored trace context but no local span, which is the bus-consume
// case). It returns the child's context and a func that ends the span; the caller does
// `ctx, end := obs.StartSpan(...); defer end()`. With tracing off the tracer is the
// no-op, so this is cheap and the returned ctx is unchanged (RFC-010 section 4.1).
func StartSpan(ctx context.Context, name string) (context.Context, func()) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, name)
	return ctx, func() { span.End() }
}

// SpanError records err on the active span and sets its status to Error, so a failed
// pipeline hop (a redelivered job) shows red in the trace. No-op on a nil err or when
// nothing is recording.
func SpanError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if s := trace.SpanFromContext(ctx); s.IsRecording() {
		s.RecordError(err)
		s.SetStatus(codes.Error, err.Error())
	}
}

// SpanSetBool sets a bool attribute on the active span, if one is recording.
func SpanSetBool(ctx context.Context, key string, value bool) {
	if s := trace.SpanFromContext(ctx); s.IsRecording() {
		s.SetAttributes(attribute.Bool(key, value))
	}
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

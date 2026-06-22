package obs

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SetupTracing wires an OpenTelemetry tracer provider. In the barebones it uses
// the stdout exporter when enabled; when disabled it leaves the global provider
// as the OTel default no-op, so tracing calls are cheap and harmless. The real
// OTLP-to-collector exporter (RFC-010 section 1.3) swaps in here later without
// touching callers. Returns a shutdown func to flush spans on exit.
func SetupTracing(ctx context.Context, service string, enabled bool) (func(context.Context) error, error) {
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

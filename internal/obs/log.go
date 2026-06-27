// Package obs is the cross-cutting observability wiring shared by every service:
// a structured logger, a Prometheus registry, optional tracing, and the small
// health/metrics HTTP server each service mounts. It is the RFC-010 three-pillar
// standard in barebones form; per-service SLI metrics land with each service.
package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

type ctxKey int

const corrIDKey ctxKey = iota

// Logger returns a JSON slog logger tagged with the service name. It fans out to two
// handlers: stdout (so a human watching the process sees the lines) and an OTLP bridge
// that ships each record to the global LoggerProvider, which SetupLogging wires to the
// collector and on to Loki (RFC-010 section 3). The OTLP bridge reads the trace context
// off the call's ctx, so a line logged with InfoContext(ctx, ...) carries its trace_id
// and joins to its trace in Grafana. Until SetupLogging runs the global provider is a
// no-op delegate, so this is safe to build at boot. Keep attributes low-cardinality
// (RFC-010 section 2.2): service, level, region, never monitor_id.
func Logger(service, level string) *slog.Logger {
	lvl := parseLevel(level)
	stdout := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	otlp := otelslog.NewHandler(service)
	h := &multiHandler{handlers: []slog.Handler{stdout, otlp}}
	return slog.New(h).With("service", service)
}

// SetupLogging wires the OTel LoggerProvider that the slog OTLP bridge ships to. When
// disabled or with no OTLP endpoint it leaves the global provider as the no-op default,
// so logging stays stdout-only. Otherwise it exports OTLP to the collector (RFC-010
// section 3). Returns a shutdown func to flush on exit. Mirrors SetupTracing.
func SetupLogging(ctx context.Context, service string, enabled bool, otlpEndpoint string) (func(context.Context) error, error) {
	if !enabled || otlpEndpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(otlpEndpoint),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", service),
	))
	if err != nil {
		return nil, err
	}
	lp := log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(exp)),
		log.WithResource(res),
	)
	otellog.SetLoggerProvider(lp)
	return lp.Shutdown, nil
}

// multiHandler fans one slog record out to several handlers (stdout + the OTLP bridge),
// so every line is both human-readable on stdout and shipped to Loki.
type multiHandler struct{ handlers []slog.Handler }

func (m *multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: hs}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: hs}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// WithCorrelationID stamps a correlation/trace id onto the context. The bus
// package carries this across Kafka so one check can be followed end to end
// (RFC-010 section 1.2). Header name reserved: pulse-correlation-id.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, corrIDKey, id)
}

// CorrelationID reads the correlation id, or "" if none is set.
func CorrelationID(ctx context.Context) string {
	if v, ok := ctx.Value(corrIDKey).(string); ok {
		return v
	}
	return ""
}

// LoggerFrom returns the logger with the context's correlation id attached, if any.
// The trace id rides the ctx for the OTLP bridge; the correlation_id attribute keeps the
// id visible on the stdout line too.
func LoggerFrom(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := CorrelationID(ctx); id != "" {
		return base.With("correlation_id", id)
	}
	return base
}

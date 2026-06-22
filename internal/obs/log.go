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
)

type ctxKey int

const corrIDKey ctxKey = iota

// Logger returns a JSON slog logger tagged with the service name. Output goes to
// stdout so the container runtime (Loki in prod) collects it. Keep attributes
// low-cardinality (RFC-010 section 2.2): service, level, region, never monitor_id.
func Logger(service, level string) *slog.Logger {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)})
	return slog.New(h).With("service", service)
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
func LoggerFrom(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := CorrelationID(ctx); id != "" {
		return base.With("correlation_id", id)
	}
	return base
}

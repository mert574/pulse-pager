package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"pulse/internal/obs"
)

func setTracing(t *testing.T, tp *sdktrace.TracerProvider) {
	t.Helper()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	otel.SetTracerProvider(tp)
}

// A request carrying a traceparent continues that trace: the server span has the
// inbound trace id and is a child of the inbound span, and the inbound trace id
// reaches the handler ctx as the correlation id (RFC-021 sections 4.2 and 7).
func TestChainContinuesInboundTrace(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	setTracing(t, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))

	const traceID = "0af7651916cd43dd8448eb211c80319c"
	const parentSpanID = "b7ad6b7169203331"

	var gotCorr string
	h := chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotCorr = obs.CorrelationID(r.Context())
	}), slog.Default(), newHTTPMetrics(prometheus.NewRegistry()))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-"+parentSpanID+"-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotCorr != traceID {
		t.Fatalf("correlation id on ctx = %q, want inbound trace id %q", gotCorr, traceID)
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d server spans, want 1", len(spans))
	}
	if got := spans[0].SpanContext().TraceID().String(); got != traceID {
		t.Fatalf("server span trace id = %q, want %q", got, traceID)
	}
	if got := spans[0].Parent().SpanID().String(); got != parentSpanID {
		t.Fatalf("server span parent = %q, want inbound span id %q", got, parentSpanID)
	}
}

// With no traceparent, or a malformed one, the api starts a fresh root: a real
// correlation id is set and the bad value never leaks into it (RFC-021 section 11).
func TestChainStartsRootWithoutValidTraceparent(t *testing.T) {
	setTracing(t, sdktrace.NewTracerProvider())

	for _, tc := range []struct{ name, hdr string }{
		{"none", ""},
		{"malformed", "not-a-traceparent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotCorr string
			h := chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				gotCorr = obs.CorrelationID(r.Context())
			}), slog.Default(), newHTTPMetrics(prometheus.NewRegistry()))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			if tc.hdr != "" {
				req.Header.Set("traceparent", tc.hdr)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if gotCorr == "" {
				t.Fatal("expected a fresh correlation id, got empty")
			}
			if gotCorr == tc.hdr {
				t.Fatalf("bad traceparent %q leaked into the correlation id", tc.hdr)
			}
		})
	}
}

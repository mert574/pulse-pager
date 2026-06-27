package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ReadyCheck is one dependency probe run by /readyz (e.g. "postgres", "redis").
type ReadyCheck struct {
	Name  string
	Check func(context.Context) error
}

// HealthServer serves /healthz (liveness), /readyz (dependency readiness), and
// /metrics. Every service mounts one on its health address. Liveness is "the
// process is up"; readiness runs the registered dependency checks.
type HealthServer struct {
	srv    *http.Server
	checks []ReadyCheck
}

// NewHealthServer builds the server. checks are evaluated on each /readyz hit.
func NewHealthServer(addr string, reg *prometheus.Registry, checks ...ReadyCheck) *HealthServer {
	h := &HealthServer{checks: checks}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", h.handleReady)
	// EnableOpenMetrics so histogram exemplars (the trace ids attached via
	// ObserveWithTrace) are emitted in the exposition and Prometheus can scrape them
	// (RFC-010 section 2.6).
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	h.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return h
}

func (h *HealthServer) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	failures := map[string]string{}
	for _, c := range h.checks {
		if err := c.Check(ctx); err != nil {
			failures[c.Name] = err.Error()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if len(failures) > 0 {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unready", "failed": failures})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
}

// Start runs the server in the background. It returns immediately; a serve error
// (other than the clean close on Shutdown) is delivered on the returned channel.
func (h *HealthServer) Start() <-chan error {
	errc := make(chan error, 1)
	go func() {
		if err := h.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
		close(errc)
	}()
	return errc
}

// Shutdown gracefully stops the server.
func (h *HealthServer) Shutdown(ctx context.Context) error {
	return h.srv.Shutdown(ctx)
}

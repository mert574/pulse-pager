package obs

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

// ObserveWithTrace records v on the observer, attaching the active trace id as an
// exemplar when one is present so a Grafana p99 bucket links straight to the trace that
// was slow (RFC-010 section 2.6). It falls back to a plain Observe when there is no trace
// context or the observer does not support exemplars (so it is safe on any Observer).
func ObserveWithTrace(ctx context.Context, o prometheus.Observer, v float64) {
	if tid := TraceID(ctx); tid != "" {
		if eo, ok := o.(prometheus.ExemplarObserver); ok {
			eo.ObserveWithExemplar(v, prometheus.Labels{"trace_id": tid})
			return
		}
	}
	o.Observe(v)
}

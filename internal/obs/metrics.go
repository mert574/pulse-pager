package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// NewRegistry builds a Prometheus registry with the standard Go runtime and
// process collectors plus a build-info gauge. Per-service SLI metrics (schedule
// lag, verdict latency, delivery latency, etc, RFC-010 section 2.5) register
// against this same registry in their own packages later.
func NewRegistry(service string) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "pulse_build_info",
		Help:        "Build info, value is always 1.",
		ConstLabels: prometheus.Labels{"service": service},
	})
	buildInfo.Set(1)
	reg.MustRegister(buildInfo)
	return reg
}

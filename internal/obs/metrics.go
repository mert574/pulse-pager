package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// NewRegistry builds a Prometheus registry with the standard Go runtime and
// process collectors plus the build-info and up gauges. Per-service SLI metrics
// (schedule lag, verdict latency, delivery latency, etc, RFC-010 section 2.5) register
// against this same registry in their own packages.
//
// The `service` dimension is supplied by the scrape (one target/label per service in
// dev, pod relabeling in the cluster), not as a metric const label, so every series
// (including the Go/process collectors) carries it uniformly and there is no
// exported_service collision (RFC-010 section 2.4).
func NewRegistry(service string) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pulse_build_info",
		Help: "Build info, value is always 1.",
	})
	buildInfo.Set(1)
	up := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pulse_up",
		Help: "1 while the service is serving (its own view of liveness).",
	})
	up.Set(1)
	reg.MustRegister(buildInfo, up)
	return reg
}

// Package worker consumes check jobs for its region, runs the check by reusing
// internal/checker unchanged, and emits a check.results event for alerting
// (RFC-005). It does NOT write the check_results row: per ADR-0011 the worker
// emits the event only and the control-plane alerting consumer does the durable
// idempotent upsert, folded into the alerting transaction (RFC-006 section 5.4).
// The worker stays fully off the Postgres result path. It still writes the
// per-monitor last-failure snapshot, which is operational debug data, not the
// authoritative result row.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"pulse/internal/bus"
	"pulse/internal/checkstate"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/events"
	"pulse/internal/obs"
)

// Checker runs one monitor's HTTP check (internal/checker.Checker). captureResponse
// tells it whether to capture the response on a failure for the snapshot (PRD-002 3.8).
type Checker interface {
	Check(ctx context.Context, m *domain.Monitor, captureResponse bool) *domain.CheckResult
}

// ResultWriter persists the per-monitor last-failure snapshot. The durable check
// result row is NOT written here: alerting owns that upsert (ADR-0011).
type ResultWriter interface {
	UpsertMonitorLastFailure(ctx context.Context, orgID, monitorID int64, snap *domain.ResponseSnapshot, checkedAt time.Time) error
	// UpsertMonitorCert overwrites the per-ssl-monitor cert detail (BACKLOG: SSL-expiry).
	UpsertMonitorCert(ctx context.Context, orgID, monitorID int64, c *domain.CertInfo, checkedAt time.Time) error
}

// Consumer is the subset of the bus consumer the worker needs.
type Consumer interface {
	Poll(ctx context.Context, handler func(context.Context, bus.Record) error) error
}

// Producer emits the check.results event.
type Producer interface {
	Produce(ctx context.Context, topic, key string, value []byte) error
}

// Entitlements resolves per-org feature access (the worker only needs the read).
type Entitlements interface {
	For(orgID int64) entitlements.Set
}

// Runner ties the consume loop to the checker and the result write.
type Runner struct {
	store   ResultWriter
	cons    Consumer
	prod    Producer
	checker Checker
	ents    Entitlements
	state   checkstate.Store
	region  string
	log     *slog.Logger
	metrics *metrics
}

// New builds a Runner. state is the per-(monitor, region) live-state store (Redis); it
// may be nil (then the worker just runs checks without updating live state, e.g. in
// tests or a no-Redis dev setup). reg is the Prometheus registry the SLI metrics
// register on; nil leaves them unregistered (tests).
func New(store ResultWriter, cons Consumer, prod Producer, chk Checker, ents Entitlements, state checkstate.Store, region string, log *slog.Logger, reg *prometheus.Registry) *Runner {
	return &Runner{store: store, cons: cons, prod: prod, checker: chk, ents: ents, state: state, region: region, log: log, metrics: newMetrics(reg)}
}

// metrics holds the worker SLI metrics (RFC-010 section 2.5.3).
type metrics struct {
	jobsConsumed  *prometheus.CounterVec   // {region}
	checkDuration *prometheus.HistogramVec // {region, result}
	checkResults  *prometheus.CounterVec   // {region, healthy, failure_reason}
	emitFailures  *prometheus.CounterVec   // {region}
}

// newMetrics builds the metrics and registers them on reg. A nil reg builds them
// unregistered, so recording never panics in a test that passes no registry.
func newMetrics(reg *prometheus.Registry) *metrics {
	m := &metrics{
		jobsConsumed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pulse_worker_jobs_consumed_total",
			Help: "Check jobs consumed, by region.",
		}, []string{"region"}),
		checkDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pulse_check_duration_seconds",
			Help:    "Check execution time in seconds, by region and result (healthy/unhealthy/blocked).",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}, []string{"region", "result"}),
		checkResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pulse_check_results_total",
			Help: "Checks executed, by region, healthy, and failure_reason (none when healthy).",
		}, []string{"region", "healthy", "failure_reason"}),
		emitFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pulse_check_result_emit_failures_total",
			Help: "Failures emitting check.results to the bus, by region.",
		}, []string{"region"}),
	}
	if reg != nil {
		reg.MustRegister(m.jobsConsumed, m.checkDuration, m.checkResults, m.emitFailures)
	}
	return m
}

// resultLabel buckets a check outcome for the duration metric's `result` label.
func resultLabel(r *domain.CheckResult) string {
	if r.Healthy {
		return "healthy"
	}
	if r.FailureReason != nil && *r.FailureReason == domain.ReasonBlockedTarget {
		return "blocked"
	}
	return "unhealthy"
}

// Run consumes jobs until the context is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info(fmt.Sprintf("worker started, running checks for region %s", r.region), "region", r.region)
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := r.cons.Poll(ctx, func(recCtx context.Context, rec bus.Record) error {
			return r.handle(recCtx, rec)
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.log.Error("poll failed", "err", err)
			// back off briefly so a persistent error does not hot-loop
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
	}
}

func (r *Runner) handle(ctx context.Context, rec bus.Record) (err error) {
	var job events.CheckJob
	if err := json.Unmarshal(rec.Value, &job); err != nil {
		// a malformed job is poison: log and drop rather than blocking the partition
		r.log.Error("bad job payload, dropping", "err", err)
		return nil
	}

	// Continue the check trace from the dispatch span the scheduler put on the job's
	// bus headers (RFC-010 section 4.1). A failed emit returns an error to redeliver,
	// so record it on the span via the named return.
	ctx, end := obs.StartSpan(ctx, "check.execute")
	defer func() { obs.SpanError(ctx, err); end() }()
	obs.SpanSetInt(ctx, "monitor_id", job.Monitor.ID)
	obs.SpanSetInt(ctx, "org_id", job.OrgID)
	obs.SpanSetString(ctx, "region", job.Region)

	r.metrics.jobsConsumed.WithLabelValues(job.Region).Inc()

	m := job.Monitor
	// Mark this region in-flight so the monitor's live element shows it "pinging" while
	// the check runs (best-effort; never blocks the check). Every check drives this, not
	// just manual check-now.
	r.markRunning(ctx, &m, job.Region)

	// Decide capture up front from the org's plan: an ungated org never has its
	// response read for a snapshot, so it pays no hot-path cost (PRD-002 3.8, RFC-009).
	capture := r.ents.For(job.OrgID).FailureSnapshot
	checkStart := time.Now()
	result := r.checker.Check(ctx, &m, capture)
	result.OrgID = job.OrgID
	result.Region = job.Region

	// Check execution time and the result counters (RFC-010 section 2.5.3); the duration
	// carries the trace id as an exemplar so a slow check links to its trace.
	obs.ObserveWithTrace(ctx, r.metrics.checkDuration.WithLabelValues(job.Region, resultLabel(result)), time.Since(checkStart).Seconds())
	r.metrics.checkResults.WithLabelValues(job.Region, strconv.FormatBool(result.Healthy), reasonLabel(result.FailureReason)).Inc()

	// Record the outcome on the span so a slow or failing check is visible in the trace
	// (the high-cardinality dimensions banned from metrics live here, RFC-010 section 4.1).
	obs.SpanSetBool(ctx, "healthy", result.Healthy)
	if result.StatusCode != nil {
		obs.SpanSetInt(ctx, "status_code", int64(*result.StatusCode))
	}
	if result.LatencyMs != nil {
		obs.SpanSetInt(ctx, "latency_ms", int64(*result.LatencyMs))
	}
	obs.AddEvent(ctx, "check.executed")

	// Record the terminal per-region outcome (best-effort) so the live element flips
	// this region to done/failed with its latency/status.
	r.markResult(ctx, &m, job.Region, result)

	// Emit first: the durable result row is upserted by the alerting consumer
	// (ADR-0011), so the event is the authoritative output of the worker. A failed
	// emit must redeliver, so it returns an error.
	if err := r.emit(ctx, job, result); err != nil {
		r.metrics.emitFailures.WithLabelValues(job.Region).Inc()
		return err
	}

	// Persist the snapshot when one was captured (the checker only sets it for an
	// entitled org's response-level failure). Non-fatal: it is debug data, not the
	// authoritative record, so a snapshot write failure must not block the pipeline.
	if result.Snapshot != nil {
		if err := r.store.UpsertMonitorLastFailure(ctx, result.OrgID, result.MonitorID, result.Snapshot, result.CheckedAt); err != nil {
			r.log.Warn("persist failure snapshot", "err", err, "monitor", result.MonitorID)
		}
	}

	// Persist the latest cert detail on an ssl check (BACKLOG: SSL-expiry). Same as
	// the snapshot: best-effort, off the authoritative event path.
	if result.CertInfo != nil {
		if err := r.store.UpsertMonitorCert(ctx, result.OrgID, result.MonitorID, result.CertInfo, result.CheckedAt); err != nil {
			r.log.Warn("persist monitor cert", "err", err, "monitor", result.MonitorID)
		}
	}

	// Descriptive, self-contained message (the body reads on its own in stdout and Loki),
	// with the facts also as fields for querying. InfoContext so the OTLP bridge stamps
	// the check's trace_id (RFC-010 section 3). A failed check is a warn with the reason.
	if result.Healthy {
		r.log.InfoContext(ctx, fmt.Sprintf("check ok: monitor %d (%s) from %s -> %d in %dms",
			m.ID, m.URL, job.Region, statusInt(result.StatusCode), latencyMs(result)),
			"monitor", m.ID, "region", job.Region, "status", statusInt(result.StatusCode),
			"latency_ms", latencyMs(result), "url", m.URL)
	} else {
		r.log.WarnContext(ctx, fmt.Sprintf("check failed: monitor %d (%s) from %s -> %s",
			m.ID, m.URL, job.Region, reasonStr(result.FailureReason)),
			"monitor", m.ID, "region", job.Region, "reason", reasonStr(result.FailureReason),
			"status", statusInt(result.StatusCode), "url", m.URL)
	}
	return nil
}

// markRunning flips this monitor's region to "running" in the live state. Best-effort
// and nil-safe: the live element is a convenience view, not the system of record, so it
// never blocks or fails the check.
func (r *Runner) markRunning(ctx context.Context, m *domain.Monitor, region string) {
	if r.state == nil {
		return
	}
	if err := checkstate.SetRunning(ctx, r.state, m.ID, region, m.IntervalSeconds); err != nil {
		r.log.Warn("update check state (running)", "err", err, "monitor", m.ID, "region", region)
	}
}

// markResult flips this monitor's region to its terminal state (done/failed) with the
// outcome. Best-effort and nil-safe (see markRunning).
func (r *Runner) markResult(ctx context.Context, m *domain.Monitor, region string, result *domain.CheckResult) {
	if r.state == nil {
		return
	}
	err := checkstate.SetResult(ctx, r.state, m.ID, region, m.IntervalSeconds, checkstate.Outcome{
		Healthy:       result.Healthy,
		StatusCode:    result.StatusCode,
		LatencyMs:     result.LatencyMs,
		FailureReason: result.FailureReason,
	})
	if err != nil {
		r.log.Warn("update check state (result)", "err", err, "monitor", m.ID, "region", region)
	}
}

func (r *Runner) emit(ctx context.Context, job events.CheckJob, result *domain.CheckResult) error {
	ev := events.CheckResultEvent{JobID: job.JobID, ScheduledAt: job.ScheduledAt, Result: *result}
	payload, err := json.Marshal(ev)
	if err != nil {
		// a result we cannot marshal is poison: log and drop rather than blocking
		r.log.Error("marshal result event", "err", err)
		return nil
	}
	// emit is now the authoritative output (ADR-0011): a failed emit means the result
	// is lost unless we redeliver, so return the error to leave the job unacked.
	return r.prod.Produce(ctx, bus.TopicCheckResults, strconv.FormatInt(result.MonitorID, 10), payload)
}

func reasonStr(r *domain.FailureReason) string {
	if r == nil {
		return ""
	}
	return string(*r)
}

// statusInt unwraps a *int status code for a log attribute, or 0 when there is none
// (connection error / timeout / blocked).
func statusInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// latencyMs unwraps the check latency for a log attribute, or 0 when there is none.
func latencyMs(r *domain.CheckResult) int {
	if r.LatencyMs == nil {
		return 0
	}
	return *r.LatencyMs
}

// reasonLabel is the failure_reason metric label: the reason string, or "none" when the
// check was healthy (RFC-010 section 2.5.3 keeps the null case as a bounded label value).
func reasonLabel(r *domain.FailureReason) string {
	if r == nil {
		return "none"
	}
	return string(*r)
}

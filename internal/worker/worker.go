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
	"log/slog"
	"strconv"
	"time"

	"pulse/internal/bus"
	"pulse/internal/checkstate"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/events"
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
}

// New builds a Runner. state is the per-(monitor, region) live-state store (Redis); it
// may be nil (then the worker just runs checks without updating live state, e.g. in
// tests or a no-Redis dev setup).
func New(store ResultWriter, cons Consumer, prod Producer, chk Checker, ents Entitlements, state checkstate.Store, region string, log *slog.Logger) *Runner {
	return &Runner{store: store, cons: cons, prod: prod, checker: chk, ents: ents, state: state, region: region, log: log}
}

// Run consumes jobs until the context is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info("worker started", "region", r.region)
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

func (r *Runner) handle(ctx context.Context, rec bus.Record) error {
	var job events.CheckJob
	if err := json.Unmarshal(rec.Value, &job); err != nil {
		// a malformed job is poison: log and drop rather than blocking the partition
		r.log.Error("bad job payload, dropping", "err", err)
		return nil
	}

	m := job.Monitor
	// Mark this region in-flight so the monitor's live element shows it "pinging" while
	// the check runs (best-effort; never blocks the check). Every check drives this, not
	// just manual check-now.
	r.markRunning(ctx, &m, job.Region)

	// Decide capture up front from the org's plan: an ungated org never has its
	// response read for a snapshot, so it pays no hot-path cost (PRD-002 3.8, RFC-009).
	capture := r.ents.For(job.OrgID).FailureSnapshot
	result := r.checker.Check(ctx, &m, capture)
	result.OrgID = job.OrgID
	result.Region = job.Region

	// Record the terminal per-region outcome (best-effort) so the live element flips
	// this region to done/failed with its latency/status.
	r.markResult(ctx, &m, job.Region, result)

	// Emit first: the durable result row is upserted by the alerting consumer
	// (ADR-0011), so the event is the authoritative output of the worker. A failed
	// emit must redeliver, so it returns an error.
	if err := r.emit(ctx, job, result); err != nil {
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

	r.log.Info("check done",
		"monitor", result.MonitorID, "region", result.Region,
		"healthy", result.Healthy, "reason", reasonStr(result.FailureReason))
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

// This file holds the distributed wrapper around the pure Apply state machine
// (RFC-006): the consume loop that turns check.results into incidents and
// notify.events. Apply itself (alerting.go) is reused unchanged; everything that
// touches Postgres or Kafka lives here.
//
// Single-region note (RFC-006 sections 3, 3.6): the multi-region windowed verdict
// (down_policy/quorum over a per-(monitor, scheduled_at) window in Redis) is a
// later feature (RFC-006 feature 6). Today the one check result IS the verdict, so
// the reduce step is a no-op that just hands the result straight to Apply. The
// reduce seam is reduceToVerdict below: when the window lands it replaces that
// function and feeds Apply a synthetic reduced result with the round's max
// result_id as the watermark. The persistence and emit path downstream of it do
// not change, which is why the seam is isolated to one function.
package alerting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/bus"
	"pulse/internal/domain"
	"pulse/internal/events"
	"pulse/internal/store"
)

// Store is the Postgres surface the alerting service needs. It is the subset of
// *store.Pool the wrapper uses, so tests can fake it.
type Store interface {
	// UpsertCheckResult persists the result idempotently and returns its row id
	// (the watermark anchor). Alerting owns this write (ADR-0011): the worker emits
	// the event only, so the durable row and the alerting trigger share one path.
	UpsertCheckResult(ctx context.Context, r *domain.CheckResult) (int64, error)
	// GetMonitor loads the monitor config (failure_threshold, down_policy, channels).
	GetMonitor(ctx context.Context, orgID, id int64) (*domain.Monitor, error)
	// GetAlertState loads the counters, watermark, and open incident.
	GetAlertState(ctx context.Context, orgID, monitorID int64) (*domain.AlertState, error)
	// ApplyAlertDecision persists the decision idempotently in one transaction.
	ApplyAlertDecision(ctx context.Context, m *domain.Monitor, maxResultID, firstResultID int64, d store.Decision) (store.AppliedDecision, error)
}

// Consumer is the subset of the bus consumer the service needs.
type Consumer interface {
	Poll(ctx context.Context, handler func(bus.Record) error) error
}

// Producer emits notify.events.
type Producer interface {
	Produce(ctx context.Context, topic, key string, value []byte) error
}

// Runner ties the check.results consume loop to the state machine and the writes.
type Runner struct {
	store  Store
	cons   Consumer
	prod   Producer
	engine *Engine
	log    *slog.Logger
}

// NewRunner builds a Runner. The engine is the reused pure state machine (New).
func NewRunner(st Store, cons Consumer, prod Producer, log *slog.Logger) *Runner {
	return &Runner{store: st, cons: cons, prod: prod, engine: New(), log: log}
}

// Run consumes check.results until the context is cancelled. Commit-after-process:
// the handler returns an error to leave the offset uncommitted for redelivery, so
// a Postgres blip retries cleanly (RFC-006 section 10). Mirrors worker.Runner.
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info("alerting started")
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := r.cons.Poll(ctx, func(rec bus.Record) error {
			return r.handle(ctx, rec)
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

// handle processes one check.results record: persist the row, reduce to a verdict,
// run Apply, persist the decision in one transaction, then emit the notify event.
func (r *Runner) handle(ctx context.Context, rec bus.Record) error {
	var ev events.CheckResultEvent
	if err := json.Unmarshal(rec.Value, &ev); err != nil {
		// a malformed result is poison: log and drop rather than blocking the partition
		r.log.Error("bad result payload, dropping", "err", err)
		return nil
	}
	res := ev.Result
	// ScheduledAt rides the event, not the result struct the worker fills, so copy it
	// onto the row here. It is the tick key the ui groups a run's regions by.
	res.ScheduledAt = ev.ScheduledAt

	// 1. Persist the durable row and read back its id (the watermark anchor).
	// Alerting owns this write (ADR-0011). Idempotent on (org, monitor, region,
	// checked_at), so a redelivery returns the same id and no duplicate row.
	resultID, err := r.store.UpsertCheckResult(ctx, &res)
	if err != nil {
		return err // leave the offset uncommitted; redeliver
	}

	// 2. Load the monitor config. A monitor deleted between check and apply is gone;
	// drop the orphan result (the row already cascaded away, history is consistent).
	m, err := r.store.GetMonitor(ctx, res.OrgID, res.MonitorID)
	if err != nil {
		if err == pgx.ErrNoRows {
			r.log.Warn("monitor gone, dropping result", "monitor", res.MonitorID)
			return nil
		}
		return err
	}

	// 3. Reduce per-region results to one verdict. SINGLE-REGION SEAM: today the one
	// result is the verdict; multi-region windowing (RFC-006 section 3) slots in here.
	verdict, maxResultID := r.reduceToVerdict(&res, resultID)

	// 4. Load alert state, run the PURE state machine.
	state, err := r.store.GetAlertState(ctx, m.OrgID, m.ID)
	if err != nil {
		if err == pgx.ErrNoRows {
			r.log.Warn("monitor gone, dropping result", "monitor", res.MonitorID)
			return nil
		}
		return err
	}
	decision := r.engine.Apply(m, verdict, state)

	// 5. Persist the decision idempotently in one transaction (open/close incident,
	// counters, watermark). firstResultID links an opened incident to the row.
	applied, err := r.store.ApplyAlertDecision(ctx, m, maxResultID, resultID, toStoreDecision(decision))
	if err != nil {
		return err // leave the offset uncommitted; redeliver
	}
	if applied.Skipped {
		// the watermark dropped a redelivered or older round: a clean no-op
		r.log.Debug("result already applied, skipped", "monitor", m.ID, "result_id", resultID)
		return nil
	}

	// 6. Emit the notify event after commit, only when the incident action actually
	// happened. The dedup id makes the re-emit on redelivery a no-op for the notifier.
	if decision.Notify != nil && applied.Applied && applied.Incident != nil {
		r.emit(ctx, m, applied.Incident, verdict, decision.Notify.Type)
	}

	r.log.Info("result applied",
		"monitor", m.ID, "healthy", verdict.Healthy,
		"action", decision.Action, "applied", applied.Applied)
	return nil
}

// reduceToVerdict collapses the per-region results of a check round into one
// healthy/unhealthy verdict and the watermark (the round's max result id).
//
// SINGLE-REGION SEAM (RFC-006 section 3.6): with one region the one result IS the
// verdict and its id IS the round max, so this is the identity. When the
// multi-region window lands (RFC-006 feature 6 / RFC-008), this is where the
// Redis-buffered round closes, the down_policy reduces the buffered results to one
// synthetic domain.CheckResult, and maxResultID becomes the largest id across the
// round. Everything downstream (Apply, ApplyAlertDecision, emit) is unchanged,
// which is the point of isolating the reduce here.
func (r *Runner) reduceToVerdict(res *domain.CheckResult, resultID int64) (*domain.CheckResult, int64) {
	return res, resultID
}

// emit builds and produces the notify.events message after the incident is
// persisted (the incident id and timestamps are only known then). emit failure is
// non-fatal: the incident is committed and a later redelivery re-emits with the same
// dedup id, so the notification is not lost (RFC-006 section 5.4).
func (r *Runner) emit(ctx context.Context, m *domain.Monitor, inc *domain.Incident, verdict *domain.CheckResult, kind NotifyEventType) {
	ev := events.NotifyEvent{
		OrgID:             m.OrgID,
		MonitorID:         m.ID,
		IncidentID:        inc.ID,
		EventType:         string(kind),
		DedupKey:          dedupKey(inc.ID, string(kind)),
		MonitorName:       m.Name,
		MonitorURL:        m.URL,
		MonitorMethod:     string(m.Method),
		IncidentStartedAt: inc.StartedAt,
		IncidentEndedAt:   inc.EndedAt,
		Check:             *verdict,
		ChannelIDs:        m.ChannelIDs,
		SentAt:            time.Now().UTC(),
	}
	if kind == EventRecovery && inc.EndedAt != nil {
		secs := int(inc.EndedAt.Sub(inc.StartedAt).Seconds())
		ev.DurationSeconds = &secs
	}
	if kind == EventDown && verdict.Region != "" {
		// single-region: the one failing region is the observed-unhealthy set
		ev.RegionsObserved = []string{verdict.Region}
	}

	payload, err := json.Marshal(ev)
	if err != nil {
		r.log.Error("marshal notify event", "err", err)
		return
	}
	if err := r.prod.Produce(ctx, bus.TopicNotifyEvents, strconv.FormatInt(m.ID, 10), payload); err != nil {
		r.log.Warn("emit notify event", "err", err, "monitor", m.ID)
	}
}

// dedupKey is hex(sha256(incident_id ":" event_type)), stable per (incident, kind)
// so a redelivery-driven re-emit carries the same key and the notifier suppresses it
// (RFC-006 section 7.1). An incident has exactly one down and one recovery, so at
// most two distinct keys ever.
func dedupKey(incidentID int64, eventType string) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(incidentID, 10) + ":" + eventType))
	return hex.EncodeToString(sum[:])
}

// toStoreDecision maps the pure alerting.Decision onto the store's persisted shape,
// so the store stays free of the alerting import.
func toStoreDecision(d Decision) store.Decision {
	out := store.Decision{
		NewConsecutive:    d.NewConsecutive,
		NewFirstFailAt:    d.NewFirstFailAt,
		IncidentStartedAt: d.IncidentStartedAt,
		CauseReason:       d.CauseReason,
		IncidentEndedAt:   d.IncidentEndedAt,
	}
	switch d.Action {
	case ActionOpenIncident:
		out.Action = store.IncidentOpen
	case ActionCloseIncident:
		out.Action = store.IncidentClose
	default:
		out.Action = store.IncidentNone
	}
	return out
}

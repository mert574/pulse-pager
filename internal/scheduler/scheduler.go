// Package scheduler decides which checks are due and fans out one job per
// (monitor, region) onto Kafka, keyed by monitor id for per-monitor ordering
// (RFC-004). This is the slice-level core: an in-memory next-run map rebuilt
// from Postgres, advanced on a ticker. Leader election, the entitlement clamp,
// and live monitor.changed updates are later work (marked TODO).
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"pulse/internal/bus"
	"pulse/internal/checkstate"
	"pulse/internal/domain"
	"pulse/internal/events"
	"pulse/internal/obs"
	"pulse/internal/region"
	"pulse/internal/store"
)

// Producer is the subset of the bus producer the scheduler needs.
type Producer interface {
	Produce(ctx context.Context, topic, key string, value []byte) error
}

// MonitorLister is the subset of the store the scheduler reads. It returns each
// enabled monitor with its last check time so the scheduler can seed its next-run
// from persisted state after a restart.
type MonitorLister interface {
	ListEnabledMonitorsWithLastCheck(ctx context.Context) ([]store.EnabledMonitor, error)
}

// Dispatcher fans out due checks.
type Dispatcher struct {
	store   MonitorLister
	prod    Producer
	state   checkstate.Store // live per-(monitor,region) state; nil = skip (dev/no-Redis)
	log     *slog.Logger
	tick    time.Duration
	metrics *metrics

	mu      sync.Mutex
	nextRun map[int64]time.Time // monitor id -> next dispatch time
}

// New builds a Dispatcher. tick is how often it scans for due monitors. state is the
// live per-(monitor,region) state store (Redis); it may be nil (dev/no-Redis), in
// which case the scheduler dispatches normally but writes no live state. reg is the
// Prometheus registry the SLI metrics register on; nil leaves them unregistered (tests).
func New(store MonitorLister, prod Producer, state checkstate.Store, log *slog.Logger, tick time.Duration, reg *prometheus.Registry) *Dispatcher {
	if tick <= 0 {
		tick = time.Second
	}
	return &Dispatcher{store: store, prod: prod, state: state, log: log, tick: tick, metrics: newMetrics(reg), nextRun: map[int64]time.Time{}}
}

// metrics holds the scheduler's SLI metrics (RFC-010 section 2.5.2).
type metrics struct {
	dispatchLag    *prometheus.HistogramVec
	jobsDispatched *prometheus.CounterVec
	scheduleSize   prometheus.Gauge
}

// newMetrics builds the metrics and registers them on reg. A nil reg builds them
// unregistered, so recording never panics in a test that passes no registry.
func newMetrics(reg *prometheus.Registry) *metrics {
	m := &metrics{
		dispatchLag: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pulse_schedule_dispatch_lag_seconds",
			Help:    "Dispatch lag (dispatched_at - scheduled_at) in seconds, by region.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10},
		}, []string{"region"}),
		jobsDispatched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pulse_schedule_jobs_dispatched_total",
			Help: "Check jobs published, by region.",
		}, []string{"region"}),
		scheduleSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "pulse_schedule_size",
			Help: "Enabled monitors the scheduler is tracking.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.dispatchLag, m.jobsDispatched, m.scheduleSize)
	}
	return m
}

// Run scans on each tick until the context is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	t := time.NewTicker(d.tick)
	defer t.Stop()
	d.log.Info("scheduler started", "tick", d.tick.String())
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			d.dispatchDue(ctx, now.UTC())
		}
	}
}

func (d *Dispatcher) dispatchDue(ctx context.Context, now time.Time) {
	monitors, err := d.store.ListEnabledMonitorsWithLastCheck(ctx)
	if err != nil {
		d.log.Error("list monitors", "err", err)
		return
	}
	d.metrics.scheduleSize.Set(float64(len(monitors)))
	for _, em := range monitors {
		m := em.Monitor
		interval := time.Duration(m.IntervalSeconds) * time.Second
		if interval <= 0 {
			interval = time.Minute
		}

		d.mu.Lock()
		next, seen := d.nextRun[m.ID]
		d.mu.Unlock()

		if !seen {
			// First sight of this monitor in this process: either a fresh start (the
			// service is killed often on CD, so the in-memory map is empty) or a newly
			// created monitor. Seed the next-run from the persisted last check so a
			// restart resumes the real schedule instead of dispatching everything at
			// once. A monitor that has never been checked, or is already overdue, is
			// due now and falls through to dispatch.
			if em.LastCheckedAt != nil {
				due := em.LastCheckedAt.Add(interval)
				if now.Before(due) {
					d.mu.Lock()
					d.nextRun[m.ID] = due
					d.mu.Unlock()
					continue
				}
			}
		} else if now.Before(next) {
			continue // not due yet
		}

		// scheduledFor is when this round was due, so the dispatch-lag SLI measures how
		// late we published vs the schedule (RFC-010 section 2.5.2): the seeded next-run
		// for a seen monitor, the last-check + interval for an overdue one we just saw,
		// and now (lag ~0) for a monitor that has never been checked.
		scheduledFor := now
		if seen {
			scheduledFor = next
		} else if em.LastCheckedAt != nil {
			scheduledFor = em.LastCheckedAt.Add(interval)
		}

		d.dispatch(ctx, m, now, scheduledFor)
		d.mu.Lock()
		d.nextRun[m.ID] = now.Add(interval)
		d.mu.Unlock()
	}
}

func (d *Dispatcher) dispatch(ctx context.Context, m *domain.Monitor, dispatchedAt, scheduledFor time.Time) {
	// Root of the automated check trace (RFC-010 section 4.1): a scheduled dispatch has
	// no FE request behind it, so the scheduler starts the trace here. Each region job
	// produced below carries this span's context over the bus, so every region's worker
	// continues the same trace and a multi-region round reads as one tree.
	ctx, end := obs.StartSpan(ctx, "schedule.dispatch")
	defer end()
	obs.SpanSetInt(ctx, "monitor_id", m.ID)
	obs.SpanSetInt(ctx, "org_id", m.OrgID)

	// How late this round published vs when it was due (never negative). Recorded per
	// region below since the metric is region-labeled.
	lag := dispatchedAt.Sub(scheduledFor).Seconds()
	if lag < 0 {
		lag = 0
	}

	regions := m.Regions
	if len(regions) == 0 {
		regions = []string{region.Default}
	}
	// A TLS certificate is identical from every region, so an ssl monitor is always
	// checked from a single region (BACKLOG: SSL-expiry). The api already stores one
	// region for ssl; this is the authoritative guard so even a stray multi-region
	// row never fans out into a wasted, confusing multi-region cert check.
	if m.Type == domain.MonitorSSL && len(regions) > 1 {
		regions = regions[:1]
	}
	key := strconv.FormatInt(m.ID, 10)
	for _, region := range regions {
		job := events.CheckJob{
			JobID:       fmt.Sprintf("%d:%s:%d", m.ID, region, dispatchedAt.Unix()),
			OrgID:       m.OrgID,
			Region:      region,
			ScheduledAt: dispatchedAt,
			Monitor:     *m,
		}
		payload, err := json.Marshal(job)
		if err != nil {
			d.log.Error("marshal job", "err", err, "monitor", m.ID)
			continue
		}
		if err := d.prod.Produce(ctx, bus.CheckJobsTopic(region), key, payload); err != nil {
			d.log.Error("dispatch job", "err", err, "monitor", m.ID, "region", region)
			continue
		}
		obs.ObserveWithTrace(ctx, d.metrics.dispatchLag.WithLabelValues(region), lag)
		d.metrics.jobsDispatched.WithLabelValues(region).Inc()
		// Mark the region queued so the monitor's live element shows it "scheduled"
		// until the worker picks it up. Best-effort; never blocks dispatch.
		if d.state != nil {
			if err := checkstate.SetScheduled(ctx, d.state, m.ID, region, m.IntervalSeconds); err != nil {
				d.log.Warn("mark scheduled", "err", err, "monitor", m.ID, "region", region)
			}
		}
	}
}

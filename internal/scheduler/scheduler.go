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

	"pulse/internal/bus"
	"pulse/internal/checkstate"
	"pulse/internal/domain"
	"pulse/internal/events"
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
	store MonitorLister
	prod  Producer
	state checkstate.Store // live per-(monitor,region) state; nil = skip (dev/no-Redis)
	log   *slog.Logger
	tick  time.Duration

	mu      sync.Mutex
	nextRun map[int64]time.Time // monitor id -> next dispatch time
}

// New builds a Dispatcher. tick is how often it scans for due monitors. state is the
// live per-(monitor,region) state store (Redis); it may be nil (dev/no-Redis), in
// which case the scheduler dispatches normally but writes no live state.
func New(store MonitorLister, prod Producer, state checkstate.Store, log *slog.Logger, tick time.Duration) *Dispatcher {
	if tick <= 0 {
		tick = time.Second
	}
	return &Dispatcher{store: store, prod: prod, state: state, log: log, tick: tick, nextRun: map[int64]time.Time{}}
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

		d.dispatch(ctx, m, now)
		d.mu.Lock()
		d.nextRun[m.ID] = now.Add(interval)
		d.mu.Unlock()
	}
}

func (d *Dispatcher) dispatch(ctx context.Context, m *domain.Monitor, scheduledAt time.Time) {
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
			JobID:       fmt.Sprintf("%d:%s:%d", m.ID, region, scheduledAt.Unix()),
			OrgID:       m.OrgID,
			Region:      region,
			ScheduledAt: scheduledAt,
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
		// Mark the region queued so the monitor's live element shows it "scheduled"
		// until the worker picks it up. Best-effort; never blocks dispatch.
		if d.state != nil {
			if err := checkstate.SetScheduled(ctx, d.state, m.ID, region, m.IntervalSeconds); err != nil {
				d.log.Warn("mark scheduled", "err", err, "monitor", m.ID, "region", region)
			}
		}
	}
}

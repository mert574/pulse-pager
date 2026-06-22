package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"pulse/internal/domain"
	"pulse/internal/store"
)

// fakeLister returns a fixed set of enabled monitors with their last-check times.
type fakeLister struct{ items []store.EnabledMonitor }

func (f *fakeLister) ListEnabledMonitorsWithLastCheck(context.Context) ([]store.EnabledMonitor, error) {
	return f.items, nil
}

// fakeProducer records the monitor keys it was asked to dispatch.
type fakeProducer struct {
	mu   sync.Mutex
	keys []string
}

func (p *fakeProducer) Produce(_ context.Context, _ string, key string, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = append(p.keys, key)
	return nil
}

func (p *fakeProducer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keys)
}

// fakeStateStore records the per-region HSet writes the scheduler makes.
type fakeStateStore struct {
	writes []struct{ key, field string }
}

func (f *fakeStateStore) HSet(_ context.Context, key, field, _ string) error {
	f.writes = append(f.writes, struct{ key, field string }{key, field})
	return nil
}
func (f *fakeStateStore) HGetAll(context.Context, string) (map[string]string, error) {
	return nil, nil
}
func (f *fakeStateStore) Expire(context.Context, string, time.Duration) error { return nil }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mon(id int64, intervalSeconds int) *domain.Monitor {
	return &domain.Monitor{ID: id, OrgID: 1, IntervalSeconds: intervalSeconds, Regions: []string{"eu-central"}}
}

// A fresh Dispatcher models a process that was just (re)started: its in-memory
// nextRun map is empty. On first sight it must seed next-run from the persisted last
// check instead of dispatching everything, so a restart does not cause a check storm
// and resumes the real schedule. A never-checked or overdue monitor is due now.
func TestDispatcherSeedsFromPersistedLastCheck(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { tm := now.Add(-d); return &tm }

	cases := []struct {
		name         string
		lastChecked  *time.Time
		interval     int
		wantDispatch bool
	}{
		{"never checked dispatches now", nil, 60, true},
		{"recently checked is not re-dispatched after restart", ago(10 * time.Second), 60, false},
		{"overdue dispatches now", ago(120 * time.Second), 60, true},
		{"checked exactly interval ago is due", ago(60 * time.Second), 60, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prod := &fakeProducer{}
			d := New(&fakeLister{items: []store.EnabledMonitor{
				{Monitor: mon(1, tc.interval), LastCheckedAt: tc.lastChecked},
			}}, prod, nil, discardLog(), time.Second)

			d.dispatchDue(context.Background(), now) // first tick after a "restart"

			got := prod.count() > 0
			if got != tc.wantDispatch {
				t.Fatalf("dispatch on first sight = %v, want %v", got, tc.wantDispatch)
			}
		})
	}
}

// Once a monitor has been dispatched, it must not be dispatched again until its
// interval has elapsed (the in-memory nextRun guards the dispatch->persist window).
func TestDispatcherHoldsIntervalAfterDispatch(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	prod := &fakeProducer{}
	d := New(&fakeLister{items: []store.EnabledMonitor{
		{Monitor: mon(1, 60), LastCheckedAt: nil}, // never checked -> dispatches now
	}}, prod, nil, discardLog(), time.Second)

	d.dispatchDue(context.Background(), now)
	if prod.count() != 1 {
		t.Fatalf("first tick: want 1 dispatch, got %d", prod.count())
	}
	// 10s later, well within the 60s interval: no new dispatch.
	d.dispatchDue(context.Background(), now.Add(10*time.Second))
	if prod.count() != 1 {
		t.Fatalf("within interval: want still 1 dispatch, got %d", prod.count())
	}
	// past the interval: dispatches again.
	d.dispatchDue(context.Background(), now.Add(61*time.Second))
	if prod.count() != 2 {
		t.Fatalf("after interval: want 2 dispatches, got %d", prod.count())
	}
}

// On dispatch the scheduler marks each of the monitor's regions "scheduled" in the
// live state, so the monitor's element shows the queued regions before the worker
// picks them up. A nil state store is a no-op.
func TestDispatcherMarksRegionsScheduled(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m := &domain.Monitor{ID: 9, OrgID: 1, IntervalSeconds: 60, Regions: []string{"us-west", "us-east"}}
	state := &fakeStateStore{}
	d := New(&fakeLister{items: []store.EnabledMonitor{{Monitor: m, LastCheckedAt: nil}}},
		&fakeProducer{}, state, discardLog(), time.Second)

	d.dispatchDue(context.Background(), now)

	got := map[string]bool{}
	for _, w := range state.writes {
		if w.key == "checkstate:9" {
			got[w.field] = true
		}
	}
	if !got["us-west"] || !got["us-east"] {
		t.Fatalf("want both regions marked scheduled, got %v", got)
	}
}

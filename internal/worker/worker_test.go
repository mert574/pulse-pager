package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"pulse/internal/bus"
	"pulse/internal/checkstate"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/events"
)

// countingStore records calls so the entitlement gate's effect is observable. The
// worker no longer writes the result row (ADR-0011: alerting owns that upsert), so
// the only store call left is the last-failure snapshot.
type countingStore struct {
	snapshots int
	certs     int
}

func (s *countingStore) UpsertMonitorLastFailure(context.Context, int64, int64, *domain.ResponseSnapshot, time.Time) error {
	s.snapshots++
	return nil
}

func (s *countingStore) UpsertMonitorCert(context.Context, int64, int64, *domain.CertInfo, time.Time) error {
	s.certs++
	return nil
}

type fakeChecker struct{ result *domain.CheckResult }

// Check mirrors the real checker: it only populates the snapshot when capture is on.
func (f fakeChecker) Check(_ context.Context, _ *domain.Monitor, captureResponse bool) *domain.CheckResult {
	res := *f.result
	if !captureResponse {
		res.Snapshot = nil
	}
	return &res
}

type fakeProducer struct{ emitted int }

func (f *fakeProducer) Produce(context.Context, string, string, []byte) error {
	f.emitted++
	return nil
}

// gate is a Resolver whose FailureSnapshot value is fixed.
type gate struct{ on bool }

func (g gate) For(int64) entitlements.Set { return entitlements.Set{FailureSnapshot: g.on} }

func failingResult() *domain.CheckResult {
	reason := domain.ReasonStatusMismatch
	code := 500
	return &domain.CheckResult{
		MonitorID:     7,
		Healthy:       false,
		FailureReason: &reason,
		StatusCode:    &code,
		Snapshot:      &domain.ResponseSnapshot{StatusCode: &code, Body: "boom"},
	}
}

func jobRecord(t *testing.T) bus.Record {
	t.Helper()
	payload, err := json.Marshal(events.CheckJob{OrgID: 1, Region: "eu-central", Monitor: domain.Monitor{ID: 7}})
	if err != nil {
		t.Fatal(err)
	}
	return bus.Record{Topic: bus.CheckJobsTopic("eu-central"), Key: "7", Value: payload}
}

func newRunner(st ResultWriter, prod Producer, on bool) *Runner {
	return New(st, nil, prod, fakeChecker{result: failingResult()}, gate{on: on}, nil, "eu-central",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// fakeStateStore records the per-region HSet writes so a test can see the live-state
// transitions the worker makes.
type fakeStateStore struct {
	writes []struct{ key, field, value string }
}

func (f *fakeStateStore) HSet(_ context.Context, key, field, value string) error {
	f.writes = append(f.writes, struct{ key, field, value string }{key, field, value})
	return nil
}
func (f *fakeStateStore) HGetAll(context.Context, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (f *fakeStateStore) Expire(context.Context, string, time.Duration) error { return nil }

func TestHandle_PersistsSnapshotWhenEntitled(t *testing.T) {
	st := &countingStore{}
	prod := &fakeProducer{}
	if err := newRunner(st, prod, true).handle(context.Background(), jobRecord(t)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if prod.emitted != 1 {
		t.Errorf("check.results emits = %d, want 1 (the worker emits the result)", prod.emitted)
	}
	if st.snapshots != 1 {
		t.Errorf("snapshot writes = %d, want 1 when entitled", st.snapshots)
	}
}

func TestHandle_SkipsSnapshotWhenNotEntitled(t *testing.T) {
	st := &countingStore{}
	prod := &fakeProducer{}
	if err := newRunner(st, prod, false).handle(context.Background(), jobRecord(t)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if prod.emitted != 1 {
		t.Errorf("check.results emits = %d, want 1 (result always emitted)", prod.emitted)
	}
	if st.snapshots != 0 {
		t.Errorf("snapshot writes = %d, want 0 when not entitled", st.snapshots)
	}
}

// Every check (scheduled too, since RunID no longer exists) records the per-region
// live-state transitions for its monitor: running while the check runs, then the
// terminal state (failed here, the check is unhealthy). A nil store writes nothing.
func TestHandle_UpdatesLiveStateForEveryCheck(t *testing.T) {
	state := &fakeStateStore{}
	r := New(&countingStore{}, nil, &fakeProducer{}, fakeChecker{result: failingResult()},
		gate{on: false}, state, "eu-central", slog.New(slog.NewTextHandler(io.Discard, nil)))

	// jobRecord builds a plain (scheduled) job for monitor 7, region home.
	if err := r.handle(context.Background(), jobRecord(t)); err != nil {
		t.Fatalf("handle: %v", err)
	}

	// Expect two writes to the monitor's hash for region "eu-central": running then terminal.
	var states []string
	for _, w := range state.writes {
		if w.key == checkstate.Key(7) && w.field == "eu-central" {
			states = append(states, w.value)
		}
	}
	if len(states) != 2 {
		t.Fatalf("live-state writes for home = %d, want 2 (running, terminal): %v", len(states), states)
	}
	if !strings.Contains(states[0], checkstate.StateRunning) {
		t.Errorf("first live-state write should be running, got %q", states[0])
	}
	if !strings.Contains(states[1], checkstate.StateFailed) {
		t.Errorf("terminal live-state should be failed (unhealthy), got %q", states[1])
	}

	// A nil store is a no-op (no panic, nothing written).
	r2 := New(&countingStore{}, nil, &fakeProducer{}, fakeChecker{result: failingResult()},
		gate{on: false}, nil, "eu-central", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := r2.handle(context.Background(), jobRecord(t)); err != nil {
		t.Fatalf("handle nil-store: %v", err)
	}
}

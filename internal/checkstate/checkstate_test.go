package checkstate

import (
	"context"
	"testing"
	"time"

	"pulse/internal/domain"
)

// memStore is a map-backed Store for testing the round-trip and rollup.
type memStore struct {
	hashes map[string]map[string]string
}

func newMemStore() *memStore { return &memStore{hashes: map[string]map[string]string{}} }

func (m *memStore) HSet(_ context.Context, key, field, value string) error {
	if m.hashes[key] == nil {
		m.hashes[key] = map[string]string{}
	}
	m.hashes[key][field] = value
	return nil
}
func (m *memStore) HGetAll(_ context.Context, key string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m.hashes[key] {
		out[k] = v
	}
	return out, nil
}
func (m *memStore) Expire(context.Context, string, time.Duration) error { return nil }

func TestSetAndGetRoundTrip(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	const mon = int64(7)

	if err := SetScheduled(ctx, s, mon, "us-west", 60); err != nil {
		t.Fatal(err)
	}
	if err := SetRunning(ctx, s, mon, "us-east", 60); err != nil {
		t.Fatal(err)
	}
	code := 200
	lat := 42
	if err := SetResult(ctx, s, mon, "sa-east", 60, Outcome{Healthy: true, StatusCode: &code, LatencyMs: &lat}); err != nil {
		t.Fatal(err)
	}

	got, found, err := Get(ctx, s, mon)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if got["us-west"].State != StateScheduled {
		t.Errorf("us-west = %q, want scheduled", got["us-west"].State)
	}
	if got["us-east"].State != StateRunning {
		t.Errorf("us-east = %q, want running", got["us-east"].State)
	}
	if got["sa-east"].State != StateDone || got["sa-east"].LatencyMs == nil || *got["sa-east"].LatencyMs != 42 {
		t.Errorf("sa-east = %+v, want done with latency 42", got["sa-east"])
	}

	// An unknown monitor has no state.
	if _, found, _ := Get(ctx, s, 999); found {
		t.Error("unknown monitor should not be found")
	}
}

func TestSetResultFailedWhenUnhealthy(t *testing.T) {
	s := newMemStore()
	reason := domain.ReasonStatusMismatch
	if err := SetResult(context.Background(), s, 1, "eu-central", 60, Outcome{Healthy: false, FailureReason: &reason}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Get(context.Background(), s, 1)
	if got["eu-central"].State != StateFailed {
		t.Errorf("unhealthy result state = %q, want failed", got["eu-central"].State)
	}
}

func TestRollup(t *testing.T) {
	mk := func(states ...string) map[string]RegionState {
		m := map[string]RegionState{}
		for i, st := range states {
			rs := RegionState{State: st}
			if st == StateDone {
				h := true
				rs.Healthy = &h
			} else if st == StateFailed {
				h := false
				rs.Healthy = &h
			}
			m[string(rune('a'+i))] = rs
		}
		return m
	}

	cases := []struct {
		name        string
		regions     map[string]RegionState
		policy      domain.DownPolicy
		wantPhase   Phase
		wantHealthy *bool
	}{
		{"all scheduled is pending", mk(StateScheduled, StateScheduled), domain.DownPolicyAny, PhasePending, nil},
		{"one running is running", mk(StateDone, StateRunning), domain.DownPolicyAny, PhaseRunning, nil},
		{"any: one failed is down", mk(StateDone, StateFailed), domain.DownPolicyAny, PhaseComplete, boolp(false)},
		{"all: one healthy is up", mk(StateDone, StateFailed), domain.DownPolicyAll, PhaseComplete, boolp(true)},
		{"quorum: majority down is down", mk(StateFailed, StateFailed, StateDone), domain.DownPolicyQuorum, PhaseComplete, boolp(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			phase, healthy := Rollup(tc.regions, tc.policy)
			if phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", phase, tc.wantPhase)
			}
			if (healthy == nil) != (tc.wantHealthy == nil) || (healthy != nil && *healthy != *tc.wantHealthy) {
				t.Errorf("healthy = %v, want %v", healthy, tc.wantHealthy)
			}
		})
	}
}

func TestTTLFor(t *testing.T) {
	if got := TTLFor(60); got != time.Hour {
		t.Errorf("TTLFor(60) = %v, want 1h floor", got)
	}
	if got := TTLFor(7200); got != 3*7200*time.Second {
		t.Errorf("TTLFor(7200) = %v, want interval*3", got)
	}
}

func boolp(b bool) *bool { return &b }

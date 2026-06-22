// Package checkstate is the continuous, per-(monitor, region) live state of checks.
// Every check, scheduled or manual, drives it: the scheduler marks a region
// "scheduled" when it dispatches, the worker marks it "running" then "done"/"failed"
// with the outcome. The frontend reads it to show one element per monitor with a chip
// per region (scheduled -> pinging -> done/failed), page-wide. Check-now is not
// special: it is just a manual trigger of the same machine.
//
// It lives in Redis: one hash per monitor, one field per region. It is a live view,
// not the system of record. Authoritative results still go worker -> check.results ->
// alerting -> Postgres (ADR-0011); this is a parallel, refreshed-each-cycle projection
// with a TTL sized to outlive the gap between checks.
//
// It is dependency-light (only domain) so api, worker, and scheduler can all import it
// without a cycle, like internal/events.
package checkstate

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"pulse/internal/domain"
)

// minTTL floors the per-monitor state TTL so a fast monitor's chips never blank out in
// a brief gap. The effective TTL is max(interval*3, minTTL): interval*3 survives a
// missed cycle, and each write refreshes it, so an actively-checked monitor never
// expires.
const minTTL = time.Hour

// The per-region lifecycle. scheduled -> running -> (done | failed). done means the
// check completed healthy; failed means it completed unhealthy or could not run. The
// frontend colors a chip by state.
const (
	StateScheduled = "scheduled"
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
)

// Store is the Redis subset this package needs. *kv.Client satisfies it; a test can
// pass a map-backed fake.
type Store interface {
	HSet(ctx context.Context, key, field, value string) error
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	Expire(ctx context.Context, key string, ttl time.Duration) error
}

// RegionState is one region's slot for a monitor: its state plus, once terminal, the
// outcome bits the frontend shows on the chip.
type RegionState struct {
	State         string                `json:"state"`
	Healthy       *bool                 `json:"healthy,omitempty"`
	StatusCode    *int                  `json:"status_code,omitempty"`
	LatencyMs     *int                  `json:"latency_ms,omitempty"`
	FailureReason *domain.FailureReason `json:"failure_reason,omitempty"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

// Outcome is the terminal result of one region's check, passed by the worker.
type Outcome struct {
	Healthy       bool
	StatusCode    *int
	LatencyMs     *int
	FailureReason *domain.FailureReason
}

// Key is the per-monitor hash holding region -> RegionState.
func Key(monitorID int64) string { return "checkstate:" + strconv.FormatInt(monitorID, 10) }

// TTLFor sizes the state TTL from the check interval: long enough to outlive the gap
// between checks (interval*3), with an hour floor for fast monitors.
func TTLFor(intervalSeconds int) time.Duration {
	ttl := time.Duration(intervalSeconds) * 3 * time.Second
	if ttl < minTTL {
		return minTTL
	}
	return ttl
}

// SetScheduled marks a region queued (scheduler/api, at dispatch/enqueue).
func SetScheduled(ctx context.Context, s Store, monitorID int64, region string, intervalSeconds int) error {
	return writeRegion(ctx, s, monitorID, region, intervalSeconds, RegionState{State: StateScheduled})
}

// SetRunning marks a region in-flight (worker, on pickup).
func SetRunning(ctx context.Context, s Store, monitorID int64, region string, intervalSeconds int) error {
	return writeRegion(ctx, s, monitorID, region, intervalSeconds, RegionState{State: StateRunning})
}

// SetResult marks a region terminal with its outcome (worker, on finish). The state is
// done when healthy, failed otherwise.
func SetResult(ctx context.Context, s Store, monitorID int64, region string, intervalSeconds int, o Outcome) error {
	state := StateDone
	if !o.Healthy {
		state = StateFailed
	}
	healthy := o.Healthy
	return writeRegion(ctx, s, monitorID, region, intervalSeconds, RegionState{
		State:         state,
		Healthy:       &healthy,
		StatusCode:    o.StatusCode,
		LatencyMs:     o.LatencyMs,
		FailureReason: o.FailureReason,
	})
}

func writeRegion(ctx context.Context, s Store, monitorID int64, region string, intervalSeconds int, rs RegionState) error {
	rs.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(rs)
	if err != nil {
		return err
	}
	key := Key(monitorID)
	if err := s.HSet(ctx, key, region, string(b)); err != nil {
		return err
	}
	return s.Expire(ctx, key, TTLFor(intervalSeconds))
}

// Get reads a monitor's per-region states. found is false when the monitor has no live
// state (never checked since the last restart, or its TTL lapsed) — the caller treats
// that as "no chips yet".
func Get(ctx context.Context, s Store, monitorID int64) (map[string]RegionState, bool, error) {
	return decode(s.HGetAll(ctx, Key(monitorID)))
}

func decode(raw map[string]string, err error) (map[string]RegionState, bool, error) {
	if err != nil {
		return nil, false, err
	}
	if len(raw) == 0 {
		return nil, false, nil
	}
	out := make(map[string]RegionState, len(raw))
	for region, v := range raw {
		var rs RegionState
		if uerr := json.Unmarshal([]byte(v), &rs); uerr != nil {
			return nil, false, uerr
		}
		out[region] = rs
	}
	return out, true, nil
}

// MultiStore adds a batched read to Store so the api can fetch many monitors' states
// in one Redis round-trip. *kv.Client satisfies it.
type MultiStore interface {
	Store
	HGetAllMulti(ctx context.Context, keys []string) (map[string]map[string]string, error)
}

// GetMany reads the live per-region states for several monitors at once. A monitor with
// no live state is omitted from the result (the caller renders it as "no chips yet").
func GetMany(ctx context.Context, s MultiStore, monitorIDs []int64) (map[int64]map[string]RegionState, error) {
	keys := make([]string, 0, len(monitorIDs))
	keyToID := make(map[string]int64, len(monitorIDs))
	for _, id := range monitorIDs {
		k := Key(id)
		keys = append(keys, k)
		keyToID[k] = id
	}
	raw, err := s.HGetAllMulti(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]map[string]RegionState, len(raw))
	for k, h := range raw {
		regions, found, derr := decode(h, nil)
		if derr != nil {
			return nil, derr
		}
		if found {
			out[keyToID[k]] = regions
		}
	}
	return out, nil
}

// Phase is the overall phase the rollup reports for a monitor's current cycle.
type Phase string

const (
	PhasePending  Phase = "pending"  // every region still scheduled
	PhaseRunning  Phase = "running"  // some running or some terminal, not all terminal
	PhaseComplete Phase = "complete" // every region terminal
)

// Rollup reduces the per-region states to an overall phase and, once complete, a
// healthy verdict by the monitor's down policy. healthy is nil until complete. This is
// a display rollup; the authoritative incident verdict is alerting's.
func Rollup(regions map[string]RegionState, policy domain.DownPolicy) (Phase, *bool) {
	if len(regions) == 0 {
		return PhasePending, nil
	}
	total, terminal, scheduled, healthy := 0, 0, 0, 0
	for _, rs := range regions {
		total++
		switch rs.State {
		case StateScheduled:
			scheduled++
		case StateDone, StateFailed:
			terminal++
			if rs.Healthy != nil && *rs.Healthy {
				healthy++
			}
		}
	}
	switch {
	case terminal == total:
		v := reduceHealthy(healthy, total, policy)
		return PhaseComplete, &v
	case scheduled == total:
		return PhasePending, nil
	default:
		return PhaseRunning, nil
	}
}

// reduceHealthy applies the down policy over the terminal regions: any = down if any
// region is down; all = down only if every region is down; quorum = down if a majority
// are down.
func reduceHealthy(healthy, total int, policy domain.DownPolicy) bool {
	down := total - healthy
	switch policy {
	case domain.DownPolicyAll:
		return down < total
	case domain.DownPolicyQuorum:
		return down*2 <= total
	default: // DownPolicyAny
		return down == 0
	}
}

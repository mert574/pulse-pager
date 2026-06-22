package alerting

import (
	"testing"
	"time"

	"pulse/internal/domain"
)

// base time for building check timestamps in the sequences.
var t0 = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

// checkedAt returns t0 plus step minutes so each check has a distinct time.
func checkedAt(step int) time.Time { return t0.Add(time.Duration(step) * time.Minute) }

// healthy builds a healthy CheckResult at the given step time.
func healthy(step int) *domain.CheckResult {
	return &domain.CheckResult{CheckedAt: checkedAt(step), Healthy: true}
}

// failed builds an unhealthy CheckResult at the given step time with a reason.
func failed(step int, reason domain.FailureReason) *domain.CheckResult {
	r := reason
	return &domain.CheckResult{CheckedAt: checkedAt(step), Healthy: false, FailureReason: &r}
}

// nextState builds the AlertState for the next check from the previous state and
// the Decision just produced, the same way the scheduler would after persisting.
func nextState(prev domain.AlertState, d Decision) domain.AlertState {
	ns := domain.AlertState{
		ConsecutiveFails: d.NewConsecutive,
		FirstFailAt:      d.NewFirstFailAt,
		OpenIncident:     prev.OpenIncident,
	}
	switch d.Action {
	case ActionOpenIncident:
		ns.OpenIncident = &domain.Incident{
			StartedAt:   d.IncidentStartedAt,
			CauseReason: d.CauseReason,
		}
	case ActionCloseIncident:
		ns.OpenIncident = nil
	}
	return ns
}

func notifyType(d Decision) NotifyEventType {
	if d.Notify == nil {
		return ""
	}
	return d.Notify.Type
}

// TestSequenceT3 walks the full PRD 12.5 example table with T=3 and asserts the
// action, consecutive count, and notification at each step. It also checks that
// the incident opened at step 6 has started_at = the step 4 check time (the
// first fail of the run), not the step 6 threshold-crossing time.
func TestSequenceT3(t *testing.T) {
	eng := New()
	m := &domain.Monitor{FailureThreshold: 3}

	type step struct {
		name           string
		res            *domain.CheckResult
		wantAction     Action
		wantConsec     int
		wantNotify     NotifyEventType
		wantStartedSet bool // when true, IncidentStartedAt must equal step4 time
	}

	steps := []step{
		{"1 H", healthy(1), ActionNone, 0, "", false},
		{"2 F", failed(2, domain.ReasonTimeout), ActionNone, 1, "", false},
		{"3 H blip reset", healthy(3), ActionNone, 0, "", false},
		{"4 F first of run", failed(4, domain.ReasonConnectionError), ActionNone, 1, "", false},
		{"5 F", failed(5, domain.ReasonConnectionError), ActionNone, 2, "", false},
		{"6 F open incident", failed(6, domain.ReasonStatusMismatch), ActionOpenIncident, 3, EventDown, true},
		{"7 F stay down", failed(7, domain.ReasonStatusMismatch), ActionNone, 4, "", false},
		{"8 H close recovery", healthy(8), ActionCloseIncident, 0, EventRecovery, false},
	}

	state := domain.AlertState{}
	for _, s := range steps {
		d := eng.Apply(m, s.res, &state)

		if d.Action != s.wantAction {
			t.Errorf("%s: action = %d, want %d", s.name, d.Action, s.wantAction)
		}
		if d.NewConsecutive != s.wantConsec {
			t.Errorf("%s: consecutive = %d, want %d", s.name, d.NewConsecutive, s.wantConsec)
		}
		if got := notifyType(d); got != s.wantNotify {
			t.Errorf("%s: notify = %q, want %q", s.name, got, s.wantNotify)
		}
		if s.wantStartedSet {
			if !d.IncidentStartedAt.Equal(checkedAt(4)) {
				t.Errorf("%s: started_at = %v, want step 4 time %v", s.name, d.IncidentStartedAt, checkedAt(4))
			}
			if d.CauseReason != domain.ReasonStatusMismatch {
				t.Errorf("%s: cause = %q, want %q", s.name, d.CauseReason, domain.ReasonStatusMismatch)
			}
		}
		if s.wantAction == ActionCloseIncident {
			if !d.IncidentEndedAt.Equal(s.res.CheckedAt) {
				t.Errorf("%s: ended_at = %v, want %v", s.name, d.IncidentEndedAt, s.res.CheckedAt)
			}
			if d.CloseReason != domain.CloseRecovered {
				t.Errorf("%s: close reason = %q, want %q", s.name, d.CloseReason, domain.CloseRecovered)
			}
		}

		state = nextState(state, d)
	}

	if state.OpenIncident != nil {
		t.Errorf("after recovery, expected no open incident, got %+v", state.OpenIncident)
	}
}

// TestThresholdOne covers T=1: a single F opens the incident immediately with a
// down event and started_at = that check's time, then the next H closes it with
// a recovery event.
func TestThresholdOne(t *testing.T) {
	eng := New()
	m := &domain.Monitor{FailureThreshold: 1}
	state := domain.AlertState{}

	d := eng.Apply(m, failed(2, domain.ReasonTimeout), &state)
	if d.Action != ActionOpenIncident {
		t.Fatalf("step F: action = %d, want ActionOpenIncident", d.Action)
	}
	if d.NewConsecutive != 1 {
		t.Errorf("step F: consecutive = %d, want 1", d.NewConsecutive)
	}
	if notifyType(d) != EventDown {
		t.Errorf("step F: notify = %q, want down", notifyType(d))
	}
	if !d.IncidentStartedAt.Equal(checkedAt(2)) {
		t.Errorf("step F: started_at = %v, want %v", d.IncidentStartedAt, checkedAt(2))
	}
	if d.CauseReason != domain.ReasonTimeout {
		t.Errorf("step F: cause = %q, want timeout", d.CauseReason)
	}
	state = nextState(state, d)

	d = eng.Apply(m, healthy(3), &state)
	if d.Action != ActionCloseIncident {
		t.Fatalf("step H: action = %d, want ActionCloseIncident", d.Action)
	}
	if d.NewConsecutive != 0 {
		t.Errorf("step H: consecutive = %d, want 0", d.NewConsecutive)
	}
	if notifyType(d) != EventRecovery {
		t.Errorf("step H: notify = %q, want recovery", notifyType(d))
	}
	if !d.IncidentEndedAt.Equal(checkedAt(3)) {
		t.Errorf("step H: ended_at = %v, want %v", d.IncidentEndedAt, checkedAt(3))
	}
	if d.NewFirstFailAt != nil {
		t.Errorf("step H: first_fail_at = %v, want nil", d.NewFirstFailAt)
	}
}

// TestBlip checks that F then H with no incident opened resets consecutive to 0
// and sends no notification, and clears first_fail_at.
func TestBlip(t *testing.T) {
	eng := New()
	m := &domain.Monitor{FailureThreshold: 3}
	state := domain.AlertState{}

	d := eng.Apply(m, failed(2, domain.ReasonTimeout), &state)
	if d.Action != ActionNone || d.NewConsecutive != 1 || d.Notify != nil {
		t.Fatalf("F: got action=%d consec=%d notify=%v", d.Action, d.NewConsecutive, d.Notify)
	}
	if d.NewFirstFailAt == nil || !d.NewFirstFailAt.Equal(checkedAt(2)) {
		t.Errorf("F: first_fail_at = %v, want step 2 time", d.NewFirstFailAt)
	}
	state = nextState(state, d)

	d = eng.Apply(m, healthy(3), &state)
	if d.Action != ActionNone {
		t.Errorf("H: action = %d, want ActionNone", d.Action)
	}
	if d.NewConsecutive != 0 {
		t.Errorf("H: consecutive = %d, want 0", d.NewConsecutive)
	}
	if d.Notify != nil {
		t.Errorf("H: notify = %v, want nil", d.Notify)
	}
	if d.NewFirstFailAt != nil {
		t.Errorf("H: first_fail_at = %v, want nil", d.NewFirstFailAt)
	}
}

// TestFailWhileDown checks that a fail while an incident is already open returns
// ActionNone with no notify, still increments consecutive, and carries
// first_fail_at forward unchanged (not reset to the new check time).
func TestFailWhileDown(t *testing.T) {
	eng := New()
	m := &domain.Monitor{FailureThreshold: 1}

	first := checkedAt(2)
	state := domain.AlertState{
		ConsecutiveFails: 1,
		FirstFailAt:      &first,
		OpenIncident:     &domain.Incident{StartedAt: first, CauseReason: domain.ReasonTimeout},
	}

	d := eng.Apply(m, failed(3, domain.ReasonConnectionError), &state)
	if d.Action != ActionNone {
		t.Errorf("action = %d, want ActionNone (no re-notify while down)", d.Action)
	}
	if d.Notify != nil {
		t.Errorf("notify = %v, want nil", d.Notify)
	}
	if d.NewConsecutive != 2 {
		t.Errorf("consecutive = %d, want 2", d.NewConsecutive)
	}
	if d.NewFirstFailAt == nil || !d.NewFirstFailAt.Equal(first) {
		t.Errorf("first_fail_at = %v, want carried %v", d.NewFirstFailAt, first)
	}
}

// TestNoChannelsStillTransitions documents that Apply does not look at channels.
// A monitor with no attached channels still opens and closes incidents and still
// reports the notify event; the caller is what decides there is nobody to send
// to. Apply is pure and channel-agnostic.
func TestNoChannelsStillTransitions(t *testing.T) {
	eng := New()
	m := &domain.Monitor{FailureThreshold: 1, ChannelIDs: nil}
	state := domain.AlertState{}

	d := eng.Apply(m, failed(2, domain.ReasonTimeout), &state)
	if d.Action != ActionOpenIncident {
		t.Errorf("action = %d, want ActionOpenIncident even with no channels", d.Action)
	}
	if notifyType(d) != EventDown {
		t.Errorf("notify = %q, want down even with no channels", notifyType(d))
	}
}

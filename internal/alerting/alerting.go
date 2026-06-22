// Package alerting holds the pure alerting state machine from PRD 12.5.
//
// Apply is a pure function: it reads the monitor, the new check result, and the
// current AlertState, and returns a Decision describing what changed. It does no
// DB work, sends no notifications, starts no goroutines, and never reads the
// clock. The caller (the scheduler) persists the changes and sends any
// notification. This lets the whole 12.5 table be a table-driven unit test.
//
// Two transitions are NOT handled here because they are not driven by a check:
// disabling a down monitor and editing a down monitor. Those live in the api
// handlers calling store directly. See ARCHITECTURE.md section 3.5a.
package alerting

import (
	"time"

	"pulse/internal/domain"
)

// Engine applies the alerting state machine. It holds no state of its own.
type Engine struct{}

// New returns a new Engine.
func New() *Engine { return &Engine{} }

// Action is what the caller should do with the incident after a check.
type Action int

const (
	// ActionNone means no incident change. The caller still persists counters.
	ActionNone Action = iota
	// ActionOpenIncident means open a new incident for this monitor.
	ActionOpenIncident
	// ActionCloseIncident means close the open incident (recovered).
	ActionCloseIncident
)

// NotifyEventType is the kind of notification to send.
type NotifyEventType string

const (
	// EventDown fires once when an incident opens.
	EventDown NotifyEventType = "down"
	// EventRecovery fires once when an incident closes by recovery.
	EventRecovery NotifyEventType = "recovery"
)

// NotifyEvent is intentionally minimal: just the type. The scheduler builds the
// full notify.Event (monitor, incident, result) after it persists the incident,
// because the incident id and timestamps are only known then.
type NotifyEvent struct {
	Type NotifyEventType
}

// Decision is the result of Apply. The caller persists NewConsecutive and
// NewFirstFailAt for every check, applies the incident Action when set, and
// sends Notify when it is non-nil.
type Decision struct {
	Action Action

	NewConsecutive int        // value to persist for consecutive_fails
	NewFirstFailAt *time.Time // value to persist for first_fail_at (nil clears it)

	IncidentStartedAt time.Time            // set when Action == ActionOpenIncident (first fail of the run)
	CauseReason       domain.FailureReason // set when opening

	IncidentEndedAt time.Time          // set when Action == ActionCloseIncident
	CloseReason     domain.CloseReason // set when closing (recovered)

	Notify *NotifyEvent // nil for ActionNone or when no event fires
}

// Apply implements PRD 12.5 exactly. It uses res.CheckedAt as "now" and never
// reads the wall clock.
func (e *Engine) Apply(m *domain.Monitor, res *domain.CheckResult, state *domain.AlertState) Decision {
	if res.Healthy {
		return e.applyHealthy(res, state)
	}
	return e.applyUnhealthy(m, res, state)
}

// applyHealthy resets the counters. If an incident is open it closes it and
// emits a recovery event, otherwise it does nothing (a blip reset, step 3).
func (e *Engine) applyHealthy(res *domain.CheckResult, state *domain.AlertState) Decision {
	d := Decision{
		Action:         ActionNone,
		NewConsecutive: 0,
		NewFirstFailAt: nil,
	}
	if state.OpenIncident != nil {
		d.Action = ActionCloseIncident
		d.IncidentEndedAt = res.CheckedAt
		d.CloseReason = domain.CloseRecovered
		d.Notify = &NotifyEvent{Type: EventRecovery}
	}
	return d
}

// applyUnhealthy increments the consecutive count and tracks the first-fail
// time. It opens an incident when the count reaches the threshold and no
// incident is open yet, and stays quiet when already down.
func (e *Engine) applyUnhealthy(m *domain.Monitor, res *domain.CheckResult, state *domain.AlertState) Decision {
	newConsecutive := state.ConsecutiveFails + 1

	// firstFailAt is the time of the first fail in this run. On the 0->1
	// transition that is this check; after that we carry the stored value
	// forward unchanged.
	var firstFailAt *time.Time
	if state.ConsecutiveFails == 0 {
		firstFailAt = &res.CheckedAt
	} else {
		firstFailAt = state.FirstFailAt
	}

	d := Decision{
		Action:         ActionNone,
		NewConsecutive: newConsecutive,
		NewFirstFailAt: firstFailAt,
	}

	// Already down: stay down, no re-notify. Still persist the counters above.
	if state.OpenIncident != nil {
		return d
	}

	// Not yet at the threshold: keep counting, no incident.
	if newConsecutive < m.FailureThreshold {
		return d
	}

	// Crossing the threshold opens an incident. started_at is the FIRST fail of
	// the run, not this threshold-crossing check (PRD 12.5 step 4 vs 6). At this
	// point firstFailAt holds that first-fail time. Fall back to res.CheckedAt
	// for the threshold==1 case, where the first fail IS this check and the
	// stored FirstFailAt may not be set yet.
	startedAt := res.CheckedAt
	if firstFailAt != nil {
		startedAt = *firstFailAt
	}

	d.Action = ActionOpenIncident
	d.IncidentStartedAt = startedAt
	if res.FailureReason != nil {
		d.CauseReason = *res.FailureReason
	}
	d.Notify = &NotifyEvent{Type: EventDown}
	return d
}

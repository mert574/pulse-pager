package domain

// DeriveStatus follows PRD 12.1 order: disabled -> pending -> down -> up.
//   - disabled: enabled is false, takes priority over everything.
//   - pending: enabled but no check results yet.
//   - down: enabled, has results, and an incident is open.
//   - up: enabled, has results, and no incident open.
func DeriveStatus(enabled bool, hasResults bool, openIncident bool) Status {
	if !enabled {
		return StatusDisabled
	}
	if !hasResults {
		return StatusPending
	}
	if openIncident {
		return StatusDown
	}
	return StatusUp
}

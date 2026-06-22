package domain

import "testing"

func TestDeriveStatus(t *testing.T) {
	tests := []struct {
		name         string
		enabled      bool
		hasResults   bool
		openIncident bool
		want         Status
	}{
		// disabled wins over everything
		{"disabled no results", false, false, false, StatusDisabled},
		{"disabled with results and open incident", false, true, true, StatusDisabled},
		// pending: enabled, no results
		{"pending", true, false, false, StatusPending},
		{"pending ignores stray incident flag", true, false, true, StatusPending},
		// down: enabled, has results, open incident
		{"down", true, true, true, StatusDown},
		// up: enabled, has results, no incident
		{"up", true, true, false, StatusUp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveStatus(tt.enabled, tt.hasResults, tt.openIncident)
			if got != tt.want {
				t.Errorf("DeriveStatus(%v,%v,%v) = %q, want %q",
					tt.enabled, tt.hasResults, tt.openIncident, got, tt.want)
			}
		})
	}
}

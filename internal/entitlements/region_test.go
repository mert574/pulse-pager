package entitlements

import (
	"testing"

	"pulse/internal/region"
)

// Every region a plan allows must be one the registry knows, and Free must
// include the default region so a defaulted monitor on any plan can actually
// run. This catches a plan catalog that drifts from internal/region.
func TestPlanRegionsAreKnown(t *testing.T) {
	for plan, lim := range monitorLimits {
		for _, r := range lim.RegionsAllowed {
			if !region.Known(r) {
				t.Errorf("plan %q allows unknown region %q", plan, r)
			}
		}
	}
	if !monitorLimits[PlanTier1].AllowsRegion(region.Default) {
		t.Errorf("Free plan must allow the default region %q", region.Default)
	}
}

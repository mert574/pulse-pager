// Package region is the single source of truth for probe region codes. The same
// region string shows up in three places that must agree or the pipeline breaks:
// the worker's PULSE_REGION (which check.jobs.<region> topic it consumes), the
// scheduler/store default a monitor runs from when none is set, and the per-plan
// catalog in internal/entitlements. When those drifted (worker on "eu-central",
// default on "home") dispatched jobs piled up on a topic no worker read and
// monitors hung "pending". Keeping every region literal here, imported by the
// rest, means they can't quietly disagree.
package region

// Region codes. Used as bus topic suffixes, monitor config, and plan limits.
const (
	EUCentral = "eu-central"
	USEast    = "us-east"
	USWest    = "us-west"
	SAEast    = "sa-east"
)

// Default is the region a monitor runs from when it sets none, and the worker's
// region when PULSE_REGION is unset. It must be a region some worker actually
// consumes, otherwise defaulted monitors dispatch to a topic with no consumer.
const Default = EUCentral

// All is every region the product knows about, cheapest/closest first.
var All = []string{EUCentral, USEast, USWest, SAEast}

// Known reports whether code is a region the product knows about.
func Known(code string) bool {
	for _, r := range All {
		if r == code {
			return true
		}
	}
	return false
}

package region

import "testing"

// The default must be a region the registry knows, or defaulted monitors
// dispatch to a topic nothing consumes (the bug this package exists to stop).
func TestDefaultIsKnown(t *testing.T) {
	if !Known(Default) {
		t.Fatalf("Default %q is not in All %v", Default, All)
	}
}

func TestKnown(t *testing.T) {
	if Known("home") {
		t.Fatal(`"home" should not be a known region`)
	}
	if Known("") {
		t.Fatal("empty string should not be a known region")
	}
}

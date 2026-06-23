package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The publicly served spec must not advertise the operator admin surface: the
// admin paths and Admin* schemas are stripped, while the rest of the contract
// (e.g. /me) stays intact.
func TestPublicSpecHidesAdmin(t *testing.T) {
	b, err := publicSpecJSON()
	if err != nil {
		t.Fatalf("publicSpecJSON: %v", err)
	}
	if strings.Contains(string(b), "/admin/metrics") {
		t.Error("public spec still contains the /admin/metrics path")
	}
	if strings.Contains(string(b), "AdminMetrics") {
		t.Error("public spec still contains the AdminMetrics schema")
	}

	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	paths, _ := doc["paths"].(map[string]any)
	if _, ok := paths["/me"]; !ok {
		t.Error("public spec dropped /me; filter is too aggressive")
	}
	if _, ok := paths["/admin/metrics"]; ok {
		t.Error("public spec still lists /admin/metrics")
	}
}

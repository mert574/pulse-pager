//go:build integration

// Platform metrics integration test. It drives store.PlatformMetrics against a REAL
// Postgres (testcontainers, so RLS is in force) and proves the cross-org counts work
// despite the FORCE ROW LEVEL SECURITY on monitors/channels: the SECURITY DEFINER
// functions in schema.sql bypass RLS for aggregate counts. It also covers the
// activation numbers (orgs_with_monitor, median time-to-first-monitor) and the
// 30-day signup series shape.
package integration

import (
	"context"
	"testing"

	"pulse/internal/domain"
)

func TestPlatformMetrics(t *testing.T) {
	pool, cleanup := alertingPostgres(t)
	defer cleanup()
	ctx := context.Background()

	// two orgs, each with an owner user; org two is on the business plan.
	u1 := mkUser(ctx, t, pool, "owner1@example.com")
	org1, _, err := pool.CreateOrgWithOwner(ctx, "Org One", "", u1)
	if err != nil {
		t.Fatalf("create org1: %v", err)
	}
	u2 := mkUser(ctx, t, pool, "owner2@example.com")
	org2, _, err := pool.CreateOrgWithOwner(ctx, "Org Two", "", u2)
	if err != nil {
		t.Fatalf("create org2: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE organizations SET plan='business' WHERE id=$1", org2.ID); err != nil {
		t.Fatalf("set plan: %v", err)
	}

	// only org1 has a monitor, so activation is 1 of 2 orgs.
	m := &domain.Monitor{
		OrgID:               org1.ID,
		Name:                "api",
		URL:                 "https://api.example.com/health",
		Method:              "GET",
		ExpectedStatusCodes: "200",
		TimeoutSeconds:      5,
		IntervalSeconds:     60,
		Enabled:             true,
		FailureThreshold:    1,
		Regions:             []string{"eu-central"},
		DownPolicy:          domain.DownPolicyQuorum,
	}
	if _, err := pool.CreateMonitor(ctx, m); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	got, err := pool.PlatformMetrics(ctx)
	if err != nil {
		t.Fatalf("PlatformMetrics: %v", err)
	}

	if got.Users != 2 {
		t.Errorf("Users = %d, want 2", got.Users)
	}
	if got.Orgs != 2 {
		t.Errorf("Orgs = %d, want 2", got.Orgs)
	}
	// the SECURITY DEFINER count sees the monitor through FORCE RLS.
	if got.MonitorsTotal != 1 || got.MonitorsEnabled != 1 {
		t.Errorf("monitors total=%d enabled=%d, want 1/1", got.MonitorsTotal, got.MonitorsEnabled)
	}
	// activation: one org created a monitor; no checks ran, so no active orgs.
	if got.OrgsWithMonitor != 1 {
		t.Errorf("OrgsWithMonitor = %d, want 1", got.OrgsWithMonitor)
	}
	if got.ActiveOrgs7d != 0 {
		t.Errorf("ActiveOrgs7d = %d, want 0 (no check results)", got.ActiveOrgs7d)
	}
	// median time-to-first-monitor is set (org1 just got a monitor), so not nil.
	if got.MedianTimeToFirstMonitorSeconds == nil {
		t.Error("MedianTimeToFirstMonitorSeconds = nil, want a value")
	}
	// plan breakdown: free (org1) and business (org2) each present once.
	byPlan := map[string]int64{}
	for _, pc := range got.OrgsByPlan {
		byPlan[pc.Plan] = pc.Count
	}
	if byPlan["tier1"] != 1 || byPlan["tierCustom"] != 1 {
		t.Errorf("OrgsByPlan = %+v, want free=1 business=1", got.OrgsByPlan)
	}
	// signup series is one row per day for 30 days.
	if len(got.Signups) != 30 {
		t.Errorf("Signups length = %d, want 30", len(got.Signups))
	}
}

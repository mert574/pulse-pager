//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/modules/redpanda"

	"pulse/internal/alerting"
	"pulse/internal/bus"
	"pulse/internal/checker"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/scheduler"
	"pulse/internal/store"
	"pulse/internal/worker"
)

// TestPipelineSchedulerToWorker proves the Phase-0 slice end to end: a seeded
// monitor is dispatched by the scheduler, consumed and checked by the worker
// (reusing internal/checker against a real HTTP target), and its result row
// lands in Postgres.
func TestPipelineSchedulerToWorker(t *testing.T) {
	ctx := context.Background()

	pgC, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("pulse"),
		postgres.WithUsername("pulse"),
		postgres.WithPassword("pulse"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	defer func() { _ = pgC.Terminate(ctx) }()

	rpC, err := redpanda.Run(ctx, "redpandadata/redpanda:v24.2.7", redpanda.WithAutoCreateTopics())
	if err != nil {
		t.Fatalf("start redpanda: %v", err)
	}
	defer func() { _ = rpC.Terminate(ctx) }()

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	broker, err := rpC.KafkaSeedBroker(ctx)
	if err != nil {
		t.Fatal(err)
	}

	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	// A real target that always returns 200.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	// A target that fails with a response, to exercise the failure snapshot.
	failTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Debug", "yes")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("kaboom"))
	}))
	defer failTarget.Close()

	var orgID int64
	if err := pool.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Pipeline Org', 'pipeline-org') RETURNING id").Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	newMonitor := func(name, url string) *domain.Monitor {
		return &domain.Monitor{
			OrgID:               orgID,
			Name:                name,
			URL:                 url,
			Method:              "GET",
			ExpectedStatusCodes: "200",
			TimeoutSeconds:      5,
			IntervalSeconds:     1,
			Enabled:             true,
			FailureThreshold:    1,
			Regions:             []string{"home"},
			DownPolicy:          domain.DownPolicyQuorum,
		}
	}
	m := newMonitor("target", target.URL)
	if _, err := pool.CreateMonitor(ctx, m); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	mFail := newMonitor("failing", failTarget.URL)
	if _, err := pool.CreateMonitor(ctx, mFail); err != nil {
		t.Fatalf("create failing monitor: %v", err)
	}

	prod, err := bus.NewProducer([]string{broker})
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	defer prod.Close()
	cons, err := bus.NewConsumer([]string{broker}, "worker-home", bus.CheckJobsTopic("home"))
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	defer cons.Close()
	// The worker emits check.results only (ADR-0011); the alerting consumer does the
	// durable check_results upsert, so it must run for the result row to land.
	alertCons, err := bus.NewConsumer([]string{broker}, "alerting", bus.TopicCheckResults)
	if err != nil {
		t.Fatalf("alerting consumer: %v", err)
	}
	defer alertCons.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	chk := checker.New(checker.Config{BlockPrivateNetworks: false}) // allow the loopback test target

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = scheduler.New(pool, prod, nil, log, 500*time.Millisecond).Run(runCtx) }()
	go func() { _ = worker.New(pool, cons, prod, chk, entitlements.AllOn{}, nil, "home", log).Run(runCtx) }()
	go func() { _ = alerting.NewRunner(pool, alertCons, prod, log).Run(runCtx) }()

	// Wait for the healthy result row and the failure snapshot to both land.
	deadline := time.Now().Add(30 * time.Second)
	var (
		healthy   bool
		status    *int
		region    string
		gotResult bool

		failStatus  *int
		failBody    string
		failHeaders []byte
		gotSnapshot bool
	)
	for time.Now().Before(deadline) {
		if !gotResult {
			if err := pool.QueryRow(ctx,
				"SELECT healthy, status_code, region FROM check_results WHERE monitor_id=$1 ORDER BY checked_at DESC LIMIT 1",
				m.ID).Scan(&healthy, &status, &region); err == nil {
				gotResult = true
			}
		}
		if !gotSnapshot {
			if err := pool.QueryRow(ctx,
				"SELECT status_code, body, headers FROM monitor_last_failure WHERE monitor_id=$1",
				mFail.ID).Scan(&failStatus, &failBody, &failHeaders); err == nil {
				gotSnapshot = true
			}
		}
		if gotResult && gotSnapshot {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()

	if !gotResult {
		t.Fatal("no check result was persisted within the deadline")
	}
	if !healthy {
		t.Errorf("expected a healthy result for a 200 target")
	}
	if status == nil || *status != 200 {
		t.Errorf("status_code = %v, want 200", status)
	}
	if region != "home" {
		t.Errorf("region = %q, want home", region)
	}

	if !gotSnapshot {
		t.Fatal("no failure snapshot was persisted for the failing monitor")
	}
	if failStatus == nil || *failStatus != 500 {
		t.Errorf("snapshot status = %v, want 500", failStatus)
	}
	if failBody != "kaboom" {
		t.Errorf("snapshot body = %q, want \"kaboom\"", failBody)
	}
	var hdrs map[string][]string
	if err := json.Unmarshal(failHeaders, &hdrs); err != nil {
		t.Fatalf("snapshot headers not valid JSON: %v", err)
	}
	if got := hdrs["X-Debug"]; len(got) != 1 || got[0] != "yes" {
		t.Errorf("snapshot header X-Debug = %v, want [yes]", got)
	}
}

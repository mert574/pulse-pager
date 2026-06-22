//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/alerting"
	"pulse/internal/bus"
	"pulse/internal/domain"
	"pulse/internal/events"
	"pulse/internal/store"
)

// captureProducer records every notify.events message the alerting runner emits, so
// the tests can assert one down and one recovery with the right dedup ids.
type captureProducer struct {
	mu     sync.Mutex
	notify []events.NotifyEvent
}

func (p *captureProducer) Produce(_ context.Context, topic, _ string, value []byte) error {
	if topic != bus.TopicNotifyEvents {
		return nil
	}
	var ev events.NotifyEvent
	if err := json.Unmarshal(value, &ev); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notify = append(p.notify, ev)
	return nil
}

func (p *captureProducer) events() []events.NotifyEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]events.NotifyEvent, len(p.notify))
	copy(out, p.notify)
	return out
}

// feedConsumer hands the runner one record per Poll from a fixed list, then closes
// the done channel and blocks on the context. This drives handle without standing up
// Kafka, so the assertions are deterministic: runHandle waits for done, which fires
// only after every record (including redeliveries) has been handled in order.
type feedConsumer struct {
	recs []bus.Record
	i    int
	done chan struct{}
}

func (c *feedConsumer) Poll(ctx context.Context, handler func(bus.Record) error) error {
	if c.i >= len(c.recs) {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
		<-ctx.Done()
		return ctx.Err()
	}
	rec := c.recs[c.i]
	c.i++
	return handler(rec)
}

func alertingPostgres(t *testing.T) (*store.Pool, func()) {
	t.Helper()
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
	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if err := store.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool, func() { pool.Close(); _ = pgC.Terminate(ctx) }
}

func seedMonitor(t *testing.T, pool *store.Pool, threshold int) *domain.Monitor {
	t.Helper()
	ctx := context.Background()
	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations(name, slug) VALUES('Alert Org', 'alert-org') RETURNING id").Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	m := &domain.Monitor{
		OrgID:               orgID,
		Name:                "api",
		URL:                 "https://api.example.com/health",
		Method:              "GET",
		ExpectedStatusCodes: "200",
		TimeoutSeconds:      5,
		IntervalSeconds:     60,
		Enabled:             true,
		FailureThreshold:    threshold,
		Regions:             []string{"eu-central"},
		DownPolicy:          domain.DownPolicyQuorum,
	}
	if _, err := pool.CreateMonitor(ctx, m); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	return m
}

// resultRecord builds a check.results bus record for one check of a monitor.
func resultRecord(t *testing.T, m *domain.Monitor, healthy bool, checkedAt time.Time) bus.Record {
	t.Helper()
	res := domain.CheckResult{
		OrgID:     m.OrgID,
		MonitorID: m.ID,
		Region:    "eu-central",
		CheckedAt: checkedAt,
		Healthy:   healthy,
	}
	if !healthy {
		reason := domain.ReasonStatusMismatch
		code := 503
		res.FailureReason = &reason
		res.StatusCode = &code
	}
	payload, err := json.Marshal(events.CheckResultEvent{
		JobID: "x", ScheduledAt: checkedAt, Result: res,
	})
	if err != nil {
		t.Fatal(err)
	}
	return bus.Record{Topic: bus.TopicCheckResults, Key: "1", Value: payload}
}

// runHandle drives the alerting runner over a list of records by hand (one Poll per
// record), so each handle returns before the next, exactly like a single partition.
// It returns once every record has been handled in order.
func runHandle(t *testing.T, pool *store.Pool, prod alerting.Producer, recs []bus.Record) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cons := &feedConsumer{recs: recs, done: make(chan struct{})}
	runner := alerting.NewRunner(pool, cons, prod, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { _ = runner.Run(ctx); close(stopped) }()
	select {
	case <-cons.done:
	case <-time.After(30 * time.Second):
		t.Fatal("alerting did not consume all records within the deadline")
	}
	cancel()
	<-stopped
}

// TestAlerting_OpenThenRecover proves the single-region happy path: a failing result
// (threshold 1) opens an incident and emits one down event; a healthy result closes
// it and emits one recovery event with the right duration.
func TestAlerting_OpenThenRecover(t *testing.T) {
	pool, cleanup := alertingPostgres(t)
	defer cleanup()
	m := seedMonitor(t, pool, 1)

	t0 := time.Date(2026, 6, 21, 14, 0, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Minute)
	prod := &captureProducer{}
	runHandle(t, pool, prod, []bus.Record{
		resultRecord(t, m, false, t0),
		resultRecord(t, m, true, t1),
	})

	ctx := context.Background()
	// One incident, opened at t0, closed at t1, recovered.
	var (
		started time.Time
		ended   *time.Time
		closeR  *string
		count   int
	)
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM incidents WHERE monitor_id=$1", m.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("incident count = %d, want 1", count)
	}
	if err := pool.QueryRow(ctx,
		"SELECT started_at, ended_at, close_reason FROM incidents WHERE monitor_id=$1", m.ID).
		Scan(&started, &ended, &closeR); err != nil {
		t.Fatal(err)
	}
	if !started.Equal(t0) {
		t.Errorf("started_at = %v, want %v", started, t0)
	}
	if ended == nil || !ended.Equal(t1) {
		t.Errorf("ended_at = %v, want %v", ended, t1)
	}
	if closeR == nil || *closeR != "recovered" {
		t.Errorf("close_reason = %v, want recovered", closeR)
	}

	evs := prod.events()
	if len(evs) != 2 {
		t.Fatalf("notify events = %d, want 2 (one down, one recovery): %+v", len(evs), evs)
	}
	down, rec := evs[0], evs[1]
	if down.EventType != "down" {
		t.Errorf("first event type = %q, want down", down.EventType)
	}
	if rec.EventType != "recovery" {
		t.Errorf("second event type = %q, want recovery", rec.EventType)
	}
	if rec.DurationSeconds == nil || *rec.DurationSeconds != 120 {
		t.Errorf("recovery duration = %v, want 120", rec.DurationSeconds)
	}
	if down.DedupKey == rec.DedupKey {
		t.Errorf("down and recovery share a dedup key %q; they must differ", down.DedupKey)
	}
}

// TestAlerting_ThresholdAboveOne proves an incident opens only after N consecutive
// failures (failure_threshold = 3).
func TestAlerting_ThresholdAboveOne(t *testing.T) {
	pool, cleanup := alertingPostgres(t)
	defer cleanup()
	m := seedMonitor(t, pool, 3)

	base := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	prod := &captureProducer{}
	recs := []bus.Record{
		resultRecord(t, m, false, base),
		resultRecord(t, m, false, base.Add(1*time.Minute)),
		resultRecord(t, m, false, base.Add(2*time.Minute)),
	}
	// Run them one at a time so we can check the incident only appears on the 3rd.
	runHandle(t, pool, prod, recs[:2])
	ctx := context.Background()
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM incidents WHERE monitor_id=$1", m.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("after 2 fails, incident count = %d, want 0", count)
	}

	runHandle(t, pool, prod, recs[2:])
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM incidents WHERE monitor_id=$1 AND ended_at IS NULL", m.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("after 3 fails, open incident count = %d, want 1", count)
	}
	// started_at is the FIRST fail of the run, not the threshold-crossing check.
	var started time.Time
	if err := pool.QueryRow(ctx, "SELECT started_at FROM incidents WHERE monitor_id=$1", m.ID).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if !started.Equal(base) {
		t.Errorf("started_at = %v, want first-fail %v", started, base)
	}
	if evs := prod.events(); len(evs) != 1 || evs[0].EventType != "down" {
		t.Fatalf("notify events = %+v, want exactly one down", evs)
	}
}

// TestAlerting_RedeliveryIsNoop proves a re-delivered check.result does not
// double-open, double-emit, or duplicate the check_results row: the watermark skips it.
func TestAlerting_RedeliveryIsNoop(t *testing.T) {
	pool, cleanup := alertingPostgres(t)
	defer cleanup()
	m := seedMonitor(t, pool, 1)

	t0 := time.Date(2026, 6, 21, 16, 0, 0, 0, time.UTC)
	prod := &captureProducer{}
	fail := resultRecord(t, m, false, t0)
	// Deliver the same failing result three times.
	runHandle(t, pool, prod, []bus.Record{fail, fail, fail})

	ctx := context.Background()
	// Exactly one durable row (idempotent upsert on the unique key).
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM check_results WHERE monitor_id=$1", m.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("check_results rows = %d, want 1 (redelivery must not duplicate)", rows)
	}
	// Exactly one incident.
	var incidents int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM incidents WHERE monitor_id=$1", m.ID).Scan(&incidents); err != nil {
		t.Fatal(err)
	}
	if incidents != 1 {
		t.Errorf("incidents = %d, want 1 (redelivery must not double-open)", incidents)
	}
	// consecutive_fails advanced once, not three times; watermark set.
	var consec int
	var watermark *int64
	if err := pool.QueryRow(ctx,
		"SELECT consecutive_fails, last_applied_result_id FROM monitors WHERE id=$1", m.ID).
		Scan(&consec, &watermark); err != nil {
		t.Fatal(err)
	}
	if consec != 1 {
		t.Errorf("consecutive_fails = %d, want 1 (the redelivery must not double-count)", consec)
	}
	if watermark == nil {
		t.Errorf("last_applied_result_id is nil, want it set after the first apply")
	}
	// Exactly one down notify, not three.
	if evs := prod.events(); len(evs) != 1 {
		t.Errorf("notify events = %d, want 1 (redelivery must not double-emit): %+v", len(evs), evs)
	}
}

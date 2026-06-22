//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/bus"
	"pulse/internal/checker"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/events"
	"pulse/internal/notify"
	"pulse/internal/store"
	"pulse/internal/worker"
)

// resultCaptureProducer records every check.results event the worker emits, so the
// test can hand them to the alerting runner. It mirrors the captureProducer in
// alerting_test.go but on the worker's output topic.
type resultCaptureProducer struct {
	mu   sync.Mutex
	recs []bus.Record
}

func (p *resultCaptureProducer) Produce(_ context.Context, topic, key string, value []byte) error {
	if topic != bus.TopicCheckResults {
		return nil
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recs = append(p.recs, bus.Record{Topic: topic, Key: key, Value: cp})
	return nil
}

func (p *resultCaptureProducer) records() []bus.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]bus.Record, len(p.recs))
	copy(out, p.recs)
	return out
}

// runWorkerOnce drives the worker over a single check.job by hand (one Poll), so its
// emitted check.results land in prod before it returns. It reuses the real checker
// against the loopback target, exactly like the live worker.
func runWorkerOnce(t *testing.T, pool *store.Pool, prod worker.Producer, chk worker.Checker, job events.CheckJob) {
	t.Helper()
	payload, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cons := &feedConsumer{
		recs: []bus.Record{{Topic: bus.CheckJobsTopic("home"), Key: "1", Value: payload}},
		done: make(chan struct{}),
	}
	runner := worker.New(pool, cons, prod, chk, entitlements.AllOn{}, nil, "home", log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { _ = runner.Run(ctx); close(stopped) }()
	select {
	case <-cons.done:
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not consume the job within the deadline")
	}
	cancel()
	<-stopped
}

// runNotifier drives the notifier runner over the given notify events by hand, then
// waits for the recording writes to settle. It mirrors notifier_test.go's wiring.
func runNotifier(t *testing.T, pool *store.Pool, client *http.Client, evs [][]byte) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := notify.NewManager(client, log)
	mgr.SetRetryPolicy(2, func(int) time.Duration { return time.Millisecond })
	cons := &directConsumer{recs: evs, delivered: make(chan struct{})}
	runner := notify.NewRunner(mgr, notify.Default(), pool, newMemCache(), cons, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runner.Run(ctx) }()
	select {
	case <-cons.delivered:
	case <-time.After(20 * time.Second):
		t.Fatal("notifier did not process the events within the deadline")
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
}

// TestAlertingLoop_EndToEnd proves the worker -> alerting -> notifier contract across
// every seam against one Postgres. A failing target makes the worker emit a failing
// check.result; the alerting runner opens an incident and emits a notify.event; the
// notifier delivers the down message to the channel's httptest target. A redelivered
// failing result must not double-open or double-deliver (the watermark and dedup hold).
// Then a healthy result closes the incident and the notifier delivers the recovery.
func TestAlertingLoop_EndToEnd(t *testing.T) {
	pool, cleanup := alertingPostgres(t)
	defer cleanup()
	ctx := context.Background()

	var orgID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations(name, slug) VALUES('Loop Org', 'loop-org') RETURNING id").Scan(&orgID); err != nil {
		t.Fatal(err)
	}

	// The channel target: counts down vs recovery POSTs by the webhook envelope event.
	var downHits, recoveryHits int32
	channelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Event string `json:"event"`
		}
		_ = json.Unmarshal(body, &env)
		switch env.Event {
		case "down":
			atomic.AddInt32(&downHits, 1)
		case "recovery":
			atomic.AddInt32(&recoveryHits, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer channelSrv.Close()

	// A webhook channel pointed at the target.
	var channelID int64
	cfg, _ := json.Marshal(map[string]any{"url": channelSrv.URL})
	if err := pool.QueryRow(ctx,
		"INSERT INTO channels(org_id, name, type, config, enabled) VALUES($1,'hook','webhook',$2,true) RETURNING id",
		orgID, cfg).Scan(&channelID); err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	// A failing target (always 500) and a healthy one (always 200). The worker checks
	// the monitor's URL; we swap which target it points at per round by recreating the
	// job's monitor value, so the same monitor row goes down then recovers.
	failTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failTarget.Close()
	healthyTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthyTarget.Close()

	// One monitor, threshold 1, the webhook channel attached. URL is the failing
	// target so the stored row is consistent; the recovery round overrides the job's
	// monitor URL to the healthy target.
	m := &domain.Monitor{
		OrgID:               orgID,
		Name:                "loop-api",
		URL:                 failTarget.URL,
		Method:              "GET",
		ExpectedStatusCodes: "200",
		TimeoutSeconds:      5,
		IntervalSeconds:     60,
		Enabled:             true,
		FailureThreshold:    1,
		Regions:             []string{"home"},
		DownPolicy:          domain.DownPolicyQuorum,
		ChannelIDs:          []int64{channelID},
	}
	if _, err := pool.CreateMonitor(ctx, m); err != nil {
		t.Fatalf("create monitor: %v", err)
	}

	// Allow the loopback targets through the checker (no SSRF block in the test).
	chk := checker.New(checker.Config{BlockPrivateNetworks: false})

	mkJob := func(url string, scheduledAt time.Time) events.CheckJob {
		jm := *m
		jm.URL = url
		return events.CheckJob{
			JobID: "loop-job", OrgID: orgID, Region: "home", ScheduledAt: scheduledAt, Monitor: jm,
		}
	}

	// --- down round: worker -> alerting -> notifier ---

	t0 := time.Now().UTC().Truncate(time.Second)

	// 1) Worker checks the failing target and emits a failing check.result.
	workerProd := &resultCaptureProducer{}
	runWorkerOnce(t, pool, workerProd, chk, mkJob(failTarget.URL, t0))
	results := workerProd.records()
	if len(results) != 1 {
		t.Fatalf("worker emitted %d results, want 1", len(results))
	}
	// Sanity: the emitted result is unhealthy (the worker actually ran the check).
	var emitted events.CheckResultEvent
	if err := json.Unmarshal(results[0].Value, &emitted); err != nil {
		t.Fatal(err)
	}
	if emitted.Result.Healthy {
		t.Fatalf("worker emitted a healthy result for a 500 target")
	}

	// 2) Alerting consumes the failing result TWICE (the redelivery must be a no-op):
	// it upserts the durable row, opens one incident, and emits exactly one down event.
	alertProd := &captureProducer{}
	runHandle(t, pool, alertProd, []bus.Record{results[0], results[0]})

	// An incident is open, visible via the store's global list.
	openIncidents, err := pool.ListOrgIncidents(ctx, orgID, true, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(openIncidents) != 1 {
		t.Fatalf("open incidents = %d, want 1", len(openIncidents))
	}
	if openIncidents[0].MonitorID != m.ID {
		t.Fatalf("open incident monitor = %d, want %d", openIncidents[0].MonitorID, m.ID)
	}

	downEvents := alertProd.events()
	if len(downEvents) != 1 || downEvents[0].EventType != notify.EventDown {
		t.Fatalf("alerting emitted %+v, want exactly one down event (dedup of the redelivery)", downEvents)
	}
	// The emitted event must carry the channel so the notifier has somewhere to send.
	if len(downEvents[0].ChannelIDs) != 1 || downEvents[0].ChannelIDs[0] != channelID {
		t.Fatalf("down event channels = %v, want [%d]", downEvents[0].ChannelIDs, channelID)
	}

	// 3) Notifier delivers the down event (fed twice to prove dedup holds at the seam).
	downRaw, err := json.Marshal(downEvents[0])
	if err != nil {
		t.Fatal(err)
	}
	runNotifier(t, pool, channelSrv.Client(), [][]byte{downRaw, downRaw})

	if got := atomic.LoadInt32(&downHits); got != 1 {
		t.Fatalf("channel down POSTs = %d, want exactly 1 (dedup must suppress the duplicate)", got)
	}
	if got := atomic.LoadInt32(&recoveryHits); got != 0 {
		t.Fatalf("channel recovery POSTs = %d, want 0 before recovery", got)
	}
	// The delivery row was recorded as delivered for the open incident's down event.
	assertLoopDelivery(t, pool, openIncidents[0].ID, channelID, "down", "delivered")

	// --- recovery round: worker -> alerting -> notifier ---

	t1 := t0.Add(2 * time.Minute)

	// 1) Worker checks the healthy target and emits a healthy result.
	workerProd2 := &resultCaptureProducer{}
	runWorkerOnce(t, pool, workerProd2, chk, mkJob(healthyTarget.URL, t1))
	results2 := workerProd2.records()
	if len(results2) != 1 {
		t.Fatalf("worker emitted %d results on recovery, want 1", len(results2))
	}

	// 2) Alerting consumes the healthy result: it closes the incident (recovered) and
	// emits one recovery event with the duration.
	recProd := &captureProducer{}
	runHandle(t, pool, recProd, []bus.Record{results2[0]})

	// No open incidents left.
	stillOpen, err := pool.ListOrgIncidents(ctx, orgID, true, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillOpen) != 0 {
		t.Fatalf("open incidents after recovery = %d, want 0", len(stillOpen))
	}
	// The incident closed with the recovered reason (an automatic recovery, not manual).
	closed, err := pool.GetIncident(ctx, orgID, openIncidents[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.EndedAt == nil || closed.CloseReason == nil || *closed.CloseReason != domain.CloseRecovered {
		t.Fatalf("incident close_reason = %v (ended=%v), want recovered", closed.CloseReason, closed.EndedAt)
	}

	recEvents := recProd.events()
	if len(recEvents) != 1 || recEvents[0].EventType != notify.EventRecovery {
		t.Fatalf("alerting emitted %+v on recovery, want exactly one recovery event", recEvents)
	}

	// 3) Notifier delivers the recovery event to the channel.
	recRaw, err := json.Marshal(recEvents[0])
	if err != nil {
		t.Fatal(err)
	}
	runNotifier(t, pool, channelSrv.Client(), [][]byte{recRaw})

	if got := atomic.LoadInt32(&recoveryHits); got != 1 {
		t.Fatalf("channel recovery POSTs = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&downHits); got != 1 {
		t.Fatalf("channel down POSTs = %d after recovery, want still 1", got)
	}
	assertLoopDelivery(t, pool, openIncidents[0].ID, channelID, "recovery", "delivered")
}

// assertLoopDelivery reads one notify_deliveries row and checks its status.
func assertLoopDelivery(t *testing.T, pool *store.Pool, incidentID, channelID int64, eventType, wantStatus string) {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		"SELECT status FROM notify_deliveries WHERE incident_id=$1 AND channel_id=$2 AND event_type=$3",
		incidentID, channelID, eventType).Scan(&status); err != nil {
		t.Fatalf("read delivery row (inc=%d ch=%d type=%s): %v", incidentID, channelID, eventType, err)
	}
	if status != wantStatus {
		t.Errorf("delivery (type=%s) status = %q, want %q", eventType, status, wantStatus)
	}
}

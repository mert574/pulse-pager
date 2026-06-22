//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/bus"
	"pulse/internal/domain"
	"pulse/internal/events"
	"pulse/internal/notify"
	"pulse/internal/store"
)

// directConsumer feeds a fixed set of notify events straight into the handler,
// then blocks, so the test drives delivery without a Kafka container. It mirrors
// the bus.Record shape the real consumer produces.
type directConsumer struct {
	recs    [][]byte
	once    sync.Once
	delivered chan struct{}
}

func (d *directConsumer) Poll(ctx context.Context, handler func(bus.Record) error) error {
	var err error
	d.once.Do(func() {
		for _, v := range d.recs {
			if e := handler(bus.Record{Topic: bus.TopicNotifyEvents, Value: v}); e != nil {
				err = e
				return
			}
		}
		close(d.delivered)
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestNotifierDelivery(t *testing.T) {
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

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
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

	var orgID int64
	if err := pool.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Notify Org', 'notify-org') RETURNING id").Scan(&orgID); err != nil {
		t.Fatal(err)
	}

	// Outbound capture: a slack target and a generic-webhook target, both succeed.
	var slackHits int32
	var slackBody []byte
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&slackHits, 1)
		slackBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()

	var webhookHits int32
	var webhookBody []byte
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&webhookHits, 1)
		webhookBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	// A target that always 500s, to exercise retry-then-record-failure.
	var failHits int32
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&failHits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	// Seed channels (no cipher set on the pool, so config is stored as-is).
	insertChannel := func(name, typ string, cfg map[string]any) int64 {
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := pool.QueryRow(ctx,
			"INSERT INTO channels(org_id, name, type, config, enabled) VALUES($1,$2,$3,$4,true) RETURNING id",
			orgID, name, typ, raw).Scan(&id); err != nil {
			t.Fatalf("insert channel: %v", err)
		}
		return id
	}
	slackID := insertChannel("slack", "slack", map[string]any{"webhook_url": slackSrv.URL})
	webhookID := insertChannel("webhook", "webhook", map[string]any{"url": webhookSrv.URL})
	failID := insertChannel("failing", "slack", map[string]any{"webhook_url": failSrv.URL})

	// Monitor with the slack + webhook channels, a monitor with only the failing
	// channel, and a monitor with no channels.
	newMonitor := func(name string, channelIDs []int64) *domain.Monitor {
		return &domain.Monitor{
			OrgID:               orgID,
			Name:                name,
			URL:                 "https://example.test",
			Method:              "GET",
			ExpectedStatusCodes: "200",
			TimeoutSeconds:      5,
			IntervalSeconds:     60,
			Enabled:             true,
			FailureThreshold:    1,
			Regions:             []string{"home"},
			DownPolicy:          domain.DownPolicyQuorum,
			ChannelIDs:          channelIDs,
		}
	}
	mGood := newMonitor("good", []int64{slackID, webhookID})
	if _, err := pool.CreateMonitor(ctx, mGood); err != nil {
		t.Fatalf("create good monitor: %v", err)
	}
	mFail := newMonitor("fail", []int64{failID})
	if _, err := pool.CreateMonitor(ctx, mFail); err != nil {
		t.Fatalf("create fail monitor: %v", err)
	}
	mNone := newMonitor("none", nil)
	if _, err := pool.CreateMonitor(ctx, mNone); err != nil {
		t.Fatalf("create none monitor: %v", err)
	}

	// Incidents to key the delivery rows / dedup ids on.
	now := time.Now().UTC()
	mkIncident := func(monitorID int64) int64 {
		id, err := pool.CreateIncident(ctx, &domain.Incident{
			OrgID: orgID, MonitorID: monitorID, StartedAt: now, CauseReason: domain.ReasonStatusMismatch,
		})
		if err != nil {
			t.Fatalf("create incident: %v", err)
		}
		return id
	}
	incGood := mkIncident(mGood.ID)
	incFail := mkIncident(mFail.ID)
	incNone := mkIncident(mNone.ID)

	mkEvent := func(m *domain.Monitor, incID int64) []byte {
		ev := events.NotifyEvent{
			OrgID:             orgID,
			MonitorID:         m.ID,
			IncidentID:        incID,
			EventType:         notify.EventDown,
			DedupKey:          notify.DedupKey(incID, notify.EventDown),
			MonitorName:       m.Name,
			MonitorURL:        m.URL,
			MonitorMethod:     "GET",
			IncidentStartedAt: now,
			Check: domain.CheckResult{
				MonitorID: m.ID, OrgID: orgID, CheckedAt: now, Healthy: false,
			},
			SentAt: now,
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	// Feed the good event TWICE (dedup must collapse to one send), plus the fail and
	// zero-channel events once each.
	goodRaw := mkEvent(mGood, incGood)
	cons := &directConsumer{
		recs:      [][]byte{goodRaw, goodRaw, mkEvent(mFail, incFail), mkEvent(mNone, incNone)},
		delivered: make(chan struct{}),
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := notify.NewManager(slackSrv.Client(), log)
	// Keep the failure case fast: 2 attempts, 1ms backoff.
	mgr.SetRetryPolicy(2, func(int) time.Duration { return time.Millisecond })

	runner := notify.NewRunner(mgr, notify.Default(), pool, newMemCache(), cons, log)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = runner.Run(runCtx) }()

	select {
	case <-cons.delivered:
	case <-time.After(20 * time.Second):
		t.Fatal("notifier did not process the events within the deadline")
	}
	// Give the recording writes a moment after the last dispatch returns.
	time.Sleep(300 * time.Millisecond)
	cancel()

	// --- assertions ---

	// Slack + webhook each received exactly one POST (dedup collapsed the double).
	if got := atomic.LoadInt32(&slackHits); got != 1 {
		t.Errorf("slack hits = %d, want 1 (dedup should suppress the second event)", got)
	}
	if got := atomic.LoadInt32(&webhookHits); got != 1 {
		t.Errorf("webhook hits = %d, want 1", got)
	}

	// Slack payload is the {"text": ...} shape; webhook payload is the appendix-B
	// envelope with event "down" and the monitor id.
	var slackPayload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(slackBody, &slackPayload); err != nil {
		t.Fatalf("slack body not JSON: %v (%s)", err, slackBody)
	}
	if slackPayload.Text == "" {
		t.Errorf("slack text empty: %s", slackBody)
	}
	var env struct {
		Event   string `json:"event"`
		Monitor struct {
			ID string `json:"id"`
		} `json:"monitor"`
	}
	if err := json.Unmarshal(webhookBody, &env); err != nil {
		t.Fatalf("webhook body not JSON: %v (%s)", err, webhookBody)
	}
	if env.Event != "down" {
		t.Errorf("webhook event = %q, want down", env.Event)
	}
	if want := "mon_" + strconv.FormatInt(mGood.ID, 10); env.Monitor.ID != want {
		t.Errorf("webhook monitor id = %q, want %q", env.Monitor.ID, want)
	}

	// The failing channel was retried (2 attempts) then recorded failed.
	if got := atomic.LoadInt32(&failHits); got != 2 {
		t.Errorf("failing target hits = %d, want 2 (retry policy)", got)
	}

	// Delivery rows: good monitor's two channels are delivered, failing is failed.
	assertDelivery := func(incID, channelID int64, wantStatus string) {
		var status string
		var attempts int
		if err := pool.QueryRow(ctx,
			"SELECT status, attempts FROM notify_deliveries WHERE incident_id=$1 AND channel_id=$2 AND event_type='down'",
			incID, channelID).Scan(&status, &attempts); err != nil {
			t.Fatalf("read delivery row (inc=%d ch=%d): %v", incID, channelID, err)
		}
		if status != wantStatus {
			t.Errorf("delivery (inc=%d ch=%d) status = %q, want %q", incID, channelID, status, wantStatus)
		}
	}
	assertDelivery(incGood, slackID, "delivered")
	assertDelivery(incGood, webhookID, "delivered")
	assertDelivery(incFail, failID, "failed")

	// Zero-channel monitor: no delivery rows at all, no error (it was processed).
	var noneRows int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM notify_deliveries WHERE incident_id=$1", incNone).Scan(&noneRows); err != nil {
		t.Fatal(err)
	}
	if noneRows != 0 {
		t.Errorf("zero-channel monitor recorded %d delivery rows, want 0", noneRows)
	}

	// Dedup backstop: exactly one dedup row for the good incident's down id.
	var dedupRows int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM notify_dedup WHERE org_id=$1 AND dedup_id=$2",
		orgID, notify.DedupKey(incGood, notify.EventDown)).Scan(&dedupRows); err != nil {
		t.Fatal(err)
	}
	if dedupRows != 1 {
		t.Errorf("dedup rows for good incident = %d, want 1", dedupRows)
	}
}

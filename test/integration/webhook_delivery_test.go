//go:build integration

// Org-level outbound webhook DELIVERY integration test. It drives the notify.Runner
// (the same one that delivers per-monitor channels) with WithWebhooks enabled, so a
// down/recovery notify.event fans out to the org's registered, enabled webhooks. It
// uses the directConsumer from notifier_test.go (same package) to feed events without
// a Kafka container, and httptest servers as webhook receivers.
//
// Covers: a down event delivers a signed POST whose envelope and X-Pulse-Signature
// verify against the stored secret; a recovery delivers too; a disabled webhook
// receives nothing; a failing receiver (500) is retried and recorded failed on the
// webhook row; the existing per-monitor channel delivery still works alongside.
//
// Run with: go test -tags integration ./test/integration/
package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/crypto"
	"pulse/internal/domain"
	"pulse/internal/events"
	"pulse/internal/notify"
	"pulse/internal/store"
)

// capturedReq is one received delivery: its headers and raw body.
type capturedReq struct {
	sig  string
	body []byte
}

func TestOutboundWebhookDelivery(t *testing.T) {
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
	// Encrypt the signing secret at rest; the deliverer decrypts it for signing.
	cipher, err := crypto.LoadKey("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatalf("load cipher: %v", err)
	}
	pool.SetCipher(cipher)

	var orgID int64
	if err := pool.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Hook Org', 'hook-org') RETURNING id").Scan(&orgID); err != nil {
		t.Fatal(err)
	}

	// --- receivers ---
	var (
		mu          sync.Mutex
		hookReqs    []capturedReq
		disabledHit int32
		failHits    int32
		channelHit  int32
	)
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		hookReqs = append(hookReqs, capturedReq{sig: r.Header.Get("X-Pulse-Signature"), body: body})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer hookSrv.Close()

	disabledSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&disabledHit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer disabledSrv.Close()

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&failHits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	// A per-monitor channel target, to prove channel delivery still works alongside.
	channelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&channelHit, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer channelSrv.Close()

	// --- register webhooks via the store (secret encrypted at rest) ---
	mkHook := func(url string, enabled bool, secret string) int64 {
		w := &domain.OrgWebhook{OrgID: orgID, URL: url, SigningSecret: secret, Enabled: enabled}
		id, err := pool.CreateWebhook(ctx, w)
		if err != nil {
			t.Fatalf("create webhook: %v", err)
		}
		return id
	}
	const goodSecret = "whsec_test-secret-for-signing"
	goodHookID := mkHook(hookSrv.URL, true, goodSecret)
	_ = mkHook(disabledSrv.URL, false, "whsec_disabled")
	failHookID := mkHook(failSrv.URL, true, "whsec_fail")

	// --- a per-monitor channel + monitor + incident (so channel delivery runs too) ---
	// The pool has a cipher set, so the channel's secret config must be encrypted at
	// rest (the store decrypts it on read); use the store's encryptor to match.
	var channelID int64
	encCfg, err := pool.EncryptChannelConfig(domain.ChannelSlack, map[string]any{"webhook_url": channelSrv.URL}, notify.Default().SecretKeys)
	if err != nil {
		t.Fatalf("encrypt channel config: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO channels(org_id, name, type, config, enabled) VALUES($1,'ch','slack',$2,true) RETURNING id",
		orgID, encCfg).Scan(&channelID); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	mon := &domain.Monitor{
		OrgID: orgID, Name: "site", URL: "https://example.test", Method: "GET",
		ExpectedStatusCodes: "200", TimeoutSeconds: 5, IntervalSeconds: 60, Enabled: true,
		FailureThreshold: 1, Regions: []string{"eu-central"}, DownPolicy: domain.DownPolicyQuorum,
		ChannelIDs: []int64{channelID},
	}
	if _, err := pool.CreateMonitor(ctx, mon); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	now := time.Now().UTC()
	incID, err := pool.CreateIncident(ctx, &domain.Incident{
		OrgID: orgID, MonitorID: mon.ID, StartedAt: now, CauseReason: domain.ReasonStatusMismatch,
	})
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}

	mkEvent := func(eventType string) []byte {
		ev := events.NotifyEvent{
			OrgID: orgID, MonitorID: mon.ID, IncidentID: incID,
			EventType: eventType, DedupKey: notify.DedupKey(incID, eventType),
			MonitorName: mon.Name, MonitorURL: mon.URL, MonitorMethod: "GET",
			IncidentStartedAt: now,
			Check:             domain.CheckResult{MonitorID: mon.ID, OrgID: orgID, CheckedAt: now, Healthy: eventType == notify.EventRecovery},
			SentAt:            now,
		}
		if eventType == notify.EventRecovery {
			ended := now.Add(time.Minute)
			ev.IncidentEndedAt = &ended
			secs := 60
			ev.DurationSeconds = &secs
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	cons := &directConsumer{
		recs:      [][]byte{mkEvent(notify.EventDown), mkEvent(notify.EventRecovery)},
		delivered: make(chan struct{}),
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := notify.NewManager(channelSrv.Client(), log)
	mgr.SetRetryPolicy(2, func(int) time.Duration { return time.Millisecond })

	runner := notify.NewRunner(mgr, notify.Default(), pool, newMemCache(), cons, log,
		notify.WithWebhooks(pool),
		// Fast org-webhook retry: 2 attempts, 1ms backoff, generous budget.
		notify.WithWebhookDelivery(hookSrv.Client(), 2, func(int) time.Duration { return time.Millisecond }, time.Hour),
	)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = runner.Run(runCtx) }()

	select {
	case <-cons.delivered:
	case <-time.After(20 * time.Second):
		t.Fatal("notifier did not process the events within the deadline")
	}
	time.Sleep(400 * time.Millisecond) // let the recording writes land
	cancel()

	// --- assertions ---

	mu.Lock()
	reqs := append([]capturedReq(nil), hookReqs...)
	mu.Unlock()

	// The good webhook subscribes to all types (empty events list), so a down emits
	// monitor.down + incident.opened, and a recovery emits monitor.recovery +
	// incident.closed: 4 deliveries total.
	if len(reqs) != 4 {
		t.Fatalf("good webhook got %d deliveries, want 4 (down=2, recovery=2)", len(reqs))
	}

	// Verify the signature on every delivery and collect the event types seen.
	seen := map[string]bool{}
	for i, req := range reqs {
		ts, sig, ok := parseSig(req.sig)
		if !ok {
			t.Fatalf("delivery %d: bad signature header %q", i, req.sig)
		}
		want := computeSig(goodSecret, ts, req.body)
		if !hmac.Equal([]byte(sig), []byte(want)) {
			t.Fatalf("delivery %d: signature mismatch\n got %s\nwant %s\nbody %s", i, sig, want, req.body)
		}
		var env struct {
			EventID string `json:"event_id"`
			Event   string `json:"event"`
			OrgID   string `json:"org_id"`
			Data    struct {
				Monitor struct {
					ID string `json:"id"`
				} `json:"monitor"`
				Incident struct {
					ID string `json:"id"`
				} `json:"incident"`
			} `json:"data"`
		}
		if err := json.Unmarshal(req.body, &env); err != nil {
			t.Fatalf("delivery %d: body not JSON: %v (%s)", i, err, req.body)
		}
		if env.EventID == "" || env.Event == "" {
			t.Fatalf("delivery %d: missing event_id/event: %s", i, req.body)
		}
		if want := "mon_" + strconv.FormatInt(mon.ID, 10); env.Data.Monitor.ID != want {
			t.Fatalf("delivery %d: monitor id = %q, want %q", i, env.Data.Monitor.ID, want)
		}
		if want := "inc_" + strconv.FormatInt(incID, 10); env.Data.Incident.ID != want {
			t.Fatalf("delivery %d: incident id = %q, want %q", i, env.Data.Incident.ID, want)
		}
		seen[env.Event] = true
	}
	for _, et := range []string{"monitor.down", "incident.opened", "monitor.recovery", "incident.closed"} {
		if !seen[et] {
			t.Errorf("missing event type %q in deliveries", et)
		}
	}

	// The disabled webhook received nothing.
	if got := atomic.LoadInt32(&disabledHit); got != 0 {
		t.Errorf("disabled webhook got %d deliveries, want 0", got)
	}

	// The failing webhook was retried (2 attempts per event type, 2 types per event,
	// for the down event: monitor.down + incident.opened = 2 type-deliveries x 2
	// attempts = 4; plus the recovery's 2 types x 2 = 4; 8 total). We only assert it
	// was retried more than once and recorded failed.
	if got := atomic.LoadInt32(&failHits); got < 2 {
		t.Errorf("failing webhook hits = %d, want >= 2 (retry)", got)
	}
	var failStatus string
	if err := pool.QueryRow(ctx, "SELECT last_status FROM org_webhooks WHERE id = $1", failHookID).Scan(&failStatus); err != nil {
		t.Fatalf("read fail webhook status: %v", err)
	}
	if failStatus != "failed" {
		t.Errorf("failing webhook last_status = %q, want failed", failStatus)
	}

	// The good webhook recorded delivered.
	var goodStatus string
	if err := pool.QueryRow(ctx, "SELECT last_status FROM org_webhooks WHERE id = $1", goodHookID).Scan(&goodStatus); err != nil {
		t.Fatalf("read good webhook status: %v", err)
	}
	if goodStatus != "delivered" {
		t.Errorf("good webhook last_status = %q, want delivered", goodStatus)
	}

	// The per-monitor channel delivery still ran (the slack channel got at least the
	// down message; recovery too).
	if got := atomic.LoadInt32(&channelHit); got < 1 {
		t.Errorf("per-monitor channel hits = %d, want >= 1 (channel delivery still works)", got)
	}
}

// parseSig splits "t=<ts>,v1=<sig>" into ts and sig.
func parseSig(h string) (ts, sig string, ok bool) {
	parts := strings.Split(h, ",")
	if len(parts) != 2 {
		return "", "", false
	}
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "t="):
			ts = strings.TrimPrefix(p, "t=")
		case strings.HasPrefix(p, "v1="):
			sig = strings.TrimPrefix(p, "v1=")
		}
	}
	return ts, sig, ts != "" && sig != ""
}

// computeSig recomputes hex(hmac-sha256(secret, ts + "." + body)), the v1 scheme.
func computeSig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

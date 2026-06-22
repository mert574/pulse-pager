package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/domain"
)

// fixed timestamps matching the PRD 12.7 examples so the rendered text lines up
// byte for byte.
var (
	startedAt = time.Date(2026, 6, 21, 14, 0, 0, 0, time.UTC)
	endedAt   = time.Date(2026, 6, 21, 14, 10, 0, 0, time.UTC)
	checkedAt = time.Date(2026, 6, 21, 14, 0, 30, 0, time.UTC)
	sentAt    = time.Date(2026, 6, 21, 14, 0, 31, 0, time.UTC)
)

func ptrInt(i int) *int                                      { return &i }
func ptrStr(s string) *string                                { return &s }
func ptrReason(r domain.FailureReason) *domain.FailureReason { return &r }

// downEvent matches the PRD down example.
func downEvent() Event {
	return Event{
		EventType: EventDown,
		Monitor: domain.Monitor{
			ID:     123,
			Name:   "Prod API health",
			URL:    "https://api.example.com/health",
			Method: "GET",
		},
		Incident: domain.Incident{
			ID:        456,
			StartedAt: startedAt,
			EndedAt:   nil,
		},
		Check: domain.CheckResult{
			CheckedAt:     checkedAt,
			Healthy:       false,
			FailureReason: ptrReason(domain.ReasonStatusMismatch),
			StatusCode:    ptrInt(503),
			LatencyMs:     ptrInt(120),
			ErrorText:     nil,
		},
		SentAt: sentAt,
	}
}

// recoveryEvent matches the PRD recovery example.
func recoveryEvent() Event {
	end := endedAt
	rec := time.Date(2026, 6, 21, 14, 10, 0, 0, time.UTC)
	return Event{
		EventType: EventRecovery,
		Monitor: domain.Monitor{
			ID:     123,
			Name:   "Prod API health",
			URL:    "https://api.example.com/health",
			Method: "GET",
		},
		Incident: domain.Incident{
			ID:        456,
			StartedAt: startedAt,
			EndedAt:   &end,
		},
		Check: domain.CheckResult{
			CheckedAt:     rec,
			Healthy:       true,
			FailureReason: nil,
			StatusCode:    ptrInt(200),
			LatencyMs:     ptrInt(95),
			ErrorText:     nil,
		},
		DurationSeconds: ptrInt(600),
		SentAt:          time.Date(2026, 6, 21, 14, 10, 1, 0, time.UTC),
	}
}

func TestSlackText(t *testing.T) {
	wantDown := ":red_circle: *DOWN* Prod API health\n" +
		"https://api.example.com/health\n" +
		"Reason: status_mismatch (HTTP 503)\n" +
		"Down since 2026-06-21 14:00:00 UTC"
	if got := slackText(downEvent()); got != wantDown {
		t.Errorf("slack down\n got: %q\nwant: %q", got, wantDown)
	}

	wantRec := ":large_green_circle: *RECOVERED* Prod API health\n" +
		"https://api.example.com/health\n" +
		"Was down for 10m 0s (since 2026-06-21 14:00:00 UTC)"
	if got := slackText(recoveryEvent()); got != wantRec {
		t.Errorf("slack recovery\n got: %q\nwant: %q", got, wantRec)
	}
}

func TestDiscordText(t *testing.T) {
	wantDown := "**DOWN** Prod API health\n" +
		"https://api.example.com/health\n" +
		"Reason: status_mismatch (HTTP 503)\n" +
		"Down since 2026-06-21 14:00:00 UTC"
	if got := discordText(downEvent()); got != wantDown {
		t.Errorf("discord down\n got: %q\nwant: %q", got, wantDown)
	}

	wantRec := "**RECOVERED** Prod API health\n" +
		"https://api.example.com/health\n" +
		"Was down for 10m 0s (since 2026-06-21 14:00:00 UTC)"
	if got := discordText(recoveryEvent()); got != wantRec {
		t.Errorf("discord recovery\n got: %q\nwant: %q", got, wantRec)
	}
}

func TestReasonLineNoStatus(t *testing.T) {
	ev := downEvent()
	ev.Check.StatusCode = nil
	ev.Check.FailureReason = ptrReason(domain.ReasonTimeout)
	if got := reasonLine(ev); got != "Reason: timeout" {
		t.Errorf("got %q, want %q", got, "Reason: timeout")
	}
}

func TestSlackSendBody(t *testing.T) {
	var gotBody slackPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := &slackProvider{client: srv.Client()}
	cfg := map[string]any{"webhook_url": srv.URL}
	if err := n.Send(context.Background(), cfg, downEvent()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotBody.Text, ":red_circle: *DOWN* Prod API health") {
		t.Errorf("unexpected text: %q", gotBody.Text)
	}
}

func TestDiscordSendBody(t *testing.T) {
	var gotBody discordPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(204)
	}))
	defer srv.Close()

	n := &discordProvider{client: srv.Client()}
	cfg := map[string]any{"webhook_url": srv.URL}
	if err := n.Send(context.Background(), cfg, recoveryEvent()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotBody.Content, "**RECOVERED** Prod API health") {
		t.Errorf("unexpected content: %q", gotBody.Content)
	}
}

func TestWebhookEnvelopeDown(t *testing.T) {
	var raw map[string]any
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		gotHeader = r.Header.Get("X-Token")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := &webhookProvider{client: srv.Client()}
	cfg := map[string]any{
		"url":            srv.URL,
		"custom_headers": map[string]any{"X-Token": "secret"},
	}
	if err := n.Send(context.Background(), cfg, downEvent()); err != nil {
		t.Fatal(err)
	}

	if gotHeader != "secret" {
		t.Errorf("custom header not sent, got %q", gotHeader)
	}
	if raw["event"] != "down" {
		t.Errorf("event = %v", raw["event"])
	}
	mon := raw["monitor"].(map[string]any)
	if mon["id"] != "mon_123" {
		t.Errorf("monitor.id = %v, want mon_123", mon["id"])
	}
	if mon["name"] != "Prod API health" || mon["url"] != "https://api.example.com/health" || mon["method"] != "GET" {
		t.Errorf("monitor fields = %v", mon)
	}
	inc := raw["incident"].(map[string]any)
	if inc["id"] != "inc_456" {
		t.Errorf("incident.id = %v, want inc_456", inc["id"])
	}
	if inc["started_at"] != "2026-06-21T14:00:00Z" {
		t.Errorf("started_at = %v", inc["started_at"])
	}
	if v, present := inc["ended_at"]; !present || v != nil {
		t.Errorf("ended_at should be null on down, got %v present=%v", v, present)
	}
	chk := raw["check"].(map[string]any)
	if chk["healthy"] != false {
		t.Errorf("healthy = %v", chk["healthy"])
	}
	if chk["failure_reason"] != "status_mismatch" {
		t.Errorf("failure_reason = %v", chk["failure_reason"])
	}
	if chk["status_code"].(float64) != 503 {
		t.Errorf("status_code = %v", chk["status_code"])
	}
	if chk["latency_ms"].(float64) != 120 {
		t.Errorf("latency_ms = %v", chk["latency_ms"])
	}
	if v, present := chk["error"]; !present || v != nil {
		t.Errorf("error should be null, got %v", v)
	}
	if _, present := raw["duration_seconds"]; present {
		t.Errorf("duration_seconds must be absent on down")
	}
	if raw["sent_at"] != "2026-06-21T14:00:31Z" {
		t.Errorf("sent_at = %v", raw["sent_at"])
	}
}

func TestWebhookEnvelopeRecovery(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := &webhookProvider{client: srv.Client()}
	cfg := map[string]any{"url": srv.URL}
	if err := n.Send(context.Background(), cfg, recoveryEvent()); err != nil {
		t.Fatal(err)
	}

	if raw["event"] != "recovery" {
		t.Errorf("event = %v", raw["event"])
	}
	inc := raw["incident"].(map[string]any)
	if inc["ended_at"] != "2026-06-21T14:10:00Z" {
		t.Errorf("ended_at = %v, want set on recovery", inc["ended_at"])
	}
	chk := raw["check"].(map[string]any)
	if chk["healthy"] != true {
		t.Errorf("healthy = %v", chk["healthy"])
	}
	if v, present := chk["failure_reason"]; !present || v != nil {
		t.Errorf("failure_reason must be null on recovery, got %v", v)
	}
	if dv, present := raw["duration_seconds"]; !present {
		t.Errorf("duration_seconds must be present on recovery")
	} else if dv.(float64) != 600 {
		t.Errorf("duration_seconds = %v", dv)
	}
}

func TestDispatchRetrySucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	mgr := NewManager(srv.Client(), nil)
	mgr.backoff = func(int) time.Duration { return time.Millisecond }
	ch := &domain.Channel{Type: domain.ChannelSlack, Enabled: true, Config: map[string]any{"webhook_url": srv.URL}}

	mgr.Dispatch(context.Background(), downEvent(), []*domain.Channel{ch})
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 attempts (2 fail then ok), got %d", got)
	}
}

func TestDispatchGivesUp(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	mgr := NewManager(srv.Client(), nil)
	mgr.backoff = func(int) time.Duration { return time.Millisecond }
	ch := &domain.Channel{Type: domain.ChannelSlack, Enabled: true, Config: map[string]any{"webhook_url": srv.URL}}

	mgr.Dispatch(context.Background(), downEvent(), []*domain.Channel{ch})
	if got := atomic.LoadInt32(&calls); got != int32(mgr.maxRetries) {
		t.Errorf("expected %d attempts then give up, got %d", mgr.maxRetries, got)
	}
}

func TestDispatchSkipsDisabled(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	mgr := NewManager(srv.Client(), nil)
	ch := &domain.Channel{Type: domain.ChannelSlack, Enabled: false, Config: map[string]any{"webhook_url": srv.URL}}
	mgr.Dispatch(context.Background(), downEvent(), []*domain.Channel{ch})
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("disabled channel should not be called, got %d", got)
	}
}

func TestBuildEmailDown(t *testing.T) {
	subject, body := buildEmail(downEvent())
	if subject != "[Pulse Pager] DOWN: Prod API health" {
		t.Errorf("subject = %q", subject)
	}
	for _, want := range []string{
		"Prod API health",
		"https://api.example.com/health",
		"Reason: status_mismatch (HTTP 503)",
		"Latency: 120ms",
		"Down since: 2026-06-21 14:00:00 UTC",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestBuildEmailRecovery(t *testing.T) {
	subject, body := buildEmail(recoveryEvent())
	if subject != "[Pulse Pager] RECOVERED: Prod API health" {
		t.Errorf("subject = %q", subject)
	}
	for _, want := range []string{
		"has recovered",
		"https://api.example.com/health",
		"Was down for: 10m 0s",
		"Down since: 2026-06-21 14:00:00 UTC",
		"Recovered at: 2026-06-21 14:10:00 UTC",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestSMTPSendSeam(t *testing.T) {
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte
	orig := smtpSend
	defer func() { smtpSend = orig }()
	smtpSend = func(ctx context.Context, addr, host string, auth smtp.Auth, from string, to []string, msg []byte, useTLS bool) error {
		gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, msg
		if auth == nil {
			t.Error("expected auth when username set")
		}
		return nil
	}

	n := &smtpProvider{}
	cfg := map[string]any{
		"host":     "mail.example.com",
		"port":     float64(587),
		"username": "user",
		"password": "pass",
		"from":     "pulse@example.com",
		"to":       "a@example.com, b@example.com",
		"tls":      "starttls",
	}
	if err := n.Send(context.Background(), cfg, downEvent()); err != nil {
		t.Fatal(err)
	}
	if gotAddr != "mail.example.com:587" {
		t.Errorf("addr = %q", gotAddr)
	}
	if gotFrom != "pulse@example.com" {
		t.Errorf("from = %q", gotFrom)
	}
	if len(gotTo) != 2 || gotTo[0] != "a@example.com" || gotTo[1] != "b@example.com" {
		t.Errorf("to = %v", gotTo)
	}
	if !strings.Contains(string(gotMsg), "Subject: [Pulse Pager] DOWN: Prod API health") {
		t.Errorf("msg missing subject:\n%s", gotMsg)
	}
}

func TestTestMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  domain.ChannelType
		cfg  map[string]any
	}{
		{"slack", domain.ChannelSlack, nil},
		{"discord", domain.ChannelDiscord, nil},
		{"webhook", domain.ChannelWebhook, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hit bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
				w.WriteHeader(200)
			}))
			defer srv.Close()

			cfg := map[string]any{"webhook_url": srv.URL, "url": srv.URL}
			mgr := NewManager(srv.Client(), nil)
			ch := &domain.Channel{Name: "t", Type: tc.typ, Enabled: true, Config: cfg}
			if err := mgr.Test(context.Background(), ch); err != nil {
				t.Fatal(err)
			}
			if !hit {
				t.Error("receiver was not hit")
			}
		})
	}
}

func TestTestMessageSMTP(t *testing.T) {
	orig := smtpSend
	defer func() { smtpSend = orig }()
	var called bool
	smtpSend = func(ctx context.Context, addr, host string, auth smtp.Auth, from string, to []string, msg []byte, useTLS bool) error {
		called = true
		if !strings.Contains(string(msg), "Subject: [Pulse Pager] Test message") {
			t.Errorf("missing test subject:\n%s", msg)
		}
		return nil
	}
	mgr := NewManager(nil, nil)
	ch := &domain.Channel{
		Name: "ops", Type: domain.ChannelSMTP,
		Config: map[string]any{"host": "h", "port": "25", "from": "f@x", "to": "t@x"},
	}
	if err := mgr.Test(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("smtp seam not called")
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[int]string{
		0:    "0s",
		30:   "30s",
		600:  "10m 0s",
		3905: "1h 5m 5s",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%d) = %q, want %q", in, got, want)
		}
	}
}

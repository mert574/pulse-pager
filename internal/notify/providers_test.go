package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPagerDutyTriggerAndResolveShareDedupKey(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/enqueue" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		bodies = append(bodies, m)
		w.WriteHeader(202)
	}))
	defer srv.Close()

	p := &pagerdutyProvider{client: srv.Client(), endpoint: srv.URL + "/v2/enqueue"}
	cfg := map[string]any{"routing_key": "rk_123"}

	if err := p.Send(context.Background(), cfg, downEvent()); err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), cfg, recoveryEvent()); err != nil {
		t.Fatal(err)
	}

	trig, res := bodies[0], bodies[1]
	if trig["event_action"] != "trigger" {
		t.Errorf("down event_action = %v, want trigger", trig["event_action"])
	}
	if trig["routing_key"] != "rk_123" {
		t.Errorf("routing_key = %v", trig["routing_key"])
	}
	pl, ok := trig["payload"].(map[string]any)
	if !ok {
		t.Fatalf("trigger missing payload: %v", trig)
	}
	if pl["severity"] != "critical" {
		t.Errorf("severity = %v, want critical", pl["severity"])
	}
	if res["event_action"] != "resolve" {
		t.Errorf("recovery event_action = %v, want resolve", res["event_action"])
	}
	if _, present := res["payload"]; present {
		t.Errorf("resolve must omit payload, got %v", res["payload"])
	}
	if trig["dedup_key"] != res["dedup_key"] {
		t.Errorf("dedup_key mismatch: trigger %v vs resolve %v", trig["dedup_key"], res["dedup_key"])
	}
	if trig["dedup_key"] != "pulse-inc-456" {
		t.Errorf("dedup_key = %v, want pulse-inc-456", trig["dedup_key"])
	}
}

func TestOpsgenieCreateAndCloseShareAlias(t *testing.T) {
	type req struct {
		path  string
		query string
		auth  string
		body  map[string]any
	}
	var reqs []req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		reqs = append(reqs, req{path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), body: m})
		w.WriteHeader(202)
	}))
	defer srv.Close()

	p := &opsgenieProvider{client: srv.Client(), baseURL: srv.URL}
	cfg := map[string]any{"api_key": "key_abc", "region": "us"}

	if err := p.Send(context.Background(), cfg, downEvent()); err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), cfg, recoveryEvent()); err != nil {
		t.Fatal(err)
	}

	create, closeReq := reqs[0], reqs[1]
	if create.path != "/v2/alerts" {
		t.Errorf("create path = %q", create.path)
	}
	if create.auth != "GenieKey key_abc" {
		t.Errorf("auth = %q", create.auth)
	}
	if create.body["alias"] != "pulse-inc-456" {
		t.Errorf("create alias = %v", create.body["alias"])
	}
	if create.body["priority"] != "P1" {
		t.Errorf("create priority = %v, want P1", create.body["priority"])
	}
	if closeReq.path != "/v2/alerts/pulse-inc-456/close" {
		t.Errorf("close path = %q", closeReq.path)
	}
	if closeReq.query != "identifierType=alias" {
		t.Errorf("close query = %q", closeReq.query)
	}
}

func TestOpsgenieRegionHost(t *testing.T) {
	p := &opsgenieProvider{}
	if h := p.host(map[string]any{"region": "eu"}); h != "https://api.eu.opsgenie.com" {
		t.Errorf("eu host = %q", h)
	}
	if h := p.host(map[string]any{}); h != "https://api.opsgenie.com" {
		t.Errorf("default host = %q", h)
	}
}

func TestTelegramSend(t *testing.T) {
	var gotPath string
	var body telegramMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	p := &telegramProvider{client: srv.Client(), baseURL: srv.URL}
	cfg := map[string]any{"bot_token": "TOKEN", "chat_id": "12345"}
	if err := p.Send(context.Background(), cfg, downEvent()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/botTOKEN/sendMessage" {
		t.Errorf("path = %q, want /botTOKEN/sendMessage", gotPath)
	}
	if body.ChatID != "12345" {
		t.Errorf("chat_id = %q", body.ChatID)
	}
	if !strings.Contains(body.Text, "DOWN: Prod API health") {
		t.Errorf("text = %q", body.Text)
	}
}

func TestTeamsSendAdaptiveCard(t *testing.T) {
	var body teamsMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	p := &teamsProvider{client: srv.Client()}
	cfg := map[string]any{"webhook_url": srv.URL}
	if err := p.Send(context.Background(), cfg, downEvent()); err != nil {
		t.Fatal(err)
	}
	if body.Type != "message" {
		t.Errorf("type = %q, want message", body.Type)
	}
	if len(body.Attachments) != 1 {
		t.Fatalf("attachments = %d", len(body.Attachments))
	}
	att := body.Attachments[0]
	if att.ContentType != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("contentType = %q", att.ContentType)
	}
	if att.Content.Type != "AdaptiveCard" || att.Content.Version != "1.4" {
		t.Errorf("card = %+v", att.Content)
	}
	if len(att.Content.Body) == 0 || !strings.Contains(att.Content.Body[0].Text, "DOWN: Prod API health") {
		t.Errorf("card body = %+v", att.Content.Body)
	}
}

func TestTwilioSend(t *testing.T) {
	var gotPath, gotCT, gotUser, gotPass string
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotUser, gotPass, _ = r.BasicAuth()
		raw, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(raw))
		w.WriteHeader(201)
	}))
	defer srv.Close()

	p := &twilioProvider{client: srv.Client(), baseURL: srv.URL}
	cfg := map[string]any{
		"account_sid": "AC123",
		"auth_token":  "tok",
		"from":        "+15550001111",
		"to":          "+15552223333",
	}
	if err := p.Send(context.Background(), cfg, downEvent()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/2010-04-01/Accounts/AC123/Messages.json" {
		t.Errorf("path = %q", gotPath)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotUser != "AC123" || gotPass != "tok" {
		t.Errorf("basic auth = %q/%q", gotUser, gotPass)
	}
	if form.Get("To") != "+15552223333" || form.Get("From") != "+15550001111" {
		t.Errorf("to/from = %q/%q", form.Get("To"), form.Get("From"))
	}
	if !strings.Contains(form.Get("Body"), "DOWN: Prod API health") {
		t.Errorf("body = %q", form.Get("Body"))
	}
}

func TestProviderValidate(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		good map[string]any
		bad  map[string]any
	}{
		{"pagerduty", &pagerdutyProvider{}, map[string]any{"routing_key": "x"}, map[string]any{}},
		{"opsgenie", &opsgenieProvider{}, map[string]any{"api_key": "x"}, map[string]any{}},
		{"telegram", &telegramProvider{}, map[string]any{"bot_token": "x", "chat_id": "1"}, map[string]any{"bot_token": "x"}},
		{"teams", &teamsProvider{}, map[string]any{"webhook_url": "x"}, map[string]any{}},
		{"twilio", &twilioProvider{}, map[string]any{"account_sid": "a", "auth_token": "b", "from": "+1", "to": "+2"}, map[string]any{"account_sid": "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.p.Validate(tc.good); err != nil {
				t.Errorf("good config errored: %v", err)
			}
			if err := tc.p.Validate(tc.bad); err == nil {
				t.Error("bad config should error")
			}
		})
	}
}

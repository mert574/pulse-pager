//go:build integration

// Notification-channel management HTTP API integration test. It exercises the REAL
// handlers in internal/api wired to the REAL authn services and the REAL Postgres
// store (testcontainers, RLS in force), a FAKE Google OIDC IdP, and a capturing
// Mailer so a member/viewer can be created for the role tests. It reuses the helpers
// from api_identity_test.go and api_members_test.go (same package).
//
// Covers: the channel-type catalog (v1 types available, phased types not); owner
// creates a channel (the secret url is redacted in the response); list redacts the
// secret; the secret is encrypted at rest; update with a blank secret keeps the
// stored value while name/enabled change; test-send delivers (204) and surfaces a
// delivery failure (422); plan-gating rejects a phased type (422) and an unknown
// type (422); a member can manage (member+), a viewer cannot (403); delete.
//
// Run with: go test -tags integration ./test/integration/
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/api"
	"pulse/internal/authn"
	"pulse/internal/crypto"
	"pulse/internal/entitlements"
	"pulse/internal/store"
)

type channelDTO struct {
	ID      string         `json:"id"`
	OrgID   string         `json:"org_id"`
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config"`
}

type channelCatalogDTO struct {
	ChannelTypes []struct {
		Type      string `json:"type"`
		Available bool   `json:"available"`
		Fields    []struct {
			Key      string `json:"key"`
			Required bool   `json:"required"`
			Secret   bool   `json:"secret"`
		} `json:"config_fields"`
	} `json:"channel_types"`
}

func TestChannelsManagement(t *testing.T) {
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

	host, err := pgC.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}
	adminDSN := fmt.Sprintf("postgres://pulse:pulse@%s:%s/pulse?sslmode=disable", host, port.Port())
	appDSN := fmt.Sprintf("postgres://pulse_app:pulse_app@%s:%s/pulse?sslmode=disable", host, port.Port())

	admin, err := store.Open(ctx, adminDSN)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()
	if err := store.ApplySchema(ctx, admin); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	app, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	defer app.Close()
	// Wire a real cipher so the channel's secret url is encrypted at rest (the at-rest
	// test reads the raw column and asserts it is not the plaintext url).
	cipher, err := crypto.LoadKey("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=") // 32-byte key, base64
	if err != nil {
		t.Fatalf("load cipher key: %v", err)
	}
	app.SetCipher(cipher)

	idp := newFakeIdP(t)
	google, err := authn.NewGoogleProvider(ctx, authn.OIDCConfig{
		IssuerURL:    idp.server.URL,
		ClientID:     "fake-client",
		ClientSecret: "fake-secret",
		RedirectURL:  "http://api.test/auth/google/callback",
	})
	if err != nil {
		t.Fatalf("build google provider: %v", err)
	}

	cache := newMemCache()
	signing, err := authn.GenerateSigningKey("itest-kid-1")
	if err != nil {
		t.Fatalf("gen signing key: %v", err)
	}
	jwtIssuer := authn.NewJWTIssuer("pulse", "pulse-api", signing)
	loginSvc := authn.NewLoginService([]authn.Provider{google}, cache, app)
	refreshSvc := authn.NewRefreshService(app)
	keyVerifier := authn.NewAPIKeyVerifier(app, cache)
	auth := authn.NewAuthenticator(jwtIssuer, keyVerifier, app, cache)
	mailer := &captureMailer{}

	srv := api.New(api.Config{
		Store:      app,
		Login:      loginSvc,
		JWT:        jwtIssuer,
		Refresh:    refreshSvc,
		Cookies:    authn.CookieConfig{Secure: false},
		Auth:       auth,
		Keys:       keyVerifier,
		AppBaseURL: "http://app.test",
		Seats:      entitlements.FixedSeats{Cap: 5},
		Mailer:     mailer,
	})
	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

	// Two delivery targets: one accepts (200 -> test send is 204), one fails (500 ->
	// test send is 422). The channel url is the secret, stored encrypted; the test
	// path decrypts it and POSTs here, so a real round-trip is exercised.
	okTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer okTarget.Close()
	badTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer badTarget.Close()

	newClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	login := func(t *testing.T, sub, email string) (*http.Client, meDTO) {
		t.Helper()
		idp.sub = sub
		idp.email = email
		idp.name = email
		c := newClient()
		doLogin(t, c, ts, idp)
		return c, getMe(t, c, ts)
	}
	post := func(t *testing.T, c *http.Client, path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	put := func(t *testing.T, c *http.Client, path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	ownerClient, ownerMe := login(t, "owner-sub", "owner@example.com")
	if len(ownerMe.Orgs) != 1 || ownerMe.Orgs[0].Role != "owner" {
		t.Fatalf("owner setup wrong: %+v", ownerMe.Orgs)
	}
	orgID := ownerMe.Orgs[0].OrgID
	base := "/api/v1/orgs/" + orgID + "/channels"

	var createdID, plaintextURL string

	t.Run("catalog_lists_types_with_availability", func(t *testing.T) {
		resp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/channel-types")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("catalog: want 200, got %d", resp.StatusCode)
		}
		var cat channelCatalogDTO
		if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
			t.Fatalf("decode catalog: %v", err)
		}
		avail := map[string]bool{}
		for _, e := range cat.ChannelTypes {
			avail[e.Type] = e.Available
		}
		for _, want := range []string{"slack", "discord", "webhook", "smtp"} {
			if !avail[want] {
				t.Fatalf("expected %q available on plan, catalog: %+v", want, avail)
			}
		}
		for _, want := range []string{"telegram", "pagerduty", "opsgenie", "teams", "twilio"} {
			if avail[want] {
				t.Fatalf("expected phased type %q NOT available on plan, catalog: %+v", want, avail)
			}
		}
		// The webhook url field is declared required + secret.
		for _, e := range cat.ChannelTypes {
			if e.Type != "webhook" {
				continue
			}
			for _, f := range e.Fields {
				if f.Key == "url" && (!f.Required || !f.Secret) {
					t.Fatalf("webhook url field should be required+secret: %+v", f)
				}
			}
		}
	})

	t.Run("business_plan_unlocks_phased_types", func(t *testing.T) {
		// Operator upgrades the org to the top tier; the phased integrations become
		// available in the catalog (the FE then stops disabling them).
		var orgIDInt int64
		fmt.Sscan(orgID, &orgIDInt)
		if _, err := admin.Exec(ctx, "UPDATE organizations SET plan='business' WHERE id=$1", orgIDInt); err != nil {
			t.Fatalf("set business plan: %v", err)
		}
		t.Cleanup(func() {
			admin.Exec(ctx, "UPDATE organizations SET plan='free' WHERE id=$1", orgIDInt)
		})

		resp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/channel-types")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var cat channelCatalogDTO
		decode(t, resp, &cat)
		avail := map[string]bool{}
		for _, e := range cat.ChannelTypes {
			avail[e.Type] = e.Available
		}
		for _, want := range []string{"telegram", "pagerduty", "opsgenie", "teams", "twilio"} {
			if !avail[want] {
				t.Fatalf("expected phased type %q available on business, catalog: %+v", want, avail)
			}
		}
	})

	t.Run("owner_creates_channel_secret_redacted", func(t *testing.T) {
		plaintextURL = okTarget.URL
		resp := post(t, ownerClient, base, fmt.Sprintf(`{"name":"Ops Hook","type":"webhook","enabled":true,"config":{"url":%q}}`, plaintextURL))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create: want 201, got %d", resp.StatusCode)
		}
		var ch channelDTO
		if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
			t.Fatalf("decode created: %v", err)
		}
		if ch.Name != "Ops Hook" || ch.Type != "webhook" || !ch.Enabled {
			t.Fatalf("channel metadata wrong: %+v", ch)
		}
		if u, _ := ch.Config["url"].(string); u != "" {
			t.Fatalf("secret url not redacted in create response: %q", u)
		}
		createdID = ch.ID
	})

	t.Run("list_redacts_secret", func(t *testing.T) {
		resp, err := ownerClient.Get(ts.URL + base)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(raw), plaintextURL) {
			t.Fatalf("list leaked the secret url: %s", raw)
		}
		var list []channelDTO
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("want 1 channel, got %d", len(list))
		}
	})

	t.Run("secret_encrypted_at_rest", func(t *testing.T) {
		var stored string
		if err := admin.QueryRow(ctx, "SELECT config->>'url' FROM channels WHERE id = $1", createdID).Scan(&stored); err != nil {
			t.Fatalf("read raw config: %v", err)
		}
		if stored == plaintextURL {
			t.Fatalf("channel url stored in plaintext at rest")
		}
		if stored == "" {
			t.Fatalf("channel url empty at rest")
		}
	})

	t.Run("update_blank_secret_keeps_stored", func(t *testing.T) {
		// Rename + disable, sending a blank url: the stored secret must survive.
		resp := put(t, ownerClient, base+"/"+createdID, `{"name":"Renamed Hook","type":"webhook","enabled":false,"config":{"url":""}}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("update: want 200, got %d", resp.StatusCode)
		}
		var ch channelDTO
		if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
			t.Fatalf("decode update: %v", err)
		}
		if ch.Name != "Renamed Hook" || ch.Enabled != false {
			t.Fatalf("update did not apply: %+v", ch)
		}
		// The kept secret still decrypts to the original url: a test-send reaches okTarget.
		tr := post(t, ownerClient, base+"/"+createdID+"/test", "")
		defer tr.Body.Close()
		if tr.StatusCode != http.StatusNoContent {
			t.Fatalf("test after blank-secret update: want 204 (kept url), got %d", tr.StatusCode)
		}
	})

	t.Run("test_send_failure_is_422", func(t *testing.T) {
		// Point the channel at the failing target, then a test-send surfaces the error.
		resp := put(t, ownerClient, base+"/"+createdID, fmt.Sprintf(`{"name":"Renamed Hook","type":"webhook","enabled":true,"config":{"url":%q}}`, badTarget.URL))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("repoint update: want 200, got %d", resp.StatusCode)
		}
		tr := post(t, ownerClient, base+"/"+createdID+"/test", "")
		defer tr.Body.Close()
		if tr.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("test against failing target: want 422, got %d", tr.StatusCode)
		}
	})

	t.Run("plan_gating_and_unknown_type_422", func(t *testing.T) {
		// telegram is a known type but not in the Free plan's allowed set.
		gated := post(t, ownerClient, base, `{"name":"tg","type":"telegram","enabled":true,"config":{"bot_token":"x","chat_id":"y"}}`)
		_ = gated.Body.Close()
		if gated.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("plan-gated type: want 422, got %d", gated.StatusCode)
		}
		// an unknown type is rejected too.
		unknown := post(t, ownerClient, base, `{"name":"x","type":"carrierpigeon","enabled":true,"config":{}}`)
		_ = unknown.Body.Close()
		if unknown.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("unknown type: want 422, got %d", unknown.StatusCode)
		}
	})

	t.Run("member_can_manage_viewer_cannot", func(t *testing.T) {
		invite := func(email, role string) {
			body := fmt.Sprintf(`{"email":%q,"role":%q}`, email, role)
			inv := post(t, ownerClient, "/api/v1/orgs/"+orgID+"/invitations", body)
			_ = inv.Body.Close()
			if inv.StatusCode != http.StatusCreated && inv.StatusCode != http.StatusOK {
				t.Fatalf("invite %s: want 200/201, got %d", role, inv.StatusCode)
			}
		}
		accept := func(c *http.Client, email string) {
			token := mailer.tokenFor(t, email)
			acc, err := c.Post(ts.URL+"/api/v1/invitations/"+token+"/accept", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = acc.Body.Close()
			if acc.StatusCode < 200 || acc.StatusCode >= 300 {
				t.Fatalf("accept: want 2xx, got %d", acc.StatusCode)
			}
		}

		// A member (member+) CAN create a channel.
		invite("member@example.com", "member")
		memberClient, _ := login(t, "member-sub", "member@example.com")
		accept(memberClient, "member@example.com")
		mc := post(t, memberClient, base, fmt.Sprintf(`{"name":"member hook","type":"webhook","enabled":true,"config":{"url":%q}}`, okTarget.URL))
		_ = mc.Body.Close()
		if mc.StatusCode != http.StatusCreated {
			t.Fatalf("member create channel: want 201, got %d", mc.StatusCode)
		}

		// A viewer CANNOT list or create.
		invite("viewer@example.com", "viewer")
		viewerClient, _ := login(t, "viewer-sub", "viewer@example.com")
		accept(viewerClient, "viewer@example.com")
		vl, err := viewerClient.Get(ts.URL + base)
		if err != nil {
			t.Fatal(err)
		}
		_ = vl.Body.Close()
		if vl.StatusCode != http.StatusForbidden {
			t.Fatalf("viewer list channels: want 403, got %d", vl.StatusCode)
		}
		vc := post(t, viewerClient, base, `{"name":"nope","type":"webhook","enabled":true,"config":{"url":"http://x"}}`)
		_ = vc.Body.Close()
		if vc.StatusCode != http.StatusForbidden {
			t.Fatalf("viewer create channel: want 403, got %d", vc.StatusCode)
		}
	})

	t.Run("delete", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+base+"/"+createdID, nil)
		resp, err := ownerClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete: want 204, got %d", resp.StatusCode)
		}
		// A test-send to the deleted channel is now a 404.
		tr := post(t, ownerClient, base+"/"+createdID+"/test", "")
		_ = tr.Body.Close()
		if tr.StatusCode != http.StatusNotFound {
			t.Fatalf("test after delete: want 404, got %d", tr.StatusCode)
		}
	})
}

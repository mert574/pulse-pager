//go:build integration

// Org-level outbound webhook management HTTP API integration test. It exercises the
// REAL handlers in internal/api wired to the REAL authn services and the REAL
// Postgres store (testcontainers, RLS in force), a FAKE Google OIDC IdP, and a
// capturing Mailer so a member/viewer can be created for the role tests. It reuses
// the helpers from api_identity_test.go and api_members_test.go (same package).
//
// Covers: owner registers a webhook (the signing secret is returned once, with a
// whsec_ prefix); list/get never carry the secret; update (url/enabled/events);
// rotate-secret returns a new secret once; delete; member and viewer humans cannot
// manage (403); https url validation; the secret is encrypted at rest (the raw DB
// column is not the plaintext secret).
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

type webhookDTO struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	Enabled    bool     `json:"enabled"`
	Events     []string `json:"events"`
	LastStatus *string  `json:"last_status"`
}

type webhookCreatedDTO struct {
	Webhook webhookDTO `json:"webhook"`
	Secret  string     `json:"secret"`
}

func TestOutboundWebhooksManagement(t *testing.T) {
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
	// Wire a real cipher so the signing secret is encrypted at rest (the at-rest test
	// reads the raw column and asserts it is not the plaintext secret).
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
	emailPub := &captureEmail{store: app}

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
		Email:      emailPub,
	})
	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

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
	base := "/api/v1/orgs/" + orgID + "/webhooks"

	var createdID, firstSecret string

	t.Run("owner_creates_webhook_secret_once", func(t *testing.T) {
		resp := post(t, ownerClient, base, `{"url":"https://hooks.example.com/in","events":["monitor.down","monitor.recovery"]}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create webhook: want 201, got %d", resp.StatusCode)
		}
		var created webhookCreatedDTO
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatalf("decode created: %v", err)
		}
		if !strings.HasPrefix(created.Secret, "whsec_") {
			t.Fatalf("secret missing whsec_ prefix: %q", created.Secret)
		}
		if created.Webhook.URL != "https://hooks.example.com/in" || created.Webhook.Enabled != true {
			t.Fatalf("webhook metadata wrong: %+v", created.Webhook)
		}
		if len(created.Webhook.Events) != 2 {
			t.Fatalf("events wrong: %+v", created.Webhook.Events)
		}
		createdID = created.Webhook.ID
		firstSecret = created.Secret
	})

	t.Run("list_and_get_never_carry_secret", func(t *testing.T) {
		resp, err := ownerClient.Get(ts.URL + base)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list: want 200, got %d", resp.StatusCode)
		}
		raw, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(raw), firstSecret) {
			t.Fatalf("list leaked the secret: %s", raw)
		}
		var list []webhookDTO
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("want 1 webhook, got %d", len(list))
		}

		gresp, err := ownerClient.Get(ts.URL + base + "/" + createdID)
		if err != nil {
			t.Fatal(err)
		}
		defer gresp.Body.Close()
		graw, _ := io.ReadAll(gresp.Body)
		if strings.Contains(string(graw), firstSecret) {
			t.Fatalf("get leaked the secret: %s", graw)
		}
	})

	t.Run("secret_encrypted_at_rest", func(t *testing.T) {
		// The raw DB column must not be the plaintext secret (it is AES-GCM encrypted).
		var stored string
		if err := admin.QueryRow(ctx, "SELECT signing_secret FROM org_webhooks WHERE id = $1", createdID).Scan(&stored); err != nil {
			t.Fatalf("read raw secret: %v", err)
		}
		if stored == firstSecret {
			t.Fatalf("signing secret stored in plaintext at rest")
		}
		if stored == "" {
			t.Fatalf("signing secret empty at rest")
		}
	})

	t.Run("update_url_enabled_events", func(t *testing.T) {
		resp := put(t, ownerClient, base+"/"+createdID, `{"url":"https://hooks.example.com/v2","enabled":false,"events":["incident.opened"]}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("update: want 200, got %d", resp.StatusCode)
		}
		var w webhookDTO
		if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
			t.Fatalf("decode update: %v", err)
		}
		if w.URL != "https://hooks.example.com/v2" || w.Enabled != false {
			t.Fatalf("update did not apply: %+v", w)
		}
		if len(w.Events) != 1 || w.Events[0] != "incident.opened" {
			t.Fatalf("events not updated: %+v", w.Events)
		}
	})

	t.Run("rotate_secret_returns_new_secret_once", func(t *testing.T) {
		resp := post(t, ownerClient, base+"/"+createdID+"/rotate-secret", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("rotate: want 200, got %d", resp.StatusCode)
		}
		var rotated webhookCreatedDTO
		if err := json.NewDecoder(resp.Body).Decode(&rotated); err != nil {
			t.Fatalf("decode rotated: %v", err)
		}
		if !strings.HasPrefix(rotated.Secret, "whsec_") {
			t.Fatalf("rotated secret missing prefix: %q", rotated.Secret)
		}
		if rotated.Secret == firstSecret {
			t.Fatalf("rotate returned the same secret")
		}
		// The old secret is no longer the stored one: the at-rest column changed.
		var stored string
		if err := admin.QueryRow(ctx, "SELECT signing_secret FROM org_webhooks WHERE id = $1", createdID).Scan(&stored); err != nil {
			t.Fatalf("read raw secret after rotate: %v", err)
		}
		if stored == firstSecret {
			t.Fatalf("rotate did not change the stored secret")
		}
	})

	t.Run("https_validation", func(t *testing.T) {
		resp := post(t, ownerClient, base, `{"url":"http://insecure.example.com/in"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("http url: want 422, got %d", resp.StatusCode)
		}
		resp2 := post(t, ownerClient, base, `{"url":"not a url"}`)
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("bad url: want 422, got %d", resp2.StatusCode)
		}
	})

	t.Run("member_and_viewer_cannot_manage_403", func(t *testing.T) {
		// Owner invites a member and a viewer; each accepts; then each tries to list.
		invite := func(email, role string) {
			body := fmt.Sprintf(`{"email":%q,"role":%q}`, email, role)
			inv := post(t, ownerClient, "/api/v1/orgs/"+orgID+"/invitations", body)
			_ = inv.Body.Close()
			if inv.StatusCode != http.StatusCreated && inv.StatusCode != http.StatusOK {
				t.Fatalf("invite %s: want 200/201, got %d", role, inv.StatusCode)
			}
		}
		accept := func(c *http.Client, email string) {
			token := emailPub.tokenFor(t, email)
			acc, err := c.Post(ts.URL+"/api/v1/invitations/"+token+"/accept", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = acc.Body.Close()
			if acc.StatusCode < 200 || acc.StatusCode >= 300 {
				t.Fatalf("accept: want 2xx, got %d", acc.StatusCode)
			}
		}

		invite("member@example.com", "member")
		memberClient, _ := login(t, "member-sub", "member@example.com")
		accept(memberClient, "member@example.com")
		mresp, err := memberClient.Get(ts.URL + base)
		if err != nil {
			t.Fatal(err)
		}
		_ = mresp.Body.Close()
		if mresp.StatusCode != http.StatusForbidden {
			t.Fatalf("member list webhooks: want 403, got %d", mresp.StatusCode)
		}
		// A member also cannot create.
		mc := post(t, memberClient, base, `{"url":"https://hooks.example.com/nope"}`)
		_ = mc.Body.Close()
		if mc.StatusCode != http.StatusForbidden {
			t.Fatalf("member create webhook: want 403, got %d", mc.StatusCode)
		}

		invite("viewer@example.com", "viewer")
		viewerClient, _ := login(t, "viewer-sub", "viewer@example.com")
		accept(viewerClient, "viewer@example.com")
		vresp, err := viewerClient.Get(ts.URL + base)
		if err != nil {
			t.Fatal(err)
		}
		_ = vresp.Body.Close()
		if vresp.StatusCode != http.StatusForbidden {
			t.Fatalf("viewer list webhooks: want 403, got %d", vresp.StatusCode)
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
		// Gone now.
		g, err := ownerClient.Get(ts.URL + base + "/" + createdID)
		if err != nil {
			t.Fatal(err)
		}
		_ = g.Body.Close()
		if g.StatusCode != http.StatusNotFound {
			t.Fatalf("get after delete: want 404, got %d", g.StatusCode)
		}
	})
}

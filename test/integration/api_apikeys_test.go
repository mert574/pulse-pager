//go:build integration

// API-key management HTTP API integration test. It exercises the REAL handlers in
// internal/api wired to the REAL authn services (JWT + the API-key verifier) and the
// REAL Postgres store (testcontainers, RLS in force), a FAKE Google OIDC IdP, and a
// capturing Mailer so an invited member can be created and used for the role tests.
// It reuses the helpers from api_identity_test.go and the captureMailer from
// api_members_test.go (same package).
//
// Covers: owner creates a key (response carries the full pulse_sk_ secret once plus
// metadata); list shows metadata but never the secret; the created key authenticates
// a public-API call (GET /orgs/{orgId}/monitors via Bearer) and resolves to the key's
// org + role; revoke makes the same key fail (401); role=owner create is rejected; a
// member (human) cannot create (403); a key with role=member cannot create another
// key (403). Plus the docs routes: GET /api/docs and GET /api/openapi.yaml without auth.
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
	"pulse/internal/entitlements"
	"pulse/internal/store"
)

type apiKeyMetaDTO struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	Role       string  `json:"role"`
	CreatedBy  *string `json:"created_by"`
	LastUsedAt *string `json:"last_used_at"`
}

type apiKeyCreatedDTO struct {
	Key    apiKeyMetaDTO `json:"key"`
	Secret string        `json:"secret"`
}

func TestAPIKeys(t *testing.T) {
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

	// helpers for the key endpoints
	createKey := func(t *testing.T, c *http.Client, orgID, name, role string) *http.Response {
		t.Helper()
		body := fmt.Sprintf(`{"name":%q,"role":%q}`, name, role)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/orgs/"+orgID+"/api-keys", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	listKeys := func(t *testing.T, c *http.Client, orgID string) []apiKeyMetaDTO {
		t.Helper()
		resp, err := c.Get(ts.URL + "/api/v1/orgs/" + orgID + "/api-keys")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list keys: want 200, got %d", resp.StatusCode)
		}
		var out []apiKeyMetaDTO
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode keys: %v", err)
		}
		return out
	}

	// Owner logs in and owns a personal org.
	ownerClient, ownerMe := login(t, "owner-sub", "owner@example.com")
	if len(ownerMe.Orgs) != 1 || ownerMe.Orgs[0].Role != "owner" {
		t.Fatalf("owner setup wrong: %+v", ownerMe.Orgs)
	}
	orgID := ownerMe.Orgs[0].OrgID

	// API keys need a paid plan (Free has no API access in the pricing-matched
	// entitlements). A freshly registered org is on tier1, so put it on tier3
	// (Professional, full read+write) before exercising the key CRUD/auth subtests.
	var orgIDInt int64
	fmt.Sscan(orgID, &orgIDInt)
	if _, err := admin.Exec(ctx, "UPDATE organizations SET plan='tier3' WHERE id=$1", orgIDInt); err != nil {
		t.Fatalf("set plan: %v", err)
	}

	// --- owner creates a member-role key: response carries the full secret once + metadata ---
	var memberKeySecret string
	t.Run("owner_creates_key_secret_once", func(t *testing.T) {
		resp := createKey(t, ownerClient, orgID, "ci key", "member")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create key: want 201, got %d", resp.StatusCode)
		}
		var created apiKeyCreatedDTO
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatalf("decode created: %v", err)
		}
		if !strings.HasPrefix(created.Secret, authn.APIKeyPrefix) {
			t.Fatalf("secret missing pulse_sk_ prefix: %q", created.Secret)
		}
		if created.Key.Name != "ci key" || created.Key.Role != "member" {
			t.Fatalf("key metadata wrong: %+v", created.Key)
		}
		if !strings.HasPrefix(created.Key.Prefix, authn.APIKeyPrefix) || created.Key.Prefix == created.Secret {
			t.Fatalf("prefix should be a non-secret leading slice, got prefix=%q secret=%q", created.Key.Prefix, created.Secret)
		}
		memberKeySecret = created.Secret
	})

	// --- list shows the key metadata but NOT the secret ---
	t.Run("list_metadata_no_secret", func(t *testing.T) {
		keys := listKeys(t, ownerClient, orgID)
		if len(keys) != 1 {
			t.Fatalf("want 1 key, got %d: %+v", len(keys), keys)
		}
		// the raw list body must not contain the secret anywhere.
		resp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/api-keys")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(raw), memberKeySecret) {
			t.Fatalf("list response leaked the secret: %s", raw)
		}
	})

	// --- the created key authenticates a public-API call and resolves to its org ---
	t.Run("key_authenticates_public_api_call", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/orgs/"+orgID+"/monitors", nil)
		req.Header.Set("Authorization", "Bearer "+memberKeySecret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("key call to monitors: want 200, got %d", resp.StatusCode)
		}
	})

	// --- a key with role=member cannot create another key (manage_api_keys is admin+) ---
	t.Run("member_key_cannot_manage_keys_403", func(t *testing.T) {
		body := `{"name":"nope","role":"member"}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/orgs/"+orgID+"/api-keys", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+memberKeySecret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("member-key create: want 403, got %d", resp.StatusCode)
		}
	})

	// --- revoke makes the same key fail auth immediately (401) ---
	t.Run("revoke_kills_the_key", func(t *testing.T) {
		keys := listKeys(t, ownerClient, orgID)
		var memberKeyID string
		for _, k := range keys {
			if k.Name == "ci key" {
				memberKeyID = k.ID
			}
		}
		if memberKeyID == "" {
			t.Fatalf("could not find the member key to revoke: %+v", keys)
		}
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/orgs/"+orgID+"/api-keys/"+memberKeyID, nil)
		resp, err := ownerClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("revoke: want 204, got %d", resp.StatusCode)
		}

		// the revoked key now fails the public-API call.
		req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/orgs/"+orgID+"/monitors", nil)
		req2.Header.Set("Authorization", "Bearer "+memberKeySecret)
		resp2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("revoked key call: want 401, got %d", resp2.StatusCode)
		}
	})

	// --- role=owner create is rejected (keys are never owner-equivalent) ---
	t.Run("owner_role_key_rejected", func(t *testing.T) {
		resp := createKey(t, ownerClient, orgID, "owner key", "owner")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("owner-role key: want 422, got %d", resp.StatusCode)
		}
	})

	// --- a member (human) cannot create a key (403) ---
	t.Run("member_human_cannot_create_403", func(t *testing.T) {
		// owner invites a member; the member accepts; then the member tries to create a key.
		body := `{"email":"member@example.com","role":"member"}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/orgs/"+orgID+"/invitations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		inv, err := ownerClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = inv.Body.Close()
		if inv.StatusCode != http.StatusCreated && inv.StatusCode != http.StatusOK {
			t.Fatalf("invite member: want 200/201, got %d", inv.StatusCode)
		}
		token := mailer.tokenFor(t, "member@example.com")

		memberClient, _ := login(t, "member-sub", "member@example.com")
		acc, err := memberClient.Post(ts.URL+"/api/v1/invitations/"+token+"/accept", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = acc.Body.Close()
		if acc.StatusCode < 200 || acc.StatusCode >= 300 {
			t.Fatalf("accept: want 2xx, got %d", acc.StatusCode)
		}

		resp := createKey(t, memberClient, orgID, "member tries", "member")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("member-human create: want 403, got %d", resp.StatusCode)
		}
	})

	// --- docs routes are served without auth ---
	t.Run("docs_routes_unauthenticated", func(t *testing.T) {
		docs, err := http.Get(ts.URL + "/api/docs")
		if err != nil {
			t.Fatal(err)
		}
		defer docs.Body.Close()
		if docs.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/docs: want 200, got %d", docs.StatusCode)
		}
		if ct := docs.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("GET /api/docs content-type = %q, want text/html", ct)
		}

		spec, err := http.Get(ts.URL + "/api/openapi.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer spec.Body.Close()
		if spec.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/openapi.yaml: want 200, got %d", spec.StatusCode)
		}
		raw, _ := io.ReadAll(spec.Body)
		if !strings.Contains(string(raw), "openapi") || !strings.Contains(string(raw), "api-keys") {
			t.Fatalf("openapi.yaml does not look like the spec: %.200s", raw)
		}
	})
}

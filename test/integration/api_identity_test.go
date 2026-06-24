//go:build integration

// Identity HTTP API integration test. It exercises the REAL handlers in
// internal/api wired to the REAL authn services and the REAL Postgres store
// (testcontainers, so RLS is in force), with an in-memory flow/role cache and a
// FAKE Google OIDC IdP. It drives the full edge:
//
//   - login -> callback sets session cookies -> GET /api/v1/me returns the created
//     user and their personal org with the owner role;
//   - PATCH /api/v1/me updates the locale;
//   - POST /auth/refresh rotates the refresh cookie and issues a fresh access cookie;
//   - POST /auth/logout clears the cookies and revokes the family;
//   - GET /.well-known/jwks.json serves the signing kid;
//   - POST /api/v1/orgs creates an org with the caller as owner, listed by GET /orgs;
//   - an unauthenticated GET /api/v1/me is 401;
//   - GET /api/v1/orgs/{orgId} for a non-member is 403.
//
// Run with: go test -tags integration ./test/integration/
package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/api"
	"pulse/internal/authn"
	"pulse/internal/store"
)

func TestAPIIdentity(t *testing.T) {
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

	// Fake Google OIDC IdP, configured to return a verified profile.
	idp := newFakeIdP(t)
	idp.sub = "google-sub-itest"
	idp.email = "itest@example.com"
	idp.name = "Integration Test"

	// REAL google provider against the fake IdP.
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

	srv := api.New(api.Config{
		Store:      app,
		Login:      loginSvc,
		JWT:        jwtIssuer,
		Refresh:    refreshSvc,
		Cookies:    authn.CookieConfig{Secure: false},
		Auth:       auth,
		AppBaseURL: "", // redirect to relative paths so the test can read Location
	})

	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	// A client that does NOT follow redirects, so the test can inspect each hop.
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// --- unauthenticated /me is 401 ---
	t.Run("unauthenticated_me_is_401", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/me")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	})

	// --- jwks serves the kid ---
	t.Run("jwks_serves_kid", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/.well-known/jwks.json")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var doc struct {
			Keys []struct {
				Kid string `json:"kid"`
				Kty string `json:"kty"`
			} `json:"keys"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Keys) != 1 || doc.Keys[0].Kid != "itest-kid-1" || doc.Keys[0].Kty != "RSA" {
			t.Fatalf("jwks did not serve the kid: %+v", doc.Keys)
		}
	})

	// --- full login -> callback -> /me ---
	t.Run("login_callback_me", func(t *testing.T) {
		doLogin(t, client, ts, idp)

		// GET /me returns the created user + personal org with owner role.
		me := getMe(t, client, ts)
		if me.Email != "itest@example.com" {
			t.Fatalf("me email = %q", me.Email)
		}
		if len(me.Orgs) != 1 || me.Orgs[0].Role != "owner" {
			t.Fatalf("expected one owner org, got %+v", me.Orgs)
		}
		if me.Locale != "en" {
			t.Fatalf("default locale = %q, want en", me.Locale)
		}
	})

	// --- PATCH /me updates locale ---
	t.Run("patch_me_updates_locale", func(t *testing.T) {
		body := `{"locale":"de","timezone":"Europe/Berlin"}`
		req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/me", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("patch me: want 200, got %d", resp.StatusCode)
		}
		var me meDTO
		_ = json.NewDecoder(resp.Body).Decode(&me)
		if me.Locale != "de" || me.Timezone != "Europe/Berlin" {
			t.Fatalf("patch did not update profile: %+v", me)
		}
	})

	// --- refresh rotates ---
	t.Run("refresh_rotates", func(t *testing.T) {
		before := cookieValue(jar, ts, authn.RefreshCookie)
		if before == "" {
			t.Fatal("no refresh cookie before refresh")
		}
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/refresh", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("refresh: want 204, got %d", resp.StatusCode)
		}
		after := cookieValue(jar, ts, authn.RefreshCookie)
		if after == "" || after == before {
			t.Fatalf("refresh did not rotate the refresh cookie: before=%q after=%q", before, after)
		}
		// the new access cookie still authenticates /me
		_ = getMe(t, client, ts)
	})

	// --- create org + list orgs ---
	t.Run("create_org_and_list", func(t *testing.T) {
		body := `{"name":"Acme Co"}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/orgs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create org: want 201, got %d", resp.StatusCode)
		}
		var created orgDTO
		_ = json.NewDecoder(resp.Body).Decode(&created)
		if created.Role != "owner" || created.Name != "Acme Co" {
			t.Fatalf("created org wrong: %+v", created)
		}

		// it appears in GET /orgs alongside the personal org
		listResp, err := client.Get(ts.URL + "/api/v1/orgs")
		if err != nil {
			t.Fatal(err)
		}
		defer listResp.Body.Close()
		var orgs []orgDTO
		_ = json.NewDecoder(listResp.Body).Decode(&orgs)
		found := false
		for _, o := range orgs {
			if o.OrgID == created.OrgID {
				found = true
			}
		}
		if !found || len(orgs) < 2 {
			t.Fatalf("created org not listed: %+v", orgs)
		}

		// GET /orgs/{orgId} for a member returns it
		one, err := client.Get(ts.URL + "/api/v1/orgs/" + created.OrgID)
		if err != nil {
			t.Fatal(err)
		}
		defer one.Body.Close()
		if one.StatusCode != http.StatusOK {
			t.Fatalf("get own org: want 200, got %d", one.StatusCode)
		}
	})

	// --- non-member GET /orgs/{orgId} is 403 ---
	t.Run("non_member_org_is_403", func(t *testing.T) {
		// create an org owned by a different, freshly seeded user
		otherOrg, _, err := app.CreateOrgWithOwner(ctx, "Strangers", "", mkUser(ctx, t, app, "stranger@example.com"))
		if err != nil {
			t.Fatalf("seed stranger org: %v", err)
		}
		resp, err := client.Get(fmt.Sprintf("%s/api/v1/orgs/%d", ts.URL, otherOrg.ID))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("non-member org: want 403, got %d", resp.StatusCode)
		}
	})

	// --- logout clears + revokes ---
	t.Run("logout_clears_and_revokes", func(t *testing.T) {
		rt := cookieValue(jar, ts, authn.RefreshCookie)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/logout", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("logout: want 204, got %d", resp.StatusCode)
		}
		// the refresh family is revoked: rotating the old token now fails
		if _, rerr := refreshSvc.Rotate(ctx, rt); rerr == nil {
			t.Fatal("refresh token should be revoked after logout")
		}
		// the access cookie is cleared, so /me is 401 again
		me, err := client.Get(ts.URL + "/api/v1/me")
		if err != nil {
			t.Fatal(err)
		}
		defer me.Body.Close()
		if me.StatusCode != http.StatusUnauthorized {
			t.Fatalf("after logout /me: want 401, got %d", me.StatusCode)
		}
	})
}

// --- helpers ---

type meDTO struct {
	UserID   string   `json:"user_id"`
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	Locale   string   `json:"locale"`
	Timezone string   `json:"timezone"`
	Orgs     []orgDTO `json:"orgs"`
}

type orgDTO struct {
	OrgID string `json:"org_id"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
	Role  string `json:"role"`
	Plan  string `json:"plan"`
}

// doLogin drives the login redirect, hits the IdP authorize so it records the
// nonce, then calls the callback with the matching state, leaving the session
// cookies in the client's jar.
func doLogin(t *testing.T, client *http.Client, ts *httptest.Server, idp *fakeIdP) {
	t.Helper()
	// 1. GET /auth/google/login -> 302 to the IdP, sets the state cookie.
	resp, err := client.Get(ts.URL + "/auth/google/login?return_to=/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: want 302, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	state := cookieValue(client.Jar, ts, authn.OAuthStateCookie)
	if state == "" {
		t.Fatal("login did not set the oauth state cookie")
	}

	// 2. Hit the IdP authorize so the fake records the nonce it must echo.
	u, _ := url.Parse(loc)
	ar, err := http.Get(idp.server.URL + "/authorize?" + u.RawQuery)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = ar.Body.Close()

	// 3. GET the callback with the matching state + a code. The state cookie rides
	// from the jar; the access/refresh cookies come back on the response.
	cb, err := client.Get(ts.URL + "/auth/google/callback?state=" + url.QueryEscape(state) + "&code=any-code")
	if err != nil {
		t.Fatal(err)
	}
	_ = cb.Body.Close()
	if cb.StatusCode != http.StatusFound {
		t.Fatalf("callback: want 302, got %d (loc=%q)", cb.StatusCode, cb.Header.Get("Location"))
	}
	if cookieValue(client.Jar, ts, authn.AccessCookie) == "" {
		t.Fatal("callback did not set the access cookie")
	}
}

func getMe(t *testing.T, client *http.Client, ts *httptest.Server) meDTO {
	t.Helper()
	resp, err := client.Get(ts.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get me: want 200, got %d", resp.StatusCode)
	}
	var me meDTO
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	return me
}

// cookieValue reads a cookie from the jar. The refresh and oauth-state cookies are
// path-scoped to /auth, so the lookup checks the jar at both / and /auth.
func cookieValue(jar http.CookieJar, ts *httptest.Server, name string) string {
	for _, path := range []string{"/", "/auth/refresh"} {
		u, _ := url.Parse(ts.URL + path)
		for _, c := range jar.Cookies(u) {
			if c.Name == name {
				return c.Value
			}
		}
	}
	return ""
}

// --- in-memory cache implementing the authn flow/role/key cache interface ---

type memCache struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemCache() *memCache { return &memCache{m: map[string]string{}} }

func (c *memCache) GetCache(_ context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	return v, ok, nil
}

func (c *memCache) SetCache(_ context.Context, key, value string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = value
	return nil
}

func (c *memCache) DelCache(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
	return nil
}

// GetDelCache reads and removes a key in one step, the single-use consume the
// magic-link verify relies on (mirrors *kv.Client's GETDEL-backed helper).
func (c *memCache) GetDelCache(_ context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	if ok {
		delete(c.m, key)
	}
	return v, ok, nil
}

// SetIfAbsent mimics Redis SET NX: it sets the key only if absent and reports
// whether it was newly set. Used by the notifier dedup test as the Redis fast path.
func (c *memCache) SetIfAbsent(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; ok {
		return false, nil
	}
	c.m[key] = value
	return true, nil
}

// --- fake Google OIDC IdP (mirrors authn/login_test.go's fakeOIDC) ---

type fakeIdP struct {
	server        *httptest.Server
	priv          *rsa.PrivateKey
	kid           string
	sub           string
	email         string
	name          string
	emailVerified bool
	lastNonce     string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{priv: priv, kid: "fake-oidc-itest", emailVerified: true}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		base := f.server.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"jwks_uri":                              base + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub := f.priv.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": f.kid,
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.lastNonce = r.URL.Query().Get("nonce")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		idToken := f.mintIDToken(t, "fake-client", f.lastNonce)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIdP) mintIDToken(t *testing.T, aud, nonce string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":            f.server.URL,
		"aud":            aud,
		"sub":            f.sub,
		"email":          f.email,
		"email_verified": f.emailVerified,
		"name":           f.name,
		"nonce":          nonce,
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	signed, err := tok.SignedString(f.priv)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

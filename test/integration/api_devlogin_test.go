//go:build integration

// Dev-login HTTP API integration test. It exercises the REAL POST /auth/dev/login
// handler in internal/api wired to the REAL authn services and the REAL Postgres
// store (testcontainers, so RLS is in force), with NO OAuth provider configured.
// It proves:
//
//   - with dev-login on: POST /auth/dev/login {email} is 2xx and sets the access
//     cookie; GET /api/v1/me then returns the real user with their personal org
//     (owner) read FROM POSTGRES;
//   - a second dev-login with the same email returns the same user (no duplicate);
//   - the created user persists in the store;
//   - with dev-login off: the route is 404;
//   - the api boots (builds + serves) with no OAuth provider when dev-login is on.
//
// Run with: go test -tags integration ./test/integration/
package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/api"
	"pulse/internal/authn"
	"pulse/internal/store"
)

func TestAPIDevLogin(t *testing.T) {
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

	cache := newMemCache()
	signing, err := authn.GenerateSigningKey("devlogin-kid-1")
	if err != nil {
		t.Fatalf("gen signing key: %v", err)
	}
	jwtIssuer := authn.NewJWTIssuer("pulse", "pulse-api", signing)
	// NO OAuth providers configured: dev-login does not need any.
	loginSvc := authn.NewLoginService(nil, cache, app)
	refreshSvc := authn.NewRefreshService(app)
	keyVerifier := authn.NewAPIKeyVerifier(app, cache)
	auth := authn.NewAuthenticator(jwtIssuer, keyVerifier, app, cache)

	mkServer := func(devLogin bool) *httptest.Server {
		srv := api.New(api.Config{
			Store:      app,
			Login:      loginSvc,
			JWT:        jwtIssuer,
			Refresh:    refreshSvc,
			Cookies:    authn.CookieConfig{Secure: false},
			Auth:       auth,
			AppBaseURL: "",
			DevLogin:   devLogin,
		})
		return httptest.NewServer(srv.Router())
	}

	// --- dev-login ON: the route signs in and bootstraps /me ---
	t.Run("dev_login_enabled", func(t *testing.T) {
		ts := mkServer(true)
		defer ts.Close()

		jar, _ := cookiejar.New(nil)
		client := &http.Client{
			Jar: jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}

		const email = "dev.login@example.com"

		// POST /auth/dev/login -> 204, sets the access cookie.
		resp, err := client.Post(ts.URL+"/auth/dev/login", "application/json",
			strings.NewReader(`{"email":"`+email+`","name":"Dev Login"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("dev login: want 204, got %d", resp.StatusCode)
		}
		if cookieValue(client.Jar, ts, authn.AccessCookie) == "" {
			t.Fatal("dev login did not set the access cookie")
		}

		// GET /api/v1/me returns the real user + personal org (owner) from Postgres.
		me := getMe(t, client, ts)
		if me.Email != email {
			t.Fatalf("me email = %q, want %q", me.Email, email)
		}
		if me.Name != "Dev Login" {
			t.Fatalf("me name = %q, want %q", me.Name, "Dev Login")
		}
		if len(me.Orgs) != 1 || me.Orgs[0].Role != "owner" {
			t.Fatalf("expected one owner org, got %+v", me.Orgs)
		}
		firstUserID := me.UserID

		// The user really exists in Postgres.
		u, err := app.GetUserByEmail(ctx, email)
		if err != nil {
			t.Fatalf("created user should persist: %v", err)
		}
		if fmt.Sprintf("%d", u.ID) != firstUserID {
			t.Fatalf("store user id %d != me user id %s", u.ID, firstUserID)
		}

		// A second dev-login with the same email returns the same user (no duplicate).
		jar2, _ := cookiejar.New(nil)
		client2 := &http.Client{
			Jar: jar2,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp2, err := client2.Post(ts.URL+"/auth/dev/login", "application/json",
			strings.NewReader(`{"email":"`+email+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp2.Body.Close()
		if resp2.StatusCode != http.StatusNoContent {
			t.Fatalf("second dev login: want 204, got %d", resp2.StatusCode)
		}
		me2 := getMe(t, client2, ts)
		if me2.UserID != firstUserID {
			t.Fatalf("second dev login made a different user: %s != %s", me2.UserID, firstUserID)
		}
		if len(me2.Orgs) != 1 {
			t.Fatalf("second dev login should reuse the same single personal org, got %+v", me2.Orgs)
		}
	})

	// --- bad input is a validation envelope, not a session ---
	t.Run("dev_login_bad_email_is_422", func(t *testing.T) {
		ts := mkServer(true)
		defer ts.Close()
		resp, err := http.Post(ts.URL+"/auth/dev/login", "application/json",
			strings.NewReader(`{"email":"not-an-email"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("bad email: want 422, got %d", resp.StatusCode)
		}
	})

	// --- dev-login OFF: the route is not registered, so it never signs anyone in ---
	// The stdlib mux replies 405 (not 404) here because the path /auth/dev/login still
	// overlaps the GET /auth/{provider}/login pattern, so a POST finds a path match but
	// no method match. Either way the dev-login handler does not run: no 2xx, no session
	// cookie, no user created. That is the production-safety guarantee.
	t.Run("dev_login_disabled_does_not_sign_in", func(t *testing.T) {
		ts := mkServer(false)
		defer ts.Close()
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := client.Post(ts.URL+"/auth/dev/login", "application/json",
			strings.NewReader(`{"email":"shouldnotexist@example.com"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
			t.Fatalf("dev login disabled should not sign in, got %d", resp.StatusCode)
		}
		if cookieValue(client.Jar, ts, authn.AccessCookie) != "" {
			t.Fatal("dev login disabled must not set a session cookie")
		}
		if _, err := app.GetUserByEmail(ctx, "shouldnotexist@example.com"); err == nil {
			t.Fatal("dev login disabled must not create a user")
		}
	})
}

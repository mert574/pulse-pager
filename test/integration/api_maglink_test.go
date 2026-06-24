//go:build integration

// Magic-link (passwordless) email login HTTP API integration test. It exercises
// the REAL POST /auth/email/start and GET /auth/email/verify handlers in
// internal/api wired to the REAL authn MagicLinkService and the REAL Postgres
// store (testcontainers, so RLS is in force), with NO OAuth provider configured.
// It proves:
//
//   - POST /auth/email/start {email} returns the neutral 200 and emails a link;
//     following GET /auth/email/verify?token=... sets the session cookies and
//     GET /api/v1/me then returns the real user with their personal org (owner)
//     read FROM POSTGRES;
//   - the start response is the SAME neutral body for an unknown email (the
//     handler never reveals whether the email exists);
//   - a second verify of the same token fails (single-use), so it does not sign in.
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
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/api"
	"pulse/internal/authn"
	"pulse/internal/notify"
	"pulse/internal/store"
)

// recordingMailer captures the last sent mail so the test can pull the verify link
// out of the body (the LogMailer logs it; here we read it directly).
type recordingMailer struct {
	mu   sync.Mutex
	last notify.Mail
}

func (m *recordingMailer) Send(_ context.Context, msg notify.Mail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = msg
	return nil
}

func (m *recordingMailer) body() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last.Body
}

func TestAPIMagicLink(t *testing.T) {
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
	signing, err := authn.GenerateSigningKey("maglink-kid-1")
	if err != nil {
		t.Fatalf("gen signing key: %v", err)
	}
	jwtIssuer := authn.NewJWTIssuer("pulse", "pulse-api", signing)
	// NO OAuth providers configured: magic-link does not need any.
	loginSvc := authn.NewLoginService(nil, cache, app)
	refreshSvc := authn.NewRefreshService(app)
	keyVerifier := authn.NewAPIKeyVerifier(app, cache)
	auth := authn.NewAuthenticator(jwtIssuer, keyVerifier, app, cache)
	magicSvc := authn.NewMagicLinkService(cache, app)
	mailer := &recordingMailer{}

	srv := api.New(api.Config{
		Store:      app,
		Login:      loginSvc,
		JWT:        jwtIssuer,
		Refresh:    refreshSvc,
		Cookies:    authn.CookieConfig{Secure: false},
		Auth:       auth,
		AppBaseURL: "", // verify link is then a bare /auth/email/verify path, hit on this server
		Magic:      magicSvc,
		Mailer:     mailer,
		// Redis left nil: this test does not exercise the rate-limit counters.
	})
	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

	// verifyPath pulls the /auth/email/verify?token=... line out of the email body.
	verifyPath := func() string {
		for _, line := range strings.Split(mailer.body(), "\n") {
			if strings.Contains(line, "/auth/email/verify?token=") {
				return strings.TrimSpace(line)
			}
		}
		return ""
	}

	const email = "magic.link@example.com"

	// --- start + verify signs the user in and bootstraps /me ---
	t.Run("start_then_verify_signs_in", func(t *testing.T) {
		jar, _ := cookiejar.New(nil)
		client := &http.Client{
			Jar: jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}

		// POST /auth/email/start -> neutral 200, emails a link.
		resp, err := client.Post(ts.URL+"/auth/email/start", "application/json",
			strings.NewReader(`{"email":"`+email+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("email start: want 200, got %d", resp.StatusCode)
		}

		link := verifyPath()
		if link == "" {
			t.Fatal("no verify link was emailed")
		}

		// GET the verify link -> 302 into the app, sets the session cookies.
		vresp, err := client.Get(ts.URL + link)
		if err != nil {
			t.Fatal(err)
		}
		_ = vresp.Body.Close()
		if vresp.StatusCode != http.StatusFound {
			t.Fatalf("email verify: want 302, got %d", vresp.StatusCode)
		}
		if loc := vresp.Header.Get("Location"); strings.Contains(loc, "error=") {
			t.Fatalf("email verify failed, redirected to %q", loc)
		}
		if cookieValue(client.Jar, ts, authn.AccessCookie) == "" {
			t.Fatal("email verify did not set the access cookie")
		}

		// GET /api/v1/me returns the real user + personal org (owner) from Postgres.
		me := getMe(t, client, ts)
		if me.Email != email {
			t.Fatalf("me email = %q, want %q", me.Email, email)
		}
		if len(me.Orgs) != 1 || me.Orgs[0].Role != "owner" {
			t.Fatalf("expected one owner org, got %+v", me.Orgs)
		}

		// The user really exists in Postgres.
		if _, err := app.GetUserByEmail(ctx, email); err != nil {
			t.Fatalf("created user should persist: %v", err)
		}

		// A second verify of the same token must fail (single-use): no redirect into
		// the app, sent to the login-failed page instead, and no session minted.
		jar2, _ := cookiejar.New(nil)
		client2 := &http.Client{Jar: jar2, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		v2, err := client2.Get(ts.URL + link)
		if err != nil {
			t.Fatal(err)
		}
		_ = v2.Body.Close()
		if cookieValue(client2.Jar, ts, authn.AccessCookie) != "" {
			t.Fatal("a replayed verify must not set a session cookie")
		}
		if loc := v2.Header.Get("Location"); !strings.Contains(loc, "error=auth_failed") {
			t.Fatalf("a replayed verify should redirect to the login-failed page, got %q", loc)
		}
	})

	// --- unknown email gets the SAME neutral response (enumeration-safe) ---
	t.Run("unknown_email_is_neutral", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/auth/email/start", "application/json",
			strings.NewReader(`{"email":"nobody.here@example.com"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("unknown email start: want the same neutral 200, got %d", resp.StatusCode)
		}
		// No account was created for the unknown email (start never touches the store).
		if _, err := app.GetUserByEmail(ctx, "nobody.here@example.com"); err == nil {
			t.Fatal("starting a link for an unknown email must not create a user")
		}
	})

	// --- bad input is a validation envelope, not a neutral success ---
	t.Run("bad_email_is_422", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/auth/email/start", "application/json",
			strings.NewReader(`{"email":"not-an-email"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("bad email: want 422, got %d", resp.StatusCode)
		}
	})
}

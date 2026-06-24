//go:build integration

// Members + invitations HTTP API integration test. It exercises the REAL handlers
// in internal/api wired to the REAL authn services and the REAL Postgres store
// (testcontainers, RLS in force), a FAKE Google OIDC IdP, and a capturing Mailer
// so the tokenized invite link can be read and accepted. It reuses the helpers
// from api_identity_test.go (doLogin, getMe, cookieValue, newFakeIdP, newMemCache,
// mkUser, meDTO/orgDTO) which live in this same package.
//
// Covers: invite -> accept (matching email) creates the membership; accept with a
// mismatched email is rejected; a member cannot invite (403); unauthenticated
// invite is 401; the last owner cannot leave (409); listing members; and the
// per-plan seat cap blocks an over-seat invite.
//
// Run with: go test -tags integration ./test/integration/
package integration

import (
	"context"
	"encoding/json"
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
	"pulse/internal/crypto"
	"pulse/internal/entitlements"
	"pulse/internal/events"
	"pulse/internal/store"
)

// captureEmail records the email intents the api publishes (RFC-019). The api no
// longer sends mail or mints tokens; it publishes a semantic intent and the notifier
// is the only sender. These tests act as the notifier: for an invitation, tokenFor
// mints a known token on the (still-pending) row and returns the raw value so the test
// can accept with it, exactly as the notifier's SetInvitationToken would. It is shared
// by every api integration test that needs to accept an invite for setup.
type captureEmail struct {
	mu    sync.Mutex
	store *store.Pool
	all   []events.EmailIntent
}

func (c *captureEmail) PublishEmail(_ context.Context, _ string, in events.EmailIntent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.all = append(c.all, in)
	return nil
}

// tokenFor acts as the notifier for the most recent invitation intent to email: it
// mints a known token hash on the still-pending row and returns the raw token, so the
// caller can drive the accept flow. A missing intent is a fatal test error.
func (c *captureEmail) tokenFor(t *testing.T, email string) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.all) - 1; i >= 0; i-- {
		in := c.all[i]
		if in.Type != events.EmailInvitation || in.Invitation == nil || in.Invitation.Email != email {
			continue
		}
		raw := fmt.Sprintf("itest-invite-%d", in.Invitation.InvitationID)
		if _, err := c.store.SetInvitationToken(context.Background(), in.Invitation.OrgID, in.Invitation.InvitationID, crypto.HashToken(raw)); err != nil {
			t.Fatalf("mint invite token for %s: %v", email, err)
		}
		return raw
	}
	t.Fatalf("no invite intent captured for %s", email)
	return ""
}

// magicRequested reports whether a magic-link intent was published for email (the api's
// job now; the notifier does the minting and sending).
func (c *captureEmail) magicRequested(email string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, in := range c.all {
		if in.Type == events.EmailMagicLink && in.MagicLink != nil && in.MagicLink.Email == email {
			return true
		}
	}
	return false
}

func TestAPIMembers(t *testing.T) {
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
	emailPub := &captureEmail{store: app}

	mkServer := func(cap int) *httptest.Server {
		srv := api.New(api.Config{
			Store:      app,
			Login:      loginSvc,
			JWT:        jwtIssuer,
			Refresh:    refreshSvc,
			Cookies:    authn.CookieConfig{Secure: false},
			Auth:       auth,
			AppBaseURL: "http://app.test",
			Seats:      entitlements.FixedSeats{Cap: cap},
			Email:      emailPub,
		})
		return httptest.NewServer(srv.Router())
	}

	// Main server has generous seats so invite + accept + a second invite all fit.
	ts := mkServer(5)
	defer ts.Close()

	newClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}

	// login a user with a specific verified email, returns their authed client + me.
	login := func(t *testing.T, sub, email string) (*http.Client, meDTO) {
		t.Helper()
		idp.sub = sub
		idp.email = email
		idp.name = email
		c := newClient()
		doLogin(t, c, ts, idp)
		return c, getMe(t, c, ts)
	}

	invite := func(t *testing.T, c *http.Client, orgID, email, role string) *http.Response {
		t.Helper()
		body := fmt.Sprintf(`{"email":%q,"role":%q}`, email, role)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/orgs/"+orgID+"/invitations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Owner logs in and owns a personal org.
	ownerClient, ownerMe := login(t, "owner-sub", "owner@example.com")
	if len(ownerMe.Orgs) != 1 || ownerMe.Orgs[0].Role != "owner" {
		t.Fatalf("owner setup wrong: %+v", ownerMe.Orgs)
	}
	orgID := ownerMe.Orgs[0].OrgID

	// --- unauthenticated invite is 401 ---
	t.Run("unauthenticated_invite_401", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/api/v1/orgs/"+orgID+"/invitations", "application/json",
			strings.NewReader(`{"email":"x@example.com","role":"member"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	})

	// --- owner invites; invitee with the matching email accepts ---
	t.Run("invite_then_accept", func(t *testing.T) {
		resp := invite(t, ownerClient, orgID, "invitee@example.com", "member")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("invite: want 200/201, got %d", resp.StatusCode)
		}
		token := emailPub.tokenFor(t, "invitee@example.com")

		inviteeClient, _ := login(t, "invitee-sub", "invitee@example.com")
		acc, err := inviteeClient.Post(ts.URL+"/api/v1/invitations/"+token+"/accept", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer acc.Body.Close()
		if acc.StatusCode < 200 || acc.StatusCode >= 300 {
			t.Fatalf("accept: want 2xx, got %d", acc.StatusCode)
		}

		// the owner's members list now has 2 members incl. the invitee as member.
		members := listMembers(t, ownerClient, ts, orgID)
		if len(members) != 2 {
			t.Fatalf("want 2 members, got %d: %+v", len(members), members)
		}
		var sawInvitee bool
		for _, m := range members {
			if m.Email == "invitee@example.com" {
				sawInvitee = true
				if m.Role != "member" {
					t.Fatalf("invitee role = %q, want member", m.Role)
				}
			}
		}
		if !sawInvitee {
			t.Fatalf("invitee not in members: %+v", members)
		}
	})

	// --- a member cannot invite (403) ---
	t.Run("member_cannot_invite_403", func(t *testing.T) {
		inviteeClient, _ := login(t, "invitee-sub", "invitee@example.com")
		resp := invite(t, inviteeClient, orgID, "another@example.com", "member")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("member invite: want 403, got %d", resp.StatusCode)
		}
	})

	// --- accept with a mismatched signed-in email is rejected ---
	t.Run("accept_email_mismatch_rejected", func(t *testing.T) {
		resp := invite(t, ownerClient, orgID, "stranger@example.com", "member")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("invite stranger: want 200/201, got %d", resp.StatusCode)
		}
		token := emailPub.tokenFor(t, "stranger@example.com")

		// invitee@ (a different verified email) tries to accept the stranger invite.
		inviteeClient, _ := login(t, "invitee-sub", "invitee@example.com")
		acc, err := inviteeClient.Post(ts.URL+"/api/v1/invitations/"+token+"/accept", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer acc.Body.Close()
		if acc.StatusCode < 400 {
			t.Fatalf("mismatched accept: want a 4xx, got %d", acc.StatusCode)
		}
	})

	// --- the last owner cannot leave (409) ---
	t.Run("last_owner_cannot_leave_409", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/orgs/"+orgID+"/members/me", nil)
		resp, err := ownerClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("last owner leave: want 409, got %d", resp.StatusCode)
		}
	})

	// --- the per-plan seat cap blocks an over-seat invite ---
	t.Run("seat_cap_blocks_invite", func(t *testing.T) {
		tight := mkServer(1)
		defer tight.Close()

		idp.sub = "tight-owner-sub"
		idp.email = "tight-owner@example.com"
		idp.name = "Tight Owner"
		c := newClient()
		doLogin(t, c, tight, idp)
		me := getMe(t, c, tight)
		tightOrg := me.Orgs[0].OrgID

		// the owner already occupies the only seat (cap 1), so an invite is blocked.
		body := `{"email":"nope@example.com","role":"member"}`
		req, _ := http.NewRequest(http.MethodPost, tight.URL+"/api/v1/orgs/"+tightOrg+"/invitations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("over-seat invite: want a 4xx, got %d", resp.StatusCode)
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		if !strings.Contains(strings.ToLower(env.Error.Code), "seat") {
			t.Fatalf("over-seat invite error code = %q, want a seat-limit code", env.Error.Code)
		}
	})
}

type memberDTO struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
}

func listMembers(t *testing.T, c *http.Client, ts *httptest.Server, orgID string) []memberDTO {
	t.Helper()
	resp, err := c.Get(ts.URL + "/api/v1/orgs/" + orgID + "/members")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list members: want 200, got %d", resp.StatusCode)
	}
	var out []memberDTO
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode members: %v", err)
	}
	return out
}

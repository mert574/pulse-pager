//go:build integration

// Entitlements + plan-catalog HTTP API integration test. It drives the REAL handlers
// in internal/api wired to the REAL authn services and a REAL Postgres store
// (testcontainers, so RLS is in force), reusing the login helpers from
// api_identity_test.go. It covers:
//
//   - GET /orgs/{orgId}/entitlements returns the org's plan + usage vs caps:
//     create N monitors -> monitors_used = N; invite a member -> seats_used reflects
//     accepted + pending; create a status page -> status_pages_used increments; the
//     caps match the resolver and the plan floors (retention, min interval);
//   - authz: owner reads; admin reads; member is 403; viewer is 403; a non-member is
//     403; unauthenticated is 401;
//   - GET /plans returns the four tiers with their canonical caps.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
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

type entitlementsDTO struct {
	Plan                 string   `json:"plan"`
	MonitorsUsed         int      `json:"monitors_used"`
	MonitorsCap          int      `json:"monitors_cap"`
	SeatsUsed            int      `json:"seats_used"`
	SeatsCap             int      `json:"seats_cap"`
	StatusPagesUsed      int      `json:"status_pages_used"`
	StatusPagesCap       int      `json:"status_pages_cap"`
	MinIntervalSeconds   int      `json:"min_interval_seconds"`
	RetentionDays        int      `json:"retention_days"`
	RegionsAllowed       []string `json:"regions_allowed"`
	RegionsPerMonitorCap int      `json:"regions_per_monitor_cap"`
	CustomDomainAllowed  bool     `json:"custom_domain_allowed"`
	ApiWriteAllowed      bool     `json:"api_write_allowed"`
	FailureSnapshot      bool     `json:"failure_snapshot"`
	TrialEligible        bool     `json:"trial_eligible"`
}

type planEntryDTO struct {
	Plan               string `json:"plan"`
	MonitorsCap        int    `json:"monitors_cap"`
	MinIntervalSeconds int    `json:"min_interval_seconds"`
	SeatsCap           int    `json:"seats_cap"`
	StatusPagesCap     int    `json:"status_pages_cap"`
	RetentionDays      int    `json:"retention_days"`
	CustomDomain       bool   `json:"custom_domain_allowed"`
	ApiWriteAllowed    bool   `json:"api_write_allowed"`
	ApiRatePerMin      int    `json:"api_rate_per_min"`
}

func TestAPIEntitlements(t *testing.T) {
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

	// Generous resolvers so creating a few monitors, inviting a member, and creating a
	// status page are not blocked by the Free caps; the entitlements read echoes these
	// caps back, so the test asserts against them.
	monLimits := entitlements.MonitorLimits{
		MonitorsCap: 50, MinIntervalSeconds: 60,
		RegionsAllowed: []string{"eu-central", "us-west"}, RegionsPerMonitorCap: 2,
	}
	srv := api.New(api.Config{
		Store:       app,
		Login:       loginSvc,
		JWT:         jwtIssuer,
		Refresh:     refreshSvc,
		Cookies:     authn.CookieConfig{Secure: false},
		Auth:        auth,
		AppBaseURL:  "http://app.test",
		Seats:       entitlements.FixedSeats{Cap: 5},
		Monitors:    entitlements.FixedMonitors{Limits: monLimits},
		StatusPages: entitlements.FixedStatusPages{Cap: 3},
		Email:       emailPub,
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
	post := func(c *http.Client, path, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	getEnts := func(t *testing.T, c *http.Client, orgID string) (*http.Response, entitlementsDTO) {
		t.Helper()
		resp, err := c.Get(ts.URL + "/api/v1/orgs/" + orgID + "/entitlements")
		if err != nil {
			t.Fatal(err)
		}
		var e entitlementsDTO
		if resp.StatusCode == http.StatusOK {
			_ = json.NewDecoder(resp.Body).Decode(&e)
		}
		return resp, e
	}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	monitorBody := func(name string) string {
		return fmt.Sprintf(`{
			"name":%q,"url":%q,"method":"GET","headers":[],"body":"",
			"expected_status_codes":"200","timeout_seconds":5,"interval_seconds":60,
			"enabled":true,"failure_threshold":1,"notification_channel_ids":[],
			"regions":["eu-central"],"down_policy":"quorum"
		}`, name, target.URL)
	}

	ownerClient, ownerMe := login(t, "ent-owner", "ent-owner@example.com")
	orgID := ownerMe.Orgs[0].OrgID

	// --- unauthenticated is 401 ---
	t.Run("unauthenticated_401", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/orgs/" + orgID + "/entitlements")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	})

	// --- a fresh org reads as Free-plan with only the owner's seat used ---
	t.Run("fresh_org_usage", func(t *testing.T) {
		resp, e := getEnts(t, ownerClient, orgID)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		if e.Plan != "tier1" {
			t.Fatalf("plan = %q, want free", e.Plan)
		}
		if e.MonitorsUsed != 0 {
			t.Fatalf("monitors_used = %d, want 0", e.MonitorsUsed)
		}
		if e.SeatsUsed != 1 {
			t.Fatalf("seats_used = %d, want 1 (the owner)", e.SeatsUsed)
		}
		if e.StatusPagesUsed != 0 {
			t.Fatalf("status_pages_used = %d, want 0", e.StatusPagesUsed)
		}
		// caps come from the resolver, retention from the plan (Free = 7 days).
		if e.MonitorsCap != 50 || e.SeatsCap != 5 || e.StatusPagesCap != 3 {
			t.Fatalf("caps wrong: monitors=%d seats=%d pages=%d", e.MonitorsCap, e.SeatsCap, e.StatusPagesCap)
		}
		if e.RetentionDays != 7 {
			t.Fatalf("retention_days = %d, want 7 (Free)", e.RetentionDays)
		}
		if e.CustomDomainAllowed {
			t.Fatalf("custom_domain_allowed = true, want false on Free")
		}
		if e.ApiWriteAllowed {
			t.Fatalf("api_write_allowed = true, want false on Free")
		}
		// A fresh Free org with no subscription history qualifies for a trial.
		if !e.TrialEligible {
			t.Fatalf("trial_eligible = false on a fresh Free org, want true")
		}
	})

	// --- a paid plan hides the trial offer (RFC-018: paying customers get no new trial) ---
	t.Run("trial_eligible_false_on_paid_plan", func(t *testing.T) {
		if _, err := admin.Exec(ctx, "UPDATE organizations SET plan='tier2' WHERE id=$1::bigint", orgID); err != nil {
			t.Fatalf("set paid plan: %v", err)
		}
		// Restore Free so later subtests see the original caps.
		defer func() {
			if _, err := admin.Exec(ctx, "UPDATE organizations SET plan='tier1' WHERE id=$1::bigint", orgID); err != nil {
				t.Fatalf("restore plan: %v", err)
			}
		}()
		resp, e := getEnts(t, ownerClient, orgID)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		if e.Plan != "tier2" {
			t.Fatalf("plan = %q, want tier2", e.Plan)
		}
		if e.TrialEligible {
			t.Fatalf("trial_eligible = true on a paid plan, want false")
		}
	})

	// --- usage tracks the real resource tables ---
	t.Run("usage_reflects_resources", func(t *testing.T) {
		// create 3 enabled monitors.
		for i := 0; i < 3; i++ {
			resp := post(ownerClient, "/api/v1/orgs/"+orgID+"/monitors", monitorBody(fmt.Sprintf("mon-%d", i)))
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("create monitor %d: want 201, got %d (%s)", i, resp.StatusCode, readBody(resp))
			}
			_ = resp.Body.Close()
		}
		// invite a member: a pending invite reserves a seat (accepted + pending).
		invResp := post(ownerClient, "/api/v1/orgs/"+orgID+"/invitations", `{"email":"teammate@example.com","role":"member"}`)
		if invResp.StatusCode != http.StatusCreated && invResp.StatusCode != http.StatusOK {
			t.Fatalf("invite: want 200/201, got %d (%s)", invResp.StatusCode, readBody(invResp))
		}
		_ = invResp.Body.Close()
		// create a status page.
		pageBody := `{"name":"Status","slug":"ent-status","logo_url":"","accent_color":"#4f46e5","theme":"light","display_monitors":[]}`
		spResp := post(ownerClient, "/api/v1/orgs/"+orgID+"/status-pages", pageBody)
		if spResp.StatusCode != http.StatusCreated {
			t.Fatalf("create status page: want 201, got %d (%s)", spResp.StatusCode, readBody(spResp))
		}
		_ = spResp.Body.Close()

		resp, e := getEnts(t, ownerClient, orgID)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("want 200, got %d", resp.StatusCode)
		}
		if e.MonitorsUsed != 3 {
			t.Fatalf("monitors_used = %d, want 3", e.MonitorsUsed)
		}
		// owner seat + 1 reserved pending invite = 2.
		if e.SeatsUsed != 2 {
			t.Fatalf("seats_used = %d, want 2 (owner + pending invite)", e.SeatsUsed)
		}
		if e.StatusPagesUsed != 1 {
			t.Fatalf("status_pages_used = %d, want 1", e.StatusPagesUsed)
		}
	})

	// --- a member is 403; an admin reads ---
	t.Run("authz_member_403_admin_reads", func(t *testing.T) {
		// the teammate accepts the invite and becomes a member.
		token := emailPub.tokenFor(t, "teammate@example.com")
		memberClient, _ := login(t, "ent-teammate", "teammate@example.com")
		acc, err := memberClient.Post(ts.URL+"/api/v1/invitations/"+token+"/accept", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = acc.Body.Close()
		if acc.StatusCode < 200 || acc.StatusCode >= 300 {
			t.Fatalf("accept: want 2xx, got %d", acc.StatusCode)
		}

		// a member cannot view billing/usage (PRD-006 9).
		resp, _ := getEnts(t, memberClient, orgID)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("member read: want 403, got %d", resp.StatusCode)
		}

		// promote the member to admin; an admin may view billing/usage.
		var teammateID string
		for _, m := range listMembers(t, ownerClient, ts, orgID) {
			if m.Email == "teammate@example.com" {
				teammateID = m.UserID
			}
		}
		if teammateID == "" {
			t.Fatal("teammate not in member list")
		}
		req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/orgs/"+orgID+"/members/"+teammateID, strings.NewReader(`{"role":"admin"}`))
		req.Header.Set("Content-Type", "application/json")
		pr, err := ownerClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = pr.Body.Close()
		if pr.StatusCode != http.StatusOK {
			t.Fatalf("promote to admin: want 200, got %d", pr.StatusCode)
		}

		// re-login so the new role is on the principal, then the admin can read.
		adminClient, _ := login(t, "ent-teammate", "teammate@example.com")
		ar, e := getEnts(t, adminClient, orgID)
		_ = ar.Body.Close()
		if ar.StatusCode != http.StatusOK {
			t.Fatalf("admin read: want 200, got %d", ar.StatusCode)
		}
		if e.MonitorsUsed != 3 {
			t.Fatalf("admin sees monitors_used = %d, want 3", e.MonitorsUsed)
		}
	})

	// --- a non-member is 403 ---
	t.Run("non_member_403", func(t *testing.T) {
		strangerClient, _ := login(t, "ent-stranger", "stranger@example.com")
		resp, _ := getEnts(t, strangerClient, orgID)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("non-member read: want 403, got %d", resp.StatusCode)
		}
	})

	// --- the plan catalog returns the four tiers with their canonical caps ---
	t.Run("plan_catalog", func(t *testing.T) {
		resp, err := ownerClient.Get(ts.URL + "/api/v1/plans")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("plans: want 200, got %d", resp.StatusCode)
		}
		var plans []planEntryDTO
		if err := json.NewDecoder(resp.Body).Decode(&plans); err != nil {
			t.Fatalf("decode plans: %v", err)
		}
		byPlan := map[string]planEntryDTO{}
		for _, p := range plans {
			byPlan[p.Plan] = p
		}
		for _, name := range []string{"tier1", "tier2", "tier3", "tierCustom"} {
			if _, ok := byPlan[name]; !ok {
				t.Fatalf("plan %q missing from catalog: %+v", name, plans)
			}
		}
		// Free and Custom anchor the catalog (pricing.html).
		free := byPlan["tier1"]
		if free.MonitorsCap != 10 || free.SeatsCap != 1 || free.StatusPagesCap != 1 {
			t.Fatalf("free caps wrong: %+v", free)
		}
		if free.RetentionDays != 7 || free.ApiWriteAllowed || free.CustomDomain {
			t.Fatalf("free flags wrong: %+v", free)
		}
		biz := byPlan["tierCustom"]
		if biz.MonitorsCap != 1000 || biz.SeatsCap != 1_000_000 || biz.StatusPagesCap != 1000 {
			t.Fatalf("business caps wrong: %+v", biz)
		}
		if biz.RetentionDays != 180 || !biz.ApiWriteAllowed || !biz.CustomDomain || biz.ApiRatePerMin != 600 {
			t.Fatalf("business flags wrong: %+v", biz)
		}
	})
}

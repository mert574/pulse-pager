//go:build integration

// Status pages HTTP API integration test (PRD-004). It drives the REAL handlers in
// internal/api wired to the REAL authn services and a REAL Postgres store
// (testcontainers, so RLS is in force), reusing the login helpers from
// api_identity_test.go. It covers:
//
//   - management: owner creates a page, adds monitors with display names, publishes;
//     list/get/update/delete;
//   - entitlement: creating past the status-page cap is blocked with
//     status_page_limit_reached (402);
//   - authz: a member can manage, a viewer cannot (403);
//   - validation: a bad slug and a monitor not in the org are per-field 422;
//   - public: GET the public endpoint for a PUBLISHED page returns friendly display
//     names + per-monitor status + uptime and does NOT leak the raw monitor url /
//     headers / assertions; an unpublished or unknown slug is a 404; no auth needed;
//   - banner: a down monitor reports partial/major outage; all-up reports operational.
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
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/api"
	"pulse/internal/authn"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/store"
)

type statusPageDTO struct {
	Id              string `json:"id"`
	OrgId           string `json:"org_id"`
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	State           string `json:"state"`
	DisplayMonitors []struct {
		MonitorId   string `json:"monitor_id"`
		DisplayName string `json:"display_name"`
		Order       int    `json:"order"`
	} `json:"display_monitors"`
}

type publicStatusPageDTO struct {
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Banner   string `json:"banner"`
	Monitors []struct {
		DisplayName string `json:"display_name"`
		Status      string `json:"status"`
		Uptime      struct {
			Uptime24h float64 `json:"uptime_24h"`
			Has24h    bool    `json:"has_24h"`
		} `json:"uptime"`
		History []struct {
			Up bool `json:"up"`
		} `json:"history"`
	} `json:"monitors"`
	Incidents []struct {
		DisplayName string `json:"display_name"`
		Resolved    bool   `json:"resolved"`
	} `json:"incidents"`
}

func TestAPIStatusPages(t *testing.T) {
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

	mkServer := func(cap int) *httptest.Server {
		srv := api.New(api.Config{
			Store:       app,
			Login:       loginSvc,
			JWT:         jwtIssuer,
			Refresh:     refreshSvc,
			Cookies:     authn.CookieConfig{Secure: false},
			Auth:        auth,
			AppBaseURL:  "http://app.test",
			Seats:       entitlements.FixedSeats{Cap: 5},
			Monitors:    entitlements.FixedMonitors{Limits: entitlements.MonitorLimits{MonitorsCap: 50, MinIntervalSeconds: 30, RegionsAllowed: []string{"home"}, RegionsPerMonitorCap: 1}},
			StatusPages: entitlements.FixedStatusPages{Cap: cap},
		})
		return httptest.NewServer(srv.Router())
	}

	ts := mkServer(5)
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
	put := func(c *http.Client, path, body string) *http.Response {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	ownerClient, ownerMe := login(t, "sp-owner", "sp-owner@example.com")
	orgID := ownerMe.Orgs[0].OrgID
	var orgIDInt int64
	fmt.Sscan(orgID, &orgIDInt)

	// Seed two monitors in the org directly via the store. mon1 carries a SECRET-bearing
	// internal url + header + body assertion; the public endpoint must never reveal them.
	secretURL := "https://internal-api.example.com/super-secret-health"
	maxLat := 250
	bodyContains := "ok-secret-token"
	mon1 := &domain.Monitor{
		OrgID: orgIDInt, Type: domain.MonitorHTTP, Name: "internal-1", URL: secretURL,
		Method: "GET", ExpectedStatusCodes: "200", TimeoutSeconds: 5, IntervalSeconds: 60,
		Enabled: true, FailureThreshold: 1, Regions: []string{"home"}, DownPolicy: domain.DownPolicyQuorum,
		Headers:      []domain.Header{{Key: "Authorization", Value: "Bearer leak-me", Secret: true}},
		MaxLatencyMs: &maxLat, BodyContains: &bodyContains,
	}
	mon2 := &domain.Monitor{
		OrgID: orgIDInt, Type: domain.MonitorHTTP, Name: "internal-2", URL: "https://internal-2.example.com/x",
		Method: "GET", ExpectedStatusCodes: "200", TimeoutSeconds: 5, IntervalSeconds: 60,
		Enabled: true, FailureThreshold: 1, Regions: []string{"home"}, DownPolicy: domain.DownPolicyQuorum,
	}
	if _, err := app.CreateMonitor(ctx, mon1); err != nil {
		t.Fatalf("seed mon1: %v", err)
	}
	if _, err := app.CreateMonitor(ctx, mon2); err != nil {
		t.Fatalf("seed mon2: %v", err)
	}
	mon1ID := fmt.Sprintf("%d", mon1.ID)
	mon2ID := fmt.Sprintf("%d", mon2.ID)

	// seed some check results so uptime/history have data; both healthy initially.
	seedResults := func(monID int64, healthy bool, n int) {
		base := time.Now().UTC()
		for i := 0; i < n; i++ {
			r := &domain.CheckResult{OrgID: orgIDInt, MonitorID: monID, Region: "home",
				CheckedAt: base.Add(-time.Duration(i) * time.Minute), Healthy: healthy}
			if err := app.InsertCheckResult(ctx, r); err != nil {
				t.Fatalf("seed result: %v", err)
			}
		}
	}
	seedResults(mon1.ID, true, 5)
	seedResults(mon2.ID, true, 5)

	pageBody := func(name, slug string) string {
		return fmt.Sprintf(`{
			"name":%q,"slug":%q,"logo_url":"","accent_color":"#4f46e5","theme":"light",
			"display_monitors":[
				{"monitor_id":%q,"display_name":"Website","order":0},
				{"monitor_id":%q,"display_name":"API","order":1}
			]
		}`, name, slug, mon1ID, mon2ID)
	}

	var pageID string

	// --- create persists with displayed monitors ---
	t.Run("create_with_monitors", func(t *testing.T) {
		resp := post(ownerClient, "/api/v1/orgs/"+orgID+"/status-pages", pageBody("Acme Status", "acme"))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create: want 201, got %d (%s)", resp.StatusCode, readBody(resp))
		}
		var sp statusPageDTO
		decode(t, resp, &sp)
		if sp.Slug != "acme" || sp.State != "draft" {
			t.Fatalf("unexpected page: %+v", sp)
		}
		if len(sp.DisplayMonitors) != 2 {
			t.Fatalf("want 2 displayed monitors, got %d", len(sp.DisplayMonitors))
		}
		if sp.DisplayMonitors[0].DisplayName != "Website" {
			t.Fatalf("display name = %q", sp.DisplayMonitors[0].DisplayName)
		}
		pageID = sp.Id
	})

	// --- a draft is NOT publicly reachable (same 404 as unknown) ---
	t.Run("draft_public_404", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/public/status-pages/acme")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("draft public: want 404, got %d", resp.StatusCode)
		}
	})

	// --- publish makes the public URL resolve ---
	t.Run("publish", func(t *testing.T) {
		resp := put(ownerClient, "/api/v1/orgs/"+orgID+"/status-pages/"+pageID+"/publish", `{"published":true}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("publish: want 200, got %d (%s)", resp.StatusCode, readBody(resp))
		}
		var sp statusPageDTO
		decode(t, resp, &sp)
		if sp.State != "published" {
			t.Fatalf("state = %q, want published", sp.State)
		}
	})

	// --- public read: NO auth, friendly names + status + uptime, no internal leak ---
	t.Run("public_read_no_leak", func(t *testing.T) {
		// a fresh client with no cookies proves no auth is needed.
		resp, err := http.Get(ts.URL + "/api/v1/public/status-pages/acme")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("public read: want 200, got %d (%s)", resp.StatusCode, readBody(resp))
		}
		raw := readBody(resp)

		// the raw url, secret header value, and body-assertion string must be absent.
		for _, secret := range []string{secretURL, "internal-api.example.com", "Bearer leak-me", "Authorization", bodyContains, "internal-1", "internal-2"} {
			if strings.Contains(raw, secret) {
				t.Fatalf("public payload leaked internal data %q: %s", secret, raw)
			}
		}

		var pub publicStatusPageDTO
		if err := json.Unmarshal([]byte(raw), &pub); err != nil {
			t.Fatalf("decode public: %v", err)
		}
		if len(pub.Monitors) != 2 {
			t.Fatalf("want 2 public monitors, got %d", len(pub.Monitors))
		}
		if pub.Monitors[0].DisplayName != "Website" || pub.Monitors[1].DisplayName != "API" {
			t.Fatalf("friendly names missing: %+v", pub.Monitors)
		}
		if !pub.Monitors[0].Uptime.Has24h || pub.Monitors[0].Uptime.Uptime24h != 100 {
			t.Fatalf("uptime missing/wrong: %+v", pub.Monitors[0].Uptime)
		}
		if len(pub.Monitors[0].History) == 0 {
			t.Fatalf("expected a history bar")
		}
		if pub.Banner != "operational" {
			t.Fatalf("all-up banner = %q, want operational", pub.Banner)
		}
	})

	// --- banner: one monitor down -> partial outage ---
	t.Run("banner_partial_outage", func(t *testing.T) {
		// open an incident on mon2 so it derives down.
		inc := &domain.Incident{OrgID: orgIDInt, MonitorID: mon2.ID, StartedAt: time.Now().UTC().Add(-10 * time.Minute), CauseReason: domain.FailureReason("status_mismatch")}
		if _, err := app.CreateIncident(ctx, inc); err != nil {
			t.Fatalf("open incident: %v", err)
		}
		var pub publicStatusPageDTO
		getPublic(t, ts, "acme", &pub)
		if pub.Banner != "partial_outage" {
			t.Fatalf("banner = %q, want partial_outage", pub.Banner)
		}
		// the down monitor surfaces as a public incident, by friendly name only.
		foundDown := false
		for _, m := range pub.Monitors {
			if m.DisplayName == "API" && m.Status == "down" {
				foundDown = true
			}
		}
		if !foundDown {
			t.Fatalf("expected API monitor down: %+v", pub.Monitors)
		}
		if len(pub.Incidents) == 0 || pub.Incidents[0].DisplayName != "API" {
			t.Fatalf("expected a public incident for API, got %+v", pub.Incidents)
		}
	})

	// --- banner: all monitors down -> major outage ---
	t.Run("banner_major_outage", func(t *testing.T) {
		inc := &domain.Incident{OrgID: orgIDInt, MonitorID: mon1.ID, StartedAt: time.Now().UTC().Add(-5 * time.Minute), CauseReason: domain.FailureReason("status_mismatch")}
		if _, err := app.CreateIncident(ctx, inc); err != nil {
			t.Fatalf("open incident: %v", err)
		}
		var pub publicStatusPageDTO
		getPublic(t, ts, "acme", &pub)
		if pub.Banner != "major_outage" {
			t.Fatalf("banner = %q, want major_outage", pub.Banner)
		}
	})

	// --- list / get / update / delete ---
	t.Run("list_get_update_delete", func(t *testing.T) {
		// list
		resp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/status-pages")
		if err != nil {
			t.Fatal(err)
		}
		var pages []statusPageDTO
		decode(t, resp, &pages)
		resp.Body.Close()
		if len(pages) != 1 {
			t.Fatalf("want 1 page, got %d", len(pages))
		}

		// update: rename + reduce to one monitor.
		upBody := fmt.Sprintf(`{"name":"Renamed","slug":"acme","logo_url":"","accent_color":"#000","theme":"dark","display_monitors":[{"monitor_id":%q,"display_name":"Just Web","order":0}]}`, mon1ID)
		uresp := put(ownerClient, "/api/v1/orgs/"+orgID+"/status-pages/"+pageID, upBody)
		var usp statusPageDTO
		decode(t, uresp, &usp)
		uresp.Body.Close()
		if usp.Name != "Renamed" || len(usp.DisplayMonitors) != 1 {
			t.Fatalf("update wrong: %+v", usp)
		}

		// get
		gresp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/status-pages/" + pageID)
		if err != nil {
			t.Fatal(err)
		}
		var gsp statusPageDTO
		decode(t, gresp, &gsp)
		gresp.Body.Close()
		if gsp.Name != "Renamed" {
			t.Fatalf("get name = %q", gsp.Name)
		}

		// delete
		dreq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/orgs/"+orgID+"/status-pages/"+pageID, nil)
		dresp, err := ownerClient.Do(dreq)
		if err != nil {
			t.Fatal(err)
		}
		dresp.Body.Close()
		if dresp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete: want 204, got %d", dresp.StatusCode)
		}
		// gone publicly too.
		presp, _ := http.Get(ts.URL + "/api/v1/public/status-pages/acme")
		if presp.StatusCode != http.StatusNotFound {
			t.Fatalf("deleted public: want 404, got %d", presp.StatusCode)
		}
		presp.Body.Close()
	})

	// --- validation: bad slug, monitor not in org ---
	t.Run("validation", func(t *testing.T) {
		bad := post(ownerClient, "/api/v1/orgs/"+orgID+"/status-pages",
			`{"name":"x","slug":"Bad Slug!","logo_url":"","accent_color":"","theme":"light","display_monitors":[]}`)
		defer bad.Body.Close()
		if bad.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("bad slug: want 422, got %d", bad.StatusCode)
		}
		var env errEnvelope
		decode(t, bad, &env)
		if _, ok := env.Error.Fields["slug"]; !ok {
			t.Fatalf("expected per-field slug error, got %+v", env.Error.Fields)
		}

		foreign := post(ownerClient, "/api/v1/orgs/"+orgID+"/status-pages",
			`{"name":"y","slug":"yyy","logo_url":"","accent_color":"","theme":"light","display_monitors":[{"monitor_id":"999999","display_name":"Nope","order":0}]}`)
		defer foreign.Body.Close()
		if foreign.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("foreign monitor: want 422, got %d (%s)", foreign.StatusCode, readBody(foreign))
		}
		var env2 errEnvelope
		decode(t, foreign, &env2)
		if _, ok := env2.Error.Fields["display_monitors"]; !ok {
			t.Fatalf("expected per-field display_monitors error, got %+v", env2.Error.Fields)
		}
	})

	// --- authz: member can manage, viewer cannot ---
	t.Run("authz_member_viewer", func(t *testing.T) {
		memberID := mkUser(ctx, t, app, "sp-member@example.com")
		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgIDInt, UserID: memberID, Role: domain.RoleMember}); err != nil {
			t.Fatalf("seed member: %v", err)
		}
		memberClient, _ := login(t, "sp-member", "sp-member@example.com")
		mresp := post(memberClient, "/api/v1/orgs/"+orgID+"/status-pages", pageBody("Member Page", "memberpage"))
		mbody := readBody(mresp)
		mresp.Body.Close()
		if mresp.StatusCode != http.StatusCreated {
			t.Fatalf("member create: want 201, got %d (%s)", mresp.StatusCode, mbody)
		}

		viewerID := mkUser(ctx, t, app, "sp-viewer@example.com")
		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgIDInt, UserID: viewerID, Role: domain.RoleViewer}); err != nil {
			t.Fatalf("seed viewer: %v", err)
		}
		viewerClient, _ := login(t, "sp-viewer", "sp-viewer@example.com")
		vresp := post(viewerClient, "/api/v1/orgs/"+orgID+"/status-pages", pageBody("Nope", "nopepage"))
		vresp.Body.Close()
		if vresp.StatusCode != http.StatusForbidden {
			t.Fatalf("viewer create: want 403, got %d", vresp.StatusCode)
		}
		// but a viewer CAN list (view = any member).
		vlist, err := viewerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/status-pages")
		if err != nil {
			t.Fatal(err)
		}
		vlist.Body.Close()
		if vlist.StatusCode != http.StatusOK {
			t.Fatalf("viewer list: want 200, got %d", vlist.StatusCode)
		}
	})

	// --- entitlement: creating past the status-page cap is blocked ---
	t.Run("status_page_cap_blocked", func(t *testing.T) {
		capOwner, capMe := login(t, "sp-cap-owner", "sp-cap-owner@example.com")
		capOrg := capMe.Orgs[0].OrgID
		capTS := mkServer(1)
		defer capTS.Close()
		postCap := func(name, slug string) *http.Response {
			req, _ := http.NewRequest(http.MethodPost, capTS.URL+"/api/v1/orgs/"+capOrg+"/status-pages",
				strings.NewReader(fmt.Sprintf(`{"name":%q,"slug":%q,"logo_url":"","accent_color":"","theme":"light","display_monitors":[]}`, name, slug)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := capOwner.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}
		first := postCap("one", "cap-one")
		if first.StatusCode != http.StatusCreated {
			t.Fatalf("first page: want 201, got %d (%s)", first.StatusCode, readBody(first))
		}
		first.Body.Close()
		second := postCap("two", "cap-two")
		defer second.Body.Close()
		if second.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("over-cap: want 402, got %d (%s)", second.StatusCode, readBody(second))
		}
		var env errEnvelope
		decode(t, second, &env)
		if env.Error.Code != "status_page_limit_reached" {
			t.Fatalf("code = %q, want status_page_limit_reached", env.Error.Code)
		}
	})

	// --- unknown slug 404, no auth needed ---
	t.Run("unknown_slug_404", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/public/status-pages/does-not-exist")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown slug: want 404, got %d", resp.StatusCode)
		}
	})
}

// getPublic fetches and decodes the public projection for a slug (no auth).
func getPublic(t *testing.T, ts *httptest.Server, slug string, v any) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/v1/public/status-pages/" + slug)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public %s: want 200, got %d (%s)", slug, resp.StatusCode, readBody(resp))
	}
	decode(t, resp, v)
}

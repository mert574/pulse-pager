//go:build integration

// Incidents HTTP API integration test. It drives the REAL handlers in internal/api
// wired to the REAL authn services and a REAL Postgres store (testcontainers, so RLS
// is in force), reusing the login helpers from api_identity_test.go. It covers:
//
//   - a closed and an open incident exist (seeded via the store);
//   - GET /incidents lists both; status=open filters to the open one;
//   - GET /incidents/{id} returns the detail;
//   - POST .../annotations adds a note that then appears in the detail;
//   - owner/admin manual-close works: it sets close_reason=manual and does NOT emit a
//     recovery notification (no notify publisher is even wired, and the row is just
//     closed);
//   - a member cannot manual-close (403);
//   - closing an already-closed incident is a 409;
//   - a viewer can view; a non-member is 403; unauthenticated is 401.
package integration

import (
	"context"
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

type incidentDTO struct {
	Id          string     `json:"id"`
	MonitorId   string     `json:"monitor_id"`
	CauseReason string     `json:"cause_reason"`
	CloseReason string     `json:"close_reason"`
	EndedAt     *time.Time `json:"ended_at"`
}

type incidentDetailDTOResp struct {
	Id          string `json:"id"`
	MonitorId   string `json:"monitor_id"`
	CloseReason string `json:"close_reason"`
	Annotations []struct {
		Id   string `json:"id"`
		Note string `json:"note"`
	} `json:"annotations"`
}

type pageIncidentDTO struct {
	Items      []incidentDTO `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

func TestAPIIncidents(t *testing.T) {
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

	limits := entitlements.MonitorLimits{
		MonitorsCap: 50, MinIntervalSeconds: 30,
		RegionsAllowed: []string{"home"}, RegionsPerMonitorCap: 4,
	}
	srv := api.New(api.Config{
		Store:      app,
		Login:      loginSvc,
		JWT:        jwtIssuer,
		Refresh:    refreshSvc,
		Cookies:    authn.CookieConfig{Secure: false},
		Auth:       auth,
		AppBaseURL: "http://app.test",
		Seats:      entitlements.FixedSeats{Cap: 5},
		Monitors:   entitlements.FixedMonitors{Limits: limits},
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

	ownerClient, ownerMe := login(t, "inc-owner", "inc-owner@example.com")
	orgID := ownerMe.Orgs[0].OrgID
	var orgIDInt int64
	fmt.Sscan(orgID, &orgIDInt)

	// Seed a monitor and two incidents (one closed, one open) via the store.
	m := &domain.Monitor{
		OrgID:               orgIDInt,
		Name:                "inc-monitor",
		URL:                 "https://example.test",
		Method:              "GET",
		ExpectedStatusCodes: "200",
		TimeoutSeconds:      5,
		IntervalSeconds:     60,
		Enabled:             true,
		FailureThreshold:    1,
		Regions:             []string{"home"},
		DownPolicy:          domain.DownPolicyQuorum,
	}
	if _, err := app.CreateMonitor(ctx, m); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	closedStart := time.Now().UTC().Add(-2 * time.Hour)
	closedEnd := closedStart.Add(30 * time.Minute)
	recovered := domain.CloseRecovered
	closedID, err := app.CreateIncident(ctx, &domain.Incident{
		OrgID: orgIDInt, MonitorID: m.ID, StartedAt: closedStart, EndedAt: &closedEnd,
		CauseReason: domain.ReasonStatusMismatch, CloseReason: &recovered,
	})
	if err != nil {
		t.Fatalf("seed closed incident: %v", err)
	}
	openID, err := app.CreateIncident(ctx, &domain.Incident{
		OrgID: orgIDInt, MonitorID: m.ID, StartedAt: time.Now().UTC().Add(-10 * time.Minute),
		CauseReason: domain.ReasonTimeout,
	})
	if err != nil {
		t.Fatalf("seed open incident: %v", err)
	}
	closedStr := fmt.Sprintf("%d", closedID)
	openStr := fmt.Sprintf("%d", openID)

	get := func(c *http.Client, path string) *http.Response {
		resp, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		return resp
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

	// --- unauthenticated list is 401 ---
	t.Run("unauthenticated_401", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/api/v1/orgs/" + orgID + "/incidents")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	})

	// --- list returns both incidents; status=open filters to the open one ---
	t.Run("list_all_and_open", func(t *testing.T) {
		resp := get(ownerClient, "/api/v1/orgs/"+orgID+"/incidents")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list all: want 200, got %d (%s)", resp.StatusCode, readBody(resp))
		}
		var all pageIncidentDTO
		decode(t, resp, &all)
		if len(all.Items) != 2 {
			t.Fatalf("list all items = %d, want 2", len(all.Items))
		}

		resp2 := get(ownerClient, "/api/v1/orgs/"+orgID+"/incidents?status=open")
		defer resp2.Body.Close()
		var openOnly pageIncidentDTO
		decode(t, resp2, &openOnly)
		if len(openOnly.Items) != 1 {
			t.Fatalf("list open items = %d, want 1", len(openOnly.Items))
		}
		if openOnly.Items[0].Id != openStr {
			t.Fatalf("open list returned id %q, want %q", openOnly.Items[0].Id, openStr)
		}
		if openOnly.Items[0].EndedAt != nil {
			t.Fatalf("open incident has ended_at set: %+v", openOnly.Items[0])
		}
	})

	// --- detail returns the incident ---
	t.Run("get_detail", func(t *testing.T) {
		resp := get(ownerClient, "/api/v1/orgs/"+orgID+"/incidents/"+openStr)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get detail: want 200, got %d", resp.StatusCode)
		}
		var d incidentDetailDTOResp
		decode(t, resp, &d)
		if d.Id != openStr {
			t.Fatalf("detail id = %q, want %q", d.Id, openStr)
		}
		if len(d.Annotations) != 0 {
			t.Fatalf("new incident should have no annotations, got %d", len(d.Annotations))
		}
	})

	// --- unknown incident is 404 ---
	t.Run("get_unknown_404", func(t *testing.T) {
		resp := get(ownerClient, "/api/v1/orgs/"+orgID+"/incidents/99999")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	// --- add an annotation; it appears in the detail ---
	t.Run("add_annotation_appears", func(t *testing.T) {
		resp := post(ownerClient, "/api/v1/orgs/"+orgID+"/incidents/"+openStr+"/annotations", `{"note":"looking into it"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("add annotation: want 201, got %d (%s)", resp.StatusCode, readBody(resp))
		}

		d := get(ownerClient, "/api/v1/orgs/"+orgID+"/incidents/"+openStr)
		defer d.Body.Close()
		var detail incidentDetailDTOResp
		decode(t, d, &detail)
		if len(detail.Annotations) != 1 || detail.Annotations[0].Note != "looking into it" {
			t.Fatalf("annotation not in detail: %+v", detail.Annotations)
		}
	})

	// --- a blank annotation is a 422 ---
	t.Run("blank_annotation_422", func(t *testing.T) {
		resp := post(ownerClient, "/api/v1/orgs/"+orgID+"/incidents/"+openStr+"/annotations", `{"note":"   "}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("blank note: want 422, got %d", resp.StatusCode)
		}
	})

	// --- a member can view and annotate but cannot manual-close (403) ---
	t.Run("member_can_view_cannot_close", func(t *testing.T) {
		memberID := mkUser(ctx, t, app, "inc-member@example.com")
		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgIDInt, UserID: memberID, Role: domain.RoleMember}); err != nil {
			t.Fatalf("seed member: %v", err)
		}
		memberClient, _ := login(t, "inc-member", "inc-member@example.com")

		// member can list.
		l := get(memberClient, "/api/v1/orgs/"+orgID+"/incidents")
		defer l.Body.Close()
		if l.StatusCode != http.StatusOK {
			t.Fatalf("member list: want 200, got %d", l.StatusCode)
		}
		// member can annotate.
		a := post(memberClient, "/api/v1/orgs/"+orgID+"/incidents/"+openStr+"/annotations", `{"note":"member note"}`)
		defer a.Body.Close()
		if a.StatusCode != http.StatusCreated {
			t.Fatalf("member annotate: want 201, got %d", a.StatusCode)
		}
		// member cannot close.
		c := post(memberClient, "/api/v1/orgs/"+orgID+"/incidents/"+openStr+"/close", "")
		defer c.Body.Close()
		if c.StatusCode != http.StatusForbidden {
			t.Fatalf("member close: want 403, got %d", c.StatusCode)
		}
	})

	// --- a viewer can view ---
	t.Run("viewer_can_view", func(t *testing.T) {
		viewerID := mkUser(ctx, t, app, "inc-viewer@example.com")
		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgIDInt, UserID: viewerID, Role: domain.RoleViewer}); err != nil {
			t.Fatalf("seed viewer: %v", err)
		}
		viewerClient, _ := login(t, "inc-viewer", "inc-viewer@example.com")
		resp := get(viewerClient, "/api/v1/orgs/"+orgID+"/incidents/"+openStr)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("viewer view: want 200, got %d", resp.StatusCode)
		}
	})

	// --- a non-member is 403 ---
	t.Run("non_member_403", func(t *testing.T) {
		strangerClient, _ := login(t, "inc-stranger", "inc-stranger@example.com")
		resp := get(strangerClient, "/api/v1/orgs/"+orgID+"/incidents")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("non-member: want 403, got %d", resp.StatusCode)
		}
	})

	// --- owner manual-close sets close_reason=manual and does not recover ---
	t.Run("owner_manual_close", func(t *testing.T) {
		resp := post(ownerClient, "/api/v1/orgs/"+orgID+"/incidents/"+openStr+"/close", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("owner close: want 200, got %d (%s)", resp.StatusCode, readBody(resp))
		}
		var d incidentDetailDTOResp
		decode(t, resp, &d)
		if d.CloseReason != "manual" {
			t.Fatalf("close_reason = %q, want manual", d.CloseReason)
		}

		// The row in Postgres is closed with the manual reason and closed_by set; no
		// notify path runs (a manual close is an operator override, not a recovery).
		var reason string
		var ended *time.Time
		var closedBy *int64
		// Read with the admin pool (superuser, no RLS) so the verification query is not
		// scoped by app.current_org the way the RLS app pool would require.
		if err := admin.QueryRow(ctx,
			"SELECT close_reason, ended_at, closed_by FROM incidents WHERE id=$1", openID).
			Scan(&reason, &ended, &closedBy); err != nil {
			t.Fatal(err)
		}
		if reason != "manual" {
			t.Errorf("db close_reason = %q, want manual", reason)
		}
		if ended == nil {
			t.Errorf("db ended_at is nil after close")
		}
		if closedBy == nil {
			t.Errorf("db closed_by is nil after manual close")
		}
		// No notify_deliveries row was written: the manual close never touches the
		// notify path, so a recovery is never delivered.
		var deliveries int
		if err := admin.QueryRow(ctx,
			"SELECT count(*) FROM notify_deliveries WHERE incident_id=$1", openID).Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		if deliveries != 0 {
			t.Errorf("manual close wrote %d delivery rows, want 0 (no recovery notification)", deliveries)
		}
	})

	// --- closing an already-closed incident is a 409 ---
	t.Run("close_already_closed_409", func(t *testing.T) {
		resp := post(ownerClient, "/api/v1/orgs/"+orgID+"/incidents/"+openStr+"/close", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("re-close: want 409, got %d", resp.StatusCode)
		}
		// the originally-closed (recovered) incident also rejects a manual close.
		resp2 := post(ownerClient, "/api/v1/orgs/"+orgID+"/incidents/"+closedStr+"/close", "")
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusConflict {
			t.Fatalf("close recovered incident: want 409, got %d", resp2.StatusCode)
		}
	})
}

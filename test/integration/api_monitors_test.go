//go:build integration

// Monitors HTTP API integration test. It drives the REAL handlers in internal/api
// wired to the REAL authn services and a REAL Postgres store (testcontainers, so RLS
// is in force), reusing the login helpers from api_identity_test.go to get authed
// sessions for an owner, a member, and a viewer. It covers:
//
//   - create -> persists and appears in the list with a status;
//   - get / update / delete;
//   - per-field validation failures (bad URL, interval < 30, interval < timeout,
//     body on a GET) return the per-field envelope;
//   - entitlement: creating past the plan monitor cap is blocked with
//     monitor_limit_reached; a sub-floor interval is rejected per-field;
//   - check-now returns a result;
//   - results + incidents reads work;
//   - authz: a viewer cannot create (403); a non-member is 403; unauthenticated 401;
//   - the monitor.changed event is published on create (captured fake publisher).
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
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/api"
	"pulse/internal/authn"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/events"
	"pulse/internal/store"
)

// capturePublisher records the monitor.changed events the api would publish, so the
// test can assert the scheduler wiring fires without a real bus.
type capturePublisher struct {
	mu     sync.Mutex
	events []capturedChange
}

type capturedChange struct {
	orgID     int64
	monitorID int64
}

func (c *capturePublisher) MonitorChanged(_ context.Context, orgID, monitorID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, capturedChange{orgID: orgID, monitorID: monitorID})
	return nil
}

// captureJobs records the check jobs check-now enqueues, so the test can assert the
// fan-out without a real bus/worker.
type captureJobs struct {
	mu   sync.Mutex
	jobs []events.CheckJob
}

func (c *captureJobs) PublishCheckJob(_ context.Context, job events.CheckJob) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jobs = append(c.jobs, job)
	return nil
}

func (c *captureJobs) forMonitor(id int64) []events.CheckJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []events.CheckJob
	for _, j := range c.jobs {
		if j.Monitor.ID == id {
			out = append(out, j)
		}
	}
	return out
}

// memState is a map-backed checkstate.MultiStore for the live region-state tests.
type memState struct {
	mu     sync.Mutex
	hashes map[string]map[string]string
}

func newMemState() *memState { return &memState{hashes: map[string]map[string]string{}} }

func (m *memState) HSet(_ context.Context, key, field, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hashes[key] == nil {
		m.hashes[key] = map[string]string{}
	}
	m.hashes[key][field] = value
	return nil
}
func (m *memState) HGetAll(_ context.Context, key string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.hashes[key] {
		out[k] = v
	}
	return out, nil
}
func (m *memState) Expire(context.Context, string, time.Duration) error { return nil }
func (m *memState) HGetAllMulti(ctx context.Context, keys []string) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, k := range keys {
		h, _ := m.HGetAll(ctx, k)
		out[k] = h
	}
	return out, nil
}

func (c *capturePublisher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

type monitorDTO struct {
	Id                  string `json:"id"`
	OrgId               string `json:"org_id"`
	Name                string `json:"name"`
	Url                 string `json:"url"`
	Method              string `json:"method"`
	Enabled             bool   `json:"enabled"`
	IntervalSeconds     int    `json:"interval_seconds"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
	ExpectedStatusCodes string `json:"expected_status_codes"`
}

type monitorListItemDTO struct {
	Id           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Enabled      bool   `json:"enabled"`
	IncidentOpen bool   `json:"incident_open"`
	LastLatency  *int   `json:"last_latency_ms"`
}

type errEnvelope struct {
	Error struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Fields  map[string]string `json:"fields"`
	} `json:"error"`
}

func TestAPIMonitors(t *testing.T) {
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

	pub := &capturePublisher{}
	jobsPub := &captureJobs{}
	stateStore := newMemState()
	// A generous monitor resolver so the happy path is not blocked by the Free cap of
	// 2; the cap test below uses its own tight resolver.
	bigLimits := entitlements.MonitorLimits{
		MonitorsCap: 50, MinIntervalSeconds: 30,
		RegionsAllowed: []string{"eu-central", "us-west", "us-east"}, RegionsPerMonitorCap: 4,
	}

	mkServer := func(limits entitlements.MonitorLimits) *httptest.Server {
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
			Changed:    pub,
			Jobs:       jobsPub,
			State:      stateStore,
		})
		return httptest.NewServer(srv.Router())
	}

	ts := mkServer(bigLimits)
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

	// A real loopback target that returns 200, used as the monitor URL and check-now.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	ownerClient, ownerMe := login(t, "mon-owner", "mon-owner@example.com")
	orgID := ownerMe.Orgs[0].OrgID

	validBody := func(name, url string) string {
		return fmt.Sprintf(`{
			"name":%q,"url":%q,"method":"GET","headers":[],"body":"",
			"expected_status_codes":"200","timeout_seconds":5,"interval_seconds":60,
			"enabled":true,"failure_threshold":1,"notification_channel_ids":[],
			"regions":["eu-central"],"down_policy":"quorum"
		}`, name, url)
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

	var createdID string

	// --- unauthenticated create is 401 ---
	t.Run("unauthenticated_create_401", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/api/v1/orgs/"+orgID+"/monitors", "application/json",
			strings.NewReader(validBody("x", target.URL)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", resp.StatusCode)
		}
	})

	// --- owner creates a monitor; it persists and is listed with a status ---
	t.Run("create_persists_and_lists", func(t *testing.T) {
		before := pub.count()
		resp := post(ownerClient, "/api/v1/orgs/"+orgID+"/monitors", validBody("Marketing", target.URL))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create: want 201, got %d (%s)", resp.StatusCode, readBody(resp))
		}
		var m monitorDTO
		decode(t, resp, &m)
		if m.Id == "" || m.Name != "Marketing" || m.OrgId == "" {
			t.Fatalf("created monitor wrong: %+v", m)
		}
		createdID = m.Id

		// monitor.changed was published for the create.
		if pub.count() != before+1 {
			t.Fatalf("expected monitor.changed published on create: before=%d after=%d", before, pub.count())
		}

		// it appears in the list with a derived status (pending, no checks yet).
		list := listMonitors(t, ownerClient, ts, orgID)
		var found *monitorListItemDTO
		for i := range list {
			if list[i].Id == createdID {
				found = &list[i]
			}
		}
		if found == nil {
			t.Fatalf("created monitor not in list: %+v", list)
		}
		if found.Status != "pending" {
			t.Fatalf("status = %q, want pending", found.Status)
		}
	})

	// --- get the monitor ---
	t.Run("get_monitor", func(t *testing.T) {
		resp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/monitors/" + createdID)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get: want 200, got %d", resp.StatusCode)
		}
		var m monitorDTO
		decode(t, resp, &m)
		if m.Id != createdID {
			t.Fatalf("got wrong monitor: %+v", m)
		}
	})

	// --- get an unknown monitor is 404 ---
	t.Run("get_unknown_404", func(t *testing.T) {
		resp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/monitors/99999")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	// --- update the monitor ---
	t.Run("update_monitor", func(t *testing.T) {
		body := fmt.Sprintf(`{
			"name":"Marketing v2","url":%q,"method":"GET","headers":[],"body":"",
			"expected_status_codes":"200,204","timeout_seconds":5,"interval_seconds":120,
			"enabled":true,"failure_threshold":2,"notification_channel_ids":[],
			"regions":["eu-central"],"down_policy":"any"
		}`, target.URL)
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/orgs/"+orgID+"/monitors/"+createdID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := ownerClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("update: want 200, got %d (%s)", resp.StatusCode, readBody(resp))
		}
		var m monitorDTO
		decode(t, resp, &m)
		if m.Name != "Marketing v2" || m.IntervalSeconds != 120 || m.ExpectedStatusCodes != "200,204" {
			t.Fatalf("update did not apply: %+v", m)
		}
	})

	// --- check-now enqueues per region and returns 202 with regions scheduled ---
	t.Run("check_now", func(t *testing.T) {
		resp := post(ownerClient, "/api/v1/orgs/"+orgID+"/monitors/"+createdID+"/check", "")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("check: want 202, got %d (%s)", resp.StatusCode, readBody(resp))
		}
		var acc struct {
			MonitorId string `json:"monitor_id"`
			Regions   []struct {
				Region string `json:"region"`
				State  string `json:"state"`
			} `json:"regions"`
		}
		decode(t, resp, &acc)
		if acc.MonitorId != createdID || len(acc.Regions) == 0 {
			t.Fatalf("accepted body wrong: %+v", acc)
		}
		for _, r := range acc.Regions {
			if r.State != "scheduled" {
				t.Fatalf("region %s should be scheduled at accept, got %s", r.Region, r.State)
			}
		}
		// A job was enqueued for each region of the monitor.
		var monIDInt int64
		fmt.Sscan(createdID, &monIDInt)
		if got := jobsPub.forMonitor(monIDInt); len(got) != len(acc.Regions) {
			t.Fatalf("enqueued %d jobs, want %d (one per region)", len(got), len(acc.Regions))
		}
	})

	// --- live region states reflect the scheduled check ---
	t.Run("region_states", func(t *testing.T) {
		resp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/monitor-region-states?monitor_id=" + createdID)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("region-states: want 200, got %d", resp.StatusCode)
		}
		var body struct {
			Monitors map[string][]struct {
				Region string `json:"region"`
				State  string `json:"state"`
			} `json:"monitors"`
		}
		decode(t, resp, &body)
		states, ok := body.Monitors[createdID]
		if !ok || len(states) == 0 {
			t.Fatalf("expected live region states for the monitor, got %+v", body.Monitors)
		}
		if states[0].State != "scheduled" {
			t.Fatalf("region state should be scheduled (no worker ran), got %s", states[0].State)
		}
	})

	// --- results read (seed a result via the store, then read it) ---
	t.Run("results_read", func(t *testing.T) {
		var orgIDInt, monIDInt int64
		fmt.Sscan(orgID, &orgIDInt)
		fmt.Sscan(createdID, &monIDInt)
		code := 200
		lat := 50
		scheduled := time.Now().UTC().Truncate(time.Second)
		if err := app.InsertCheckResult(ctx, &domain.CheckResult{
			OrgID: orgIDInt, MonitorID: monIDInt, Region: "eu-central",
			ScheduledAt: scheduled, CheckedAt: scheduled.Add(120 * time.Millisecond),
			Healthy: true, StatusCode: &code, LatencyMs: &lat,
		}); err != nil {
			t.Fatalf("seed result: %v", err)
		}
		resp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/monitors/" + createdID + "/results?range=24h")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("results: want 200, got %d", resp.StatusCode)
		}
		var page struct {
			Items []struct {
				Healthy     bool      `json:"healthy"`
				ScheduledAt time.Time `json:"scheduled_at"`
			} `json:"items"`
		}
		decode(t, resp, &page)
		if len(page.Items) == 0 {
			t.Fatal("expected the seeded result")
		}
		// the tick key round-trips through the store and the api DTO, so the ui can
		// group a run's regions by it.
		if !page.Items[0].ScheduledAt.Equal(scheduled) {
			t.Fatalf("scheduled_at: want %v, got %v", scheduled, page.Items[0].ScheduledAt)
		}
	})

	// --- incidents read works (seed one via the store, then read it) ---
	t.Run("incidents_read", func(t *testing.T) {
		var orgIDInt int64
		fmt.Sscan(orgID, &orgIDInt)
		var monIDInt int64
		fmt.Sscan(createdID, &monIDInt)
		_, err := app.CreateIncident(ctx, &domain.Incident{
			OrgID:       orgIDInt,
			MonitorID:   monIDInt,
			StartedAt:   time.Now().UTC().Add(-time.Hour),
			CauseReason: domain.ReasonStatusMismatch,
		})
		if err != nil {
			t.Fatalf("seed incident: %v", err)
		}
		resp, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/monitors/" + createdID + "/incidents")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("incidents: want 200, got %d", resp.StatusCode)
		}
		var page struct {
			Items []struct {
				CauseReason string `json:"cause_reason"`
			} `json:"items"`
		}
		decode(t, resp, &page)
		if len(page.Items) != 1 || page.Items[0].CauseReason != "status_mismatch" {
			t.Fatalf("incidents read wrong: %+v", page.Items)
		}
		// the open incident now flips the list status to down.
		list := listMonitors(t, ownerClient, ts, orgID)
		for _, it := range list {
			if it.Id == createdID && !it.IncidentOpen {
				t.Fatalf("expected incident_open true for the down monitor: %+v", it)
			}
		}
	})

	// --- validation: bad URL, interval < 30, interval < timeout, body on GET ---
	t.Run("validation_per_field", func(t *testing.T) {
		cases := []struct {
			name  string
			body  string
			field string
		}{
			{"bad_url", `{"name":"x","url":"ftp://nope","method":"GET","expected_status_codes":"200","timeout_seconds":5,"interval_seconds":60,"enabled":true,"failure_threshold":1,"regions":["eu-central"],"down_policy":"quorum"}`, "url"},
			{"interval_below_hard_floor", `{"name":"x","url":"https://a.test","method":"GET","expected_status_codes":"200","timeout_seconds":5,"interval_seconds":10,"enabled":true,"failure_threshold":1,"regions":["eu-central"],"down_policy":"quorum"}`, "interval_seconds"},
			{"interval_below_timeout", `{"name":"x","url":"https://a.test","method":"GET","expected_status_codes":"200","timeout_seconds":40,"interval_seconds":35,"enabled":true,"failure_threshold":1,"regions":["eu-central"],"down_policy":"quorum"}`, "interval_seconds"},
			{"body_on_get", `{"name":"x","url":"https://a.test","method":"GET","body":"hello","expected_status_codes":"200","timeout_seconds":5,"interval_seconds":60,"enabled":true,"failure_threshold":1,"regions":["eu-central"],"down_policy":"quorum"}`, "body"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				resp := post(ownerClient, "/api/v1/orgs/"+orgID+"/monitors", tc.body)
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusUnprocessableEntity {
					t.Fatalf("want 422, got %d (%s)", resp.StatusCode, readBody(resp))
				}
				var env errEnvelope
				decode(t, resp, &env)
				if env.Error.Code != "validation_failed" {
					t.Fatalf("code = %q, want validation_failed", env.Error.Code)
				}
				if _, ok := env.Error.Fields[tc.field]; !ok {
					t.Fatalf("expected per-field error on %q, got fields %+v", tc.field, env.Error.Fields)
				}
			})
		}
	})

	// --- a viewer cannot create (403) ---
	t.Run("viewer_cannot_create_403", func(t *testing.T) {
		// seed a viewer membership in the owner's org for a fresh user.
		viewerID := mkUser(ctx, t, app, "mon-viewer@example.com")
		var orgIDInt int64
		fmt.Sscan(orgID, &orgIDInt)
		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgIDInt, UserID: viewerID, Role: domain.RoleViewer}); err != nil {
			t.Fatalf("seed viewer: %v", err)
		}
		viewerClient, _ := login(t, "mon-viewer", "mon-viewer@example.com")
		resp := post(viewerClient, "/api/v1/orgs/"+orgID+"/monitors", validBody("nope", target.URL))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("viewer create: want 403, got %d", resp.StatusCode)
		}
		// but a viewer CAN list (view = any member).
		resp2, err := viewerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/monitors")
		if err != nil {
			t.Fatal(err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("viewer list: want 200, got %d", resp2.StatusCode)
		}
	})

	// --- a non-member is 403 ---
	t.Run("non_member_403", func(t *testing.T) {
		strangerClient, _ := login(t, "mon-stranger", "mon-stranger@example.com")
		resp := post(strangerClient, "/api/v1/orgs/"+orgID+"/monitors", validBody("nope", target.URL))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("non-member create: want 403, got %d", resp.StatusCode)
		}
	})

	// --- entitlement: sub-floor interval rejected per-field ---
	t.Run("sub_floor_interval_rejected", func(t *testing.T) {
		// a tight resolver: Free-like floor of 7200s.
		tightTS := mkServer(entitlements.MonitorLimits{
			MonitorsCap: 50, MinIntervalSeconds: 7200,
			RegionsAllowed: []string{"eu-central"}, RegionsPerMonitorCap: 1,
		})
		defer tightTS.Close()
		body := fmt.Sprintf(`{"name":"x","url":%q,"method":"GET","expected_status_codes":"200","timeout_seconds":5,"interval_seconds":60,"enabled":true,"failure_threshold":1,"regions":["eu-central"],"down_policy":"quorum"}`, target.URL)
		req, _ := http.NewRequest(http.MethodPost, tightTS.URL+"/api/v1/orgs/"+orgID+"/monitors", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := ownerClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("sub-floor: want 422, got %d (%s)", resp.StatusCode, readBody(resp))
		}
		var env errEnvelope
		decode(t, resp, &env)
		if _, ok := env.Error.Fields["interval_seconds"]; !ok {
			t.Fatalf("expected per-field interval_seconds error, got %+v", env.Error.Fields)
		}
	})

	// --- entitlement: creating past the monitor cap is blocked ---
	t.Run("monitor_cap_blocked", func(t *testing.T) {
		// a fresh org so the cap count starts clean; cap of 1 (Free-like, small).
		capOwner, capMe := login(t, "cap-owner", "cap-owner@example.com")
		capOrg := capMe.Orgs[0].OrgID
		capTS := mkServer(entitlements.MonitorLimits{
			MonitorsCap: 1, MinIntervalSeconds: 30,
			RegionsAllowed: []string{"eu-central"}, RegionsPerMonitorCap: 1,
		})
		defer capTS.Close()
		postCap := func(name string) *http.Response {
			req, _ := http.NewRequest(http.MethodPost, capTS.URL+"/api/v1/orgs/"+capOrg+"/monitors", strings.NewReader(validBody(name, target.URL)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := capOwner.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}
		first := postCap("one")
		if first.StatusCode != http.StatusCreated {
			t.Fatalf("first create: want 201, got %d (%s)", first.StatusCode, readBody(first))
		}
		first.Body.Close()
		second := postCap("two")
		defer second.Body.Close()
		if second.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("over-cap create: want 402, got %d (%s)", second.StatusCode, readBody(second))
		}
		var env errEnvelope
		decode(t, second, &env)
		if env.Error.Code != "monitor_limit_reached" {
			t.Fatalf("code = %q, want monitor_limit_reached", env.Error.Code)
		}
	})

	// --- delete the monitor ---
	t.Run("delete_monitor", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/orgs/"+orgID+"/monitors/"+createdID, nil)
		resp, err := ownerClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete: want 204, got %d", resp.StatusCode)
		}
		// it is gone from get.
		g, err := ownerClient.Get(ts.URL + "/api/v1/orgs/" + orgID + "/monitors/" + createdID)
		if err != nil {
			t.Fatal(err)
		}
		defer g.Body.Close()
		if g.StatusCode != http.StatusNotFound {
			t.Fatalf("get after delete: want 404, got %d", g.StatusCode)
		}
	})
}

// --- helpers ---

func listMonitors(t *testing.T, c *http.Client, ts *httptest.Server, orgID string) []monitorListItemDTO {
	t.Helper()
	resp, err := c.Get(ts.URL + "/api/v1/orgs/" + orgID + "/monitors")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list monitors: want 200, got %d", resp.StatusCode)
	}
	var list []monitorListItemDTO
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	return list
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func readBody(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

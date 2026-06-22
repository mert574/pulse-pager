package devapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(Handler(log))
	t.Cleanup(srv.Close)
	return srv
}

// get issues an authenticated GET with a short timeout, so a handler deadlock
// surfaces as a failed request instead of hanging the whole test run.
func get(t *testing.T, base, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", atCookie+"="+devSession)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func TestMeRequiresSession(t *testing.T) {
	srv := testServer(t)
	resp, err := http.Get(srv.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me without cookie = %d, want 401", resp.StatusCode)
	}
}

// Regression guard: listIncidents once re-entered the non-reentrant mutex via a
// locking helper and deadlocked. Both incident endpoints must return promptly.
func TestIncidentsDoNotDeadlock(t *testing.T) {
	srv := testServer(t)

	for _, path := range []string{
		"/api/v1/orgs/org_dev/incidents",
		"/api/v1/orgs/org_dev/monitors/mon_2/incidents",
	} {
		resp := get(t, srv.URL, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// Run with -race: check-now generates an id from shared state, so concurrent
// calls must not race on nextID.
func TestCheckNowConcurrent(t *testing.T) {
	srv := testServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/orgs/org_dev/monitors/mon_1/check", nil)
			req.Header.Set("Cookie", atCookie+"="+devSession)
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Errorf("check-now: %v", err)
				return
			}
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
}

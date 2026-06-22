package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"pulse/internal/domain"
)

func monitor(url, expect string) *domain.Monitor {
	return &domain.Monitor{
		ID:                  1,
		URL:                 url,
		Method:              "GET",
		ExpectedStatusCodes: expect,
		TimeoutSeconds:      5,
	}
}

func TestCheck_CapturesSnapshotOnStatusMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Debug", "yes")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	res := New(Config{}).Check(context.Background(), monitor(srv.URL, "200"), true)

	if res.Healthy {
		t.Fatal("expected unhealthy")
	}
	if res.FailureReason == nil || *res.FailureReason != domain.ReasonStatusMismatch {
		t.Fatalf("reason = %v, want status_mismatch", res.FailureReason)
	}
	if res.Snapshot == nil {
		t.Fatal("expected a response snapshot on a status_mismatch")
	}
	if res.Snapshot.StatusCode == nil || *res.Snapshot.StatusCode != 503 {
		t.Errorf("snapshot status = %v, want 503", res.Snapshot.StatusCode)
	}
	if res.Snapshot.Body != "boom" {
		t.Errorf("snapshot body = %q, want \"boom\"", res.Snapshot.Body)
	}
	if got := res.Snapshot.Headers["X-Debug"]; len(got) != 1 || got[0] != "yes" {
		t.Errorf("snapshot header X-Debug = %v, want [yes]", got)
	}
	if res.Snapshot.Truncated {
		t.Error("snapshot should not be truncated for a short body")
	}
}

func TestCheck_NoSnapshotWhenCaptureDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	// captureResponse=false: a failing check must not capture a snapshot (an
	// ungated org pays no capture cost).
	res := New(Config{}).Check(context.Background(), monitor(srv.URL, "200"), false)

	if res.Healthy {
		t.Fatal("expected unhealthy")
	}
	if res.Snapshot != nil {
		t.Error("capture disabled must not produce a snapshot, even on a failure")
	}
}

func TestCheck_NoSnapshotWhenHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	res := New(Config{}).Check(context.Background(), monitor(srv.URL, "200"), true)

	if !res.Healthy {
		t.Fatalf("expected healthy, reason=%v", res.FailureReason)
	}
	if res.Snapshot != nil {
		t.Error("a healthy check must not capture a snapshot")
	}
}

func TestCheck_SnapshotBodyTruncatedAtCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("123456"))
	}))
	defer srv.Close()

	res := New(Config{BodyCapBytes: 4}).Check(context.Background(), monitor(srv.URL, "200"), true)

	if res.Snapshot == nil {
		t.Fatal("expected a snapshot")
	}
	if res.Snapshot.Body != "1234" {
		t.Errorf("snapshot body = %q, want \"1234\" (capped)", res.Snapshot.Body)
	}
	if !res.Snapshot.Truncated {
		t.Error("expected truncated = true when body exceeds the cap")
	}
}

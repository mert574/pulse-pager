package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/domain"
)

// baseMonitor returns a monitor with sane defaults pointed at the given URL.
func baseMonitor(url string) *domain.Monitor {
	return &domain.Monitor{
		ID:                  1,
		Name:                "test",
		URL:                 url,
		Method:              "GET",
		ExpectedStatusCodes: "200",
		TimeoutSeconds:      5,
	}
}

func newChecker() *Checker {
	return New(Config{})
}

func TestCheck_Healthy200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := newChecker().Check(context.Background(), baseMonitor(srv.URL), false)

	if !res.Healthy {
		t.Fatalf("expected healthy, got reason %v err %v", res.FailureReason, res.ErrorText)
	}
	if res.FailureReason != nil {
		t.Fatalf("expected nil failure reason, got %v", *res.FailureReason)
	}
	if res.StatusCode == nil || *res.StatusCode != 200 {
		t.Fatalf("expected status 200, got %v", res.StatusCode)
	}
	if res.LatencyMs == nil {
		t.Fatal("expected latency to be set")
	}
	if res.CheckedAt.IsZero() || res.CheckedAt.Location() != time.UTC {
		t.Fatalf("expected non-zero UTC CheckedAt, got %v", res.CheckedAt)
	}
}

func TestCheck_WrongStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	res := newChecker().Check(context.Background(), baseMonitor(srv.URL), false)

	if res.Healthy {
		t.Fatal("expected unhealthy")
	}
	if res.FailureReason == nil || *res.FailureReason != domain.ReasonStatusMismatch {
		t.Fatalf("expected status_mismatch, got %v", res.FailureReason)
	}
	if res.StatusCode == nil || *res.StatusCode != 503 {
		t.Fatalf("expected status 503 filled, got %v", res.StatusCode)
	}
	if res.LatencyMs == nil {
		t.Fatal("expected latency filled even on failure")
	}
}

func TestCheck_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := baseMonitor(srv.URL)
	m.TimeoutSeconds = 1

	res := newChecker().Check(context.Background(), m, false)

	if res.Healthy {
		t.Fatal("expected unhealthy")
	}
	if res.FailureReason == nil || *res.FailureReason != domain.ReasonTimeout {
		t.Fatalf("expected timeout, got %v", res.FailureReason)
	}
}

func TestCheck_ConnectionRefused(t *testing.T) {
	// Start then immediately close a server so the port is no longer listening.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	res := newChecker().Check(context.Background(), baseMonitor(url), false)

	if res.Healthy {
		t.Fatal("expected unhealthy")
	}
	if res.FailureReason == nil || *res.FailureReason != domain.ReasonConnectionError {
		t.Fatalf("expected connection_error, got %v", res.FailureReason)
	}
	if res.ErrorText == nil || *res.ErrorText == "" {
		t.Fatal("expected error text on connection error")
	}
	if res.StatusCode != nil {
		t.Fatalf("expected nil status code on connection error, got %v", *res.StatusCode)
	}
}

func TestCheck_LatencyExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := baseMonitor(srv.URL)
	max := 10
	m.MaxLatencyMs = &max

	res := newChecker().Check(context.Background(), m, false)

	if res.Healthy {
		t.Fatal("expected unhealthy")
	}
	if res.FailureReason == nil || *res.FailureReason != domain.ReasonLatencyExceeded {
		t.Fatalf("expected latency_exceeded, got %v", res.FailureReason)
	}
	if res.StatusCode == nil || *res.StatusCode != 200 {
		t.Fatalf("expected status 200 filled, got %v", res.StatusCode)
	}
}

func TestCheck_BodyContainsPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello ok world"))
	}))
	defer srv.Close()

	m := baseMonitor(srv.URL)
	needle := "ok world"
	m.BodyContains = &needle

	res := newChecker().Check(context.Background(), m, false)

	if !res.Healthy {
		t.Fatalf("expected healthy, got reason %v", res.FailureReason)
	}
}

func TestCheck_BodyContainsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("nothing here"))
	}))
	defer srv.Close()

	m := baseMonitor(srv.URL)
	needle := "absent"
	m.BodyContains = &needle

	res := newChecker().Check(context.Background(), m, false)

	if res.Healthy {
		t.Fatal("expected unhealthy")
	}
	if res.FailureReason == nil || *res.FailureReason != domain.ReasonBodyAssertion {
		t.Fatalf("expected body_assertion_failed, got %v", res.FailureReason)
	}
}

func TestCheck_BodyCapNeedlePastCap(t *testing.T) {
	// Write more than the 64 KB cap, then put the needle right after it. The
	// checker only reads up to the cap, so the match must fail.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("a", 64*1024)))
		_, _ = w.Write([]byte("NEEDLE"))
	}))
	defer srv.Close()

	m := baseMonitor(srv.URL)
	needle := "NEEDLE"
	m.BodyContains = &needle

	res := newChecker().Check(context.Background(), m, false)

	if res.Healthy {
		t.Fatal("expected unhealthy: needle is past the 64KB cap")
	}
	if res.FailureReason == nil || *res.FailureReason != domain.ReasonBodyAssertion {
		t.Fatalf("expected body_assertion_failed, got %v", res.FailureReason)
	}
}

func TestCheck_BodyCapNeedleWithinCap(t *testing.T) {
	// Same large body but the needle sits within the cap, so it matches.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("a", 1000)))
		_, _ = w.Write([]byte("NEEDLE"))
		_, _ = w.Write([]byte(strings.Repeat("b", 64*1024)))
	}))
	defer srv.Close()

	m := baseMonitor(srv.URL)
	needle := "NEEDLE"
	m.BodyContains = &needle

	res := newChecker().Check(context.Background(), m, false)

	if !res.Healthy {
		t.Fatalf("expected healthy: needle is within the cap, got reason %v", res.FailureReason)
	}
}

func TestCheck_PriorityStatusBeforeLatency(t *testing.T) {
	// A slow 503: status mismatch must win over latency exceeded.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	m := baseMonitor(srv.URL)
	max := 10
	m.MaxLatencyMs = &max

	res := newChecker().Check(context.Background(), m, false)

	if res.FailureReason == nil || *res.FailureReason != domain.ReasonStatusMismatch {
		t.Fatalf("expected status_mismatch to win over latency, got %v", res.FailureReason)
	}
}

func TestCheck_SSRFBlocksLoopback(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// httptest serves on 127.0.0.1, which is loopback and must be blocked.
	c := New(Config{BlockPrivateNetworks: true})
	res := c.Check(context.Background(), baseMonitor(srv.URL), false)

	if res.Healthy {
		t.Fatal("expected unhealthy")
	}
	if res.FailureReason == nil || *res.FailureReason != domain.ReasonBlockedTarget {
		t.Fatalf("expected blocked_target, got %v (err %v)", res.FailureReason, res.ErrorText)
	}
	if res.StatusCode != nil {
		t.Fatalf("expected nil status code on block, got %v", *res.StatusCode)
	}
	if res.LatencyMs != nil {
		t.Fatalf("expected nil latency on block, got %v", *res.LatencyMs)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("expected no request to reach the server, got %d hits", got)
	}
}

func TestCheck_SSRFDialGuardEnforced(t *testing.T) {
	// The dial-time guard must refuse a loopback connection even if the
	// pre-resolve were bypassed. We verify dialControl directly rejects 127.0.0.1.
	if err := dialControl("tcp", "127.0.0.1:80", nil); err == nil {
		t.Fatal("expected dialControl to refuse loopback")
	}
	if err := dialControl("tcp", "10.0.0.1:80", nil); err == nil {
		t.Fatal("expected dialControl to refuse RFC1918 private")
	}
	if err := dialControl("tcp", "8.8.8.8:80", nil); err != nil {
		t.Fatalf("expected dialControl to allow public IP, got %v", err)
	}
}

func TestCheck_PostSendsBody(t *testing.T) {
	var gotBody string
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		gotHeader = r.Header.Get("X-Custom")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := baseMonitor(srv.URL)
	m.Method = "POST"
	m.Body = "payload data"
	m.Headers = []domain.Header{{Key: "X-Custom", Value: "abc"}}

	res := newChecker().Check(context.Background(), m, false)

	if !res.Healthy {
		t.Fatalf("expected healthy, got reason %v", res.FailureReason)
	}
	if gotBody != "payload data" {
		t.Fatalf("expected body to be sent, got %q", gotBody)
	}
	if gotHeader != "abc" {
		t.Fatalf("expected custom header to be sent, got %q", gotHeader)
	}
}

func TestCheck_StatusRangeMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 204
	}))
	defer srv.Close()

	m := baseMonitor(srv.URL)
	m.ExpectedStatusCodes = "2xx"

	res := newChecker().Check(context.Background(), m, false)

	if !res.Healthy {
		t.Fatalf("expected healthy for 204 against 2xx, got %v", res.FailureReason)
	}
}

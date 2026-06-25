package paddle

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"pulse/internal/billing"
)

const testSecret = "pdl_ntfset_test"

func newProvider(t *testing.T, baseURL string) *Provider {
	t.Helper()
	p, err := New(Config{
		APIKey:        "pdl_test",
		BaseURL:       baseURL,
		WebhookSecret: testSecret,
		Prices:        map[string]string{"tier3:monthly": "pri_test", "tier2:annual": "pri_hobby_yr"},
		PricesNoTrial: map[string]string{"tier3:monthly": "pri_test_notrial"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// signPaddle reproduces Paddle's webhook signature (HMAC-SHA256 over "<ts>:<body>"),
// so the SDK verifier accepts it. Uses the current time to stay inside the replay window.
func signPaddle(secret string, body []byte) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte(":"))
	mac.Write(body)
	return "ts=" + ts + ";h1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestCheckout(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transactions" {
			t.Errorf("path: got %s want /transactions", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"txn_123","status":"ready","checkout":{"url":"https://pay.paddle.com/?_ptxn=txn_123"}}}`))
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL)
	url, err := p.Checkout(context.Background(), 42, "tier3", "monthly", true)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if url != "https://pay.paddle.com/?_ptxn=txn_123" {
		t.Fatalf("checkout url: %q", url)
	}
	// the request must carry the trialled catalog price and the org_id in custom_data
	items, _ := gotBody["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items: %v", gotBody["items"])
	}
	if item, _ := items[0].(map[string]any); item["price_id"] != "pri_test" {
		t.Fatalf("price_id: want pri_test (trialled), got %v", item["price_id"])
	}
	if cd, _ := gotBody["custom_data"].(map[string]any); cd["org_id"] != "42" {
		t.Fatalf("custom_data org_id: %v", gotBody["custom_data"])
	}
}

// A trial-ineligible checkout (withTrial=false) must use the trialless price id.
func TestCheckoutNoTrialUsesTriallessPrice(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"txn_1","status":"ready","checkout":{"url":"https://pay.paddle.com/?_ptxn=txn_1"}}}`))
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL)
	if _, err := p.Checkout(context.Background(), 42, "tier3", "monthly", false); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	items, _ := gotBody["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items: %v", gotBody["items"])
	}
	if item, _ := items[0].(map[string]any); item["price_id"] != "pri_test_notrial" {
		t.Fatalf("price_id: want pri_test_notrial (trialless), got %v", item["price_id"])
	}
}

func TestCheckoutUnknownPrice(t *testing.T) {
	p := newProvider(t, "https://example.invalid")
	if _, err := p.Checkout(context.Background(), 1, "tier3", "annual", true); err == nil {
		t.Fatal("expected error for a plan/cycle with no configured price")
	}
}

// When no trialless price is configured for a plan/cycle, an ineligible checkout falls
// back to the trialled price so the sale still completes.
func TestCheckoutNoTrialFallsBack(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"txn_1","status":"ready","checkout":{"url":"https://pay.paddle.com/?_ptxn=txn_1"}}}`))
	}))
	defer srv.Close()

	// tier2:annual has only a trialled price configured (no trialless variant).
	p := newProvider(t, srv.URL)
	if _, err := p.Checkout(context.Background(), 42, "tier2", "annual", false); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	items, _ := gotBody["items"].([]any)
	if item, _ := items[0].(map[string]any); item["price_id"] != "pri_hobby_yr" {
		t.Fatalf("price_id: want pri_hobby_yr (fallback to trialled), got %v", item["price_id"])
	}
}

func TestPortalURL(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// the SDK addresses /customers/{customer_id}/portal-sessions
		if r.URL.Path != "/customers/ctm_1/portal-sessions" {
			t.Errorf("path: got %s want /customers/ctm_1/portal-sessions", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"cpls_1","customer_id":"ctm_1",
			"urls":{"general":{"overview":"https://pay.paddle.com/portal/ctm_1"}}}}`))
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL)
	url, err := p.PortalURL(context.Background(), "ctm_1", "sub_1")
	if err != nil {
		t.Fatalf("PortalURL: %v", err)
	}
	if url != "https://pay.paddle.com/portal/ctm_1" {
		t.Fatalf("portal url: %q", url)
	}
	// passing a subscription id asks Paddle for that subscription's deep links too
	ids, _ := gotBody["subscription_ids"].([]any)
	if len(ids) != 1 || ids[0] != "sub_1" {
		t.Fatalf("subscription_ids: %v", gotBody["subscription_ids"])
	}
}

func TestPortalURLNoCustomer(t *testing.T) {
	p := newProvider(t, "https://example.invalid")
	if _, err := p.PortalURL(context.Background(), "", ""); err == nil {
		t.Fatal("expected error when no customer id is given")
	}
}

func TestVerifyWebhookSubscription(t *testing.T) {
	p := newProvider(t, "")
	body := []byte(`{
		"event_id":"evt_1","event_type":"subscription.activated",
		"data":{"id":"sub_1","status":"active","customer_id":"ctm_1",
			"custom_data":{"org_id":"42"},
			"items":[{"price":{"id":"pri_test"}}],
			"current_billing_period":{"ends_at":"2026-07-01T00:00:00Z"},
			"scheduled_change":null}}`)

	ev, err := p.VerifyWebhook(body, signPaddle(testSecret, body))
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.ID != "evt_1" || ev.Type != "subscription.activated" || ev.Provider != "paddle" {
		t.Fatalf("event head: %+v", ev)
	}
	if ev.Kind != billing.EventKindSubscription {
		t.Fatalf("kind: got %v want EventKindSubscription", ev.Kind)
	}
	if ev.OrgID != 42 || ev.ProviderSubscriptionID != "sub_1" || ev.ProviderCustomerID != "ctm_1" {
		t.Fatalf("ids: %+v", ev)
	}
	if ev.Plan != "tier3" || ev.Cycle != "monthly" || ev.Status != "active" {
		t.Fatalf("plan/cycle/status from reverse price map: %+v", ev)
	}
	if ev.CancelAtPeriodEnd {
		t.Fatalf("cancel flag should be false")
	}
}

func TestVerifyWebhookPayment(t *testing.T) {
	p := newProvider(t, "")
	body := []byte(`{
		"event_id":"evt_2","event_type":"transaction.completed",
		"data":{"id":"txn_9","status":"completed","customer_id":"ctm_1",
			"custom_data":{"org_id":"42"},
			"details":{"totals":{"grand_total":"1900","currency_code":"USD"}}}}`)

	ev, err := p.VerifyWebhook(body, signPaddle(testSecret, body))
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if ev.Kind != billing.EventKindPayment {
		t.Fatalf("kind: got %v want EventKindPayment", ev.Kind)
	}
	if ev.Payment == nil {
		t.Fatal("expected a payment on a transaction event")
	}
	if ev.OrgID != 42 || ev.Payment.ProviderPaymentID != "txn_9" ||
		ev.Payment.Amount.Minor != 1900 || ev.Payment.Amount.Currency != "USD" {
		t.Fatalf("payment: %+v / %+v", ev, ev.Payment)
	}
}

func TestVerifyWebhookBadSignature(t *testing.T) {
	p := newProvider(t, "")
	body := []byte(`{"event_id":"evt_x","event_type":"subscription.activated","data":{}}`)
	for _, sig := range []string{"", "garbage", signPaddle("wrong-secret", body)} {
		if _, err := p.VerifyWebhook(body, sig); !errors.Is(err, billing.ErrBadSignature) {
			t.Fatalf("sig %q: got %v want ErrBadSignature", sig, err)
		}
	}
}

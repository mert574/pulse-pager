package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const testSecret = "test-billing-secret"

// a valid stub webhook body for a subscription activation on org 42.
var sampleBody = []byte(`{
	"id": "evt_1",
	"type": "subscription.activated",
	"custom_data": {"org_id": 42},
	"data": {
		"customer_id": "cus_1",
		"subscription_id": "sub_1",
		"price_id": "pri_1",
		"plan": "tier3",
		"cycle": "monthly",
		"status": "active",
		"cancel_at_period_end": false
	}
}`)

func TestStubVerifyWebhook_valid(t *testing.T) {
	s := NewStub(testSecret)
	sig := SignStubWebhook(testSecret, "1700000000", sampleBody)

	ev, err := s.VerifyWebhook(sampleBody, sig)
	if err != nil {
		t.Fatalf("verify valid: %v", err)
	}
	if ev.ID != "evt_1" || ev.Type != "subscription.activated" {
		t.Fatalf("event id/type: %q %q", ev.ID, ev.Type)
	}
	if ev.OrgID != 42 {
		t.Fatalf("org id from custom_data: got %d want 42", ev.OrgID)
	}
	if ev.Provider != "stub" {
		t.Fatalf("provider: %q", ev.Provider)
	}
	if ev.Plan != "tier3" || ev.Cycle != "monthly" || ev.Status != "active" {
		t.Fatalf("plan/cycle/status: %q %q %q", ev.Plan, ev.Cycle, ev.Status)
	}
	if ev.ProviderCustomerID != "cus_1" || ev.ProviderSubscriptionID != "sub_1" || ev.ProviderPriceID != "pri_1" {
		t.Fatalf("provider ids: %q %q %q", ev.ProviderCustomerID, ev.ProviderSubscriptionID, ev.ProviderPriceID)
	}
}

func TestStubVerifyWebhook_tamperedBody(t *testing.T) {
	s := NewStub(testSecret)
	sig := SignStubWebhook(testSecret, "1700000000", sampleBody)

	tampered := append([]byte(nil), sampleBody...)
	tampered[len(tampered)-2] = ' ' // flip a byte so the signature no longer matches

	if _, err := s.VerifyWebhook(tampered, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body: got %v want ErrBadSignature", err)
	}
}

func TestStubVerifyWebhook_wrongSecret(t *testing.T) {
	s := NewStub(testSecret)
	sig := SignStubWebhook("other-secret", "1700000000", sampleBody)

	if _, err := s.VerifyWebhook(sampleBody, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret: got %v want ErrBadSignature", err)
	}
}

func TestStubVerifyWebhook_missingSignature(t *testing.T) {
	s := NewStub(testSecret)
	for _, sig := range []string{"", "garbage", "ts=1700000000", "h1=abc"} {
		if _, err := s.VerifyWebhook(sampleBody, sig); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("sig %q: got %v want ErrBadSignature", sig, err)
		}
	}
}

func TestStubVerifyWebhook_malformedBody(t *testing.T) {
	s := NewStub(testSecret)
	bad := []byte(`{not json`)
	sig := SignStubWebhook(testSecret, "1700000000", bad)

	_, err := s.VerifyWebhook(bad, sig)
	if err == nil || errors.Is(err, ErrBadSignature) {
		t.Fatalf("malformed body: got %v want a parse error (not ErrBadSignature)", err)
	}
}

func TestStubOperatorMethods(t *testing.T) {
	s := NewStub(testSecret)
	ctx := context.Background()

	url, err := s.Checkout(ctx, 7, "tier2", "monthly")
	if err != nil || !strings.Contains(url, "org=7") || !strings.Contains(url, "plan=tier2") {
		t.Fatalf("Checkout: url=%q err=%v", url, err)
	}
	portal, err := s.PortalURL(ctx, 7)
	if err != nil || !strings.Contains(portal, "org=7") {
		t.Fatalf("PortalURL: url=%q err=%v", portal, err)
	}
	if err := s.UpdateSubscription(ctx, "sub_1", PlanChange{Plan: "tier3", Cycle: "annual"}); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	if err := s.CancelSubscription(ctx, "sub_1", CancelPeriodEnd); err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}
	if err := s.Refund(ctx, "pay_1", &Money{Minor: 500, Currency: "USD"}, "duplicate"); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	ref, err := s.SetCustomPrice(ctx, 7, Money{Minor: 12900, Currency: "USD"}, "monthly")
	if err != nil || ref == "" {
		t.Fatalf("SetCustomPrice: ref=%q err=%v", ref, err)
	}
}

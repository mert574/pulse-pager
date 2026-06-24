//go:build integration

// Billing webhook sync-path integration test (RFC-018 Phase 1). It runs the REAL
// ingest handler wired to the stub provider and the REAL Postgres store as the
// restricted pulse_app role (RLS in force, NOT the admin bypass), so it proves the
// whole path: a signed webhook moves the subscription row and reconciles
// organizations.plan, redelivery is idempotent, a bad signature changes nothing, and
// RLS still isolates one org's subscription from another.
//
// Run with: go test -tags integration -run TestBilling ./test/integration/
package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/billing"
	"pulse/internal/obs"
	"pulse/internal/store"
)

const billingSecret = "itest-billing-secret"

func TestBillingWebhookSyncPath(t *testing.T) {
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

	var orgA, orgB int64
	if err := admin.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Org A','org-a') RETURNING id").Scan(&orgA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Org B','org-b') RETURNING id").Scan(&orgB); err != nil {
		t.Fatal(err)
	}

	// The ingest runs as pulse_app so RLS is exercised, exactly like cmd/billing.
	app, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	defer app.Close()

	provider := billing.NewStub(billingSecret)
	mux := http.NewServeMux()
	mux.Handle("POST /billing/webhooks/{provider}", billing.NewHandler(provider, app, obs.Logger("billing-test", "error")))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := []byte(fmt.Sprintf(`{
		"id": "evt_activate",
		"type": "subscription.activated",
		"custom_data": {"org_id": %d},
		"data": {"customer_id":"cus_a","subscription_id":"sub_a","price_id":"pri_a","plan":"tier3","cycle":"monthly","status":"active"}
	}`, orgA))

	post := func(sig string, payload []byte) int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/billing/webhooks/stub", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(billing.StubSignatureHeader, sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	validSig := billing.SignStubWebhook(billingSecret, "1700000000", body)

	t.Run("applies subscription and reconciles plan", func(t *testing.T) {
		if code := post(validSig, body); code != http.StatusOK {
			t.Fatalf("status: got %d want 200", code)
		}
		if n := countRows(t, admin, "SELECT count(*) FROM subscriptions WHERE org_id=$1", orgA); n != 1 {
			t.Fatalf("subscriptions rows: got %d want 1", n)
		}
		if got := orgPlan(t, admin, orgA); got != "tier3" {
			t.Fatalf("organizations.plan: got %q want tier3", got)
		}
		if n := countRows(t, admin, "SELECT count(*) FROM billing_events"); n != 1 {
			t.Fatalf("billing_events rows: got %d want 1", n)
		}
	})

	t.Run("redelivery is idempotent", func(t *testing.T) {
		if code := post(validSig, body); code != http.StatusOK {
			t.Fatalf("status: got %d want 200", code)
		}
		if n := countRows(t, admin, "SELECT count(*) FROM subscriptions WHERE org_id=$1", orgA); n != 1 {
			t.Fatalf("subscriptions rows after redelivery: got %d want 1", n)
		}
		if n := countRows(t, admin, "SELECT count(*) FROM billing_events"); n != 1 {
			t.Fatalf("billing_events rows after redelivery: got %d want 1", n)
		}
	})

	t.Run("bad signature changes nothing", func(t *testing.T) {
		bad := []byte(fmt.Sprintf(`{
			"id":"evt_forged","type":"subscription.activated",
			"custom_data":{"org_id":%d},
			"data":{"customer_id":"cus_a","plan":"tier1","cycle":"monthly","status":"active"}
		}`, orgA))
		if code := post("ts=1700000000;h1=deadbeef", bad); code != http.StatusUnauthorized {
			t.Fatalf("status: got %d want 401", code)
		}
		if got := orgPlan(t, admin, orgA); got != "tier3" {
			t.Fatalf("plan must be unchanged by a forged event: got %q", got)
		}
		if n := countRows(t, admin, "SELECT count(*) FROM billing_events"); n != 1 {
			t.Fatalf("billing_events must be unchanged: got %d want 1", n)
		}
	})

	t.Run("RLS isolates the subscription", func(t *testing.T) {
		// Scoped to org B, the org A subscription must be invisible (RLS, not a WHERE).
		var seen int
		err := app.WithOrg(ctx, orgB, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM subscriptions").Scan(&seen)
		})
		if err != nil {
			t.Fatal(err)
		}
		if seen != 0 {
			t.Fatalf("org B sees %d subscriptions, RLS should hide org A's", seen)
		}
	})

	// The operator store methods (RFC-018 5.1/5.2) run through WithOrg under RLS.
	t.Run("operator updates plan on the subscription", func(t *testing.T) {
		if err := app.UpdateSubscriptionPlan(ctx, orgA, "tier2", "annual", "pri_new"); err != nil {
			t.Fatalf("UpdateSubscriptionPlan: %v", err)
		}
		sub, err := app.GetSubscriptionByOrg(ctx, orgA)
		if err != nil {
			t.Fatalf("GetSubscriptionByOrg: %v", err)
		}
		if sub.Plan != "tier2" || sub.BillingCycle != "annual" || sub.ProviderPriceID != "pri_new" {
			t.Fatalf("after update: plan=%q cycle=%q price=%q", sub.Plan, sub.BillingCycle, sub.ProviderPriceID)
		}
	})

	t.Run("operator cancels at period end", func(t *testing.T) {
		if err := app.SetSubscriptionCancelAtPeriodEnd(ctx, orgA); err != nil {
			t.Fatalf("SetSubscriptionCancelAtPeriodEnd: %v", err)
		}
		sub, err := app.GetSubscriptionByOrg(ctx, orgA)
		if err != nil {
			t.Fatal(err)
		}
		if !sub.CancelAtPeriodEnd || sub.Status != "active" {
			t.Fatalf("cancel-at-period-end: cancel=%v status=%q (want true/active)", sub.CancelAtPeriodEnd, sub.Status)
		}
	})

	t.Run("operator cancels immediately drops org to free", func(t *testing.T) {
		if err := app.CancelSubscriptionNow(ctx, orgA); err != nil {
			t.Fatalf("CancelSubscriptionNow: %v", err)
		}
		sub, err := app.GetSubscriptionByOrg(ctx, orgA)
		if err != nil {
			t.Fatal(err)
		}
		if sub.Status != "canceled" {
			t.Fatalf("status after immediate cancel: %q want canceled", sub.Status)
		}
		if got := orgPlan(t, admin, orgA); got != "tier1" {
			t.Fatalf("org plan after immediate cancel: %q want tier1", got)
		}
	})

	// Payment events feed the read-only mirror (RFC-018 4). org_id is omitted here, so
	// this also exercises the provider_customer_id -> org lookup (cus_a is org A's).
	t.Run("payment event mirrors a payment", func(t *testing.T) {
		body := []byte(`{
			"id":"evt_pay1","type":"payment.succeeded",
			"data":{"customer_id":"cus_a"},
			"payment":{"payment_id":"pay_1","amount":1900,"currency":"USD","status":"paid","period":"2026-06","hosted_invoice_url":"https://inv/1"}
		}`)
		sig := billing.SignStubWebhook(billingSecret, "1700000100", body)
		if code := post(sig, body); code != http.StatusOK {
			t.Fatalf("status: got %d want 200", code)
		}
		pays, err := app.ListPayments(ctx, orgA)
		if err != nil {
			t.Fatalf("ListPayments: %v", err)
		}
		if len(pays) != 1 {
			t.Fatalf("payments: got %d want 1", len(pays))
		}
		if pays[0].Amount != 1900 || pays[0].Currency != "USD" || pays[0].RefundedAmount != 0 {
			t.Fatalf("payment: amount=%d cur=%q refunded=%d", pays[0].Amount, pays[0].Currency, pays[0].RefundedAmount)
		}
	})

	t.Run("refund event updates the same payment row", func(t *testing.T) {
		body := []byte(`{
			"id":"evt_pay2","type":"payment.refunded",
			"data":{"customer_id":"cus_a"},
			"payment":{"payment_id":"pay_1","amount":1900,"currency":"USD","status":"refunded","refunded_amount":1900}
		}`)
		sig := billing.SignStubWebhook(billingSecret, "1700000200", body)
		if code := post(sig, body); code != http.StatusOK {
			t.Fatalf("status: got %d want 200", code)
		}
		pays, err := app.ListPayments(ctx, orgA)
		if err != nil {
			t.Fatal(err)
		}
		if len(pays) != 1 {
			t.Fatalf("refund must upsert the same row, got %d payments", len(pays))
		}
		if pays[0].RefundedAmount != 1900 || pays[0].Status != "refunded" {
			t.Fatalf("after refund: refunded=%d status=%q", pays[0].RefundedAmount, pays[0].Status)
		}
	})

	// Cross-org billing aggregates for the admin panel, via the SECURITY DEFINER
	// functions (run as pulse_app, so this proves the RLS-bypass path).
	t.Run("platform billing aggregates", func(t *testing.T) {
		// Put org B on a paid plan so paid_orgs is non-zero (org A was dropped to free
		// by the immediate-cancel subtest above).
		if _, err := admin.Exec(ctx, "UPDATE organizations SET plan='tier3' WHERE id=$1", orgB); err != nil {
			t.Fatal(err)
		}
		b, err := app.PlatformBilling(ctx)
		if err != nil {
			t.Fatalf("PlatformBilling: %v", err)
		}
		if b.PaidOrgs < 1 {
			t.Fatalf("paid_orgs: got %d want >=1", b.PaidOrgs)
		}
		var canceled int64
		for _, sc := range b.SubscriptionsByStatus {
			if sc.Status == "canceled" {
				canceled = sc.Count
			}
		}
		if canceled != 1 {
			t.Fatalf("canceled subs: got %d want 1", canceled)
		}
		var usd *store.CurrencyRevenue
		for i := range b.RevenueByCurrency {
			if b.RevenueByCurrency[i].Currency == "USD" {
				usd = &b.RevenueByCurrency[i]
			}
		}
		if usd == nil || usd.Gross != 1900 || usd.Refunded != 1900 || usd.Payments != 1 {
			t.Fatalf("USD revenue: %+v want gross=1900 refunded=1900 payments=1", usd)
		}
	})
}

func countRows(t *testing.T, p *store.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

func orgPlan(t *testing.T, p *store.Pool, orgID int64) string {
	t.Helper()
	var plan string
	if err := p.QueryRow(context.Background(), "SELECT plan FROM organizations WHERE id=$1", orgID).Scan(&plan); err != nil {
		t.Fatalf("org plan query: %v", err)
	}
	return plan
}

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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/billing"
	"pulse/internal/domain"
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
			"id":"evt_pay1","type":"transaction.completed",
			"data":{"customer_id":"cus_a"},
			"payment":{"payment_id":"pay_1","amount":1900,"currency":"USD","status":"completed","period":"2026-06","hosted_invoice_url":"https://inv/1"}
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

	t.Run("a later payment event upserts the same payment row", func(t *testing.T) {
		// Same payment id, new event id: the mirror upserts the one row rather than
		// inserting a duplicate (here it also records a refunded amount).
		body := []byte(`{
			"id":"evt_pay2","type":"transaction.paid",
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
			t.Fatalf("must upsert the same row, got %d payments", len(pays))
		}
		if pays[0].RefundedAmount != 1900 || pays[0].Status != "refunded" {
			t.Fatalf("after upsert: refunded=%d status=%q", pays[0].RefundedAmount, pays[0].Status)
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

	// Every verified event is stored raw, even a type we don't act on (RFC-018 8).
	t.Run("stores raw payload for an unhandled event type", func(t *testing.T) {
		body := []byte(`{"id":"evt_other","type":"address.updated","data":{"customer_id":"cus_a"}}`)
		if code := post(billing.SignStubWebhook(billingSecret, "1700000300", body), body); code != http.StatusOK {
			t.Fatalf("status: got %d want 200 (every type is acknowledged)", code)
		}
		var typ, payload string
		var processed *time.Time
		err := admin.QueryRow(ctx,
			"SELECT type, payload::text, processed_at FROM billing_events WHERE provider_event_id='evt_other'").
			Scan(&typ, &payload, &processed)
		if err != nil {
			t.Fatalf("raw event not stored: %v", err)
		}
		if typ != "address.updated" {
			t.Fatalf("type: got %q want address.updated", typ)
		}
		if !strings.Contains(payload, "address.updated") {
			t.Fatalf("raw payload not saved: %q", payload)
		}
		if processed == nil {
			t.Fatalf("an acknowledged event should be marked processed")
		}
	})
}

// TestBillingTrialEligibility exercises the per-person free-trial gate end to end
// (RFC-018): the person_had_recent_inactive_subscription SECURITY DEFINER function must
// read across the person's owned/admin orgs even when called as the restricted pulse_app
// role (RLS bypassed), and must honor the 35-day window, the status filter, and the role
// filter. Direct inserts go through the admin pool so we can set updated_at into the past.
func TestBillingTrialEligibility(t *testing.T) {
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
	// The check runs as pulse_app, so the SECURITY DEFINER function must still see across
	// the person's orgs (the whole point of the definer bypass).
	app, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	defer app.Close()

	// One person who owns org A.
	var userID, orgA int64
	if err := admin.QueryRow(ctx, "INSERT INTO users(email) VALUES('p@example.com') RETURNING id").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Org A','elig-a') RETURNING id").Scan(&orgA); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "INSERT INTO memberships(org_id, user_id, role) VALUES($1,$2,'owner')", orgA, userID); err != nil {
		t.Fatal(err)
	}

	// hadRecentInactive returns true when the person should be DENIED a trial.
	hadRecentInactive := func() bool {
		had, err := app.PersonHadRecentInactiveSubscription(ctx, userID, 35)
		if err != nil {
			t.Fatalf("PersonHadRecentInactiveSubscription: %v", err)
		}
		return had
	}

	// setSubA upserts org A's single subscription to a status and an age in days. The
	// window keys off ended_at, so age is when the subscription ended; an active/trialing
	// row has no end yet (ended_at NULL), mirroring how the writers stamp it.
	setSubA := func(status string, ageDays int) {
		_, err := admin.Exec(ctx, `
			INSERT INTO subscriptions (org_id, plan, billing_cycle, status, provider, updated_at, ended_at)
			VALUES ($1,'tier3','monthly',$2,'stub', now(),
				CASE WHEN $2 NOT IN ('active','trialing') THEN now() - make_interval(days => $3) ELSE NULL END)
			ON CONFLICT (org_id) DO UPDATE SET status = EXCLUDED.status, ended_at = EXCLUDED.ended_at`,
			orgA, status, ageDays)
		if err != nil {
			t.Fatalf("seed subscription: %v", err)
		}
	}

	t.Run("no subscription is eligible", func(t *testing.T) {
		if hadRecentInactive() {
			t.Fatal("a person with no subscription must be trial-eligible")
		}
	})

	t.Run("active subscription does not deny a trial", func(t *testing.T) {
		setSubA("active", 1)
		if hadRecentInactive() {
			t.Fatal("an active subscription must not deny a trial")
		}
	})

	t.Run("recently cancelled denies a trial", func(t *testing.T) {
		setSubA("canceled", 10)
		if !hadRecentInactive() {
			t.Fatal("a subscription cancelled 10 days ago must deny a trial")
		}
	})

	t.Run("cancelled outside the window is eligible again", func(t *testing.T) {
		setSubA("canceled", 40)
		if hadRecentInactive() {
			t.Fatal("a subscription cancelled 40 days ago is outside the 35-day window")
		}
	})

	t.Run("re-applying a canceled event does not restart the window", func(t *testing.T) {
		// orgA's subscription ended 40 days ago (eligible again from the previous case). A
		// trailing or re-synced provider event re-applies the same canceled state. Keying
		// on updated_at used to bump it to now and re-deny the trial; with ended_at the
		// window must stay anchored at the original end.
		if err := app.ApplySubscriptionEvent(ctx, &domain.Subscription{
			OrgID: orgA, Plan: "tier1", BillingCycle: "monthly", Status: "canceled", Provider: "stub",
		}); err != nil {
			t.Fatalf("re-apply canceled event: %v", err)
		}
		if hadRecentInactive() {
			t.Fatal("re-applying a canceled event must not restart the 35-day window")
		}
	})

	t.Run("only an owner/admin membership counts", func(t *testing.T) {
		// org A is now 40 days old (does not match). Add org B with a recent cancellation
		// where our person is only a member: it must not deny a trial. A real org always
		// has an owner (the enforce_last_owner trigger guarantees it), so seed another
		// user as org B's owner; that also lets the later promote pass the last-owner guard.
		var orgB, owner2 int64
		if err := admin.QueryRow(ctx, "INSERT INTO organizations(name, slug) VALUES('Org B','elig-b') RETURNING id").Scan(&orgB); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx, "INSERT INTO users(email) VALUES('owner2@example.com') RETURNING id").Scan(&owner2); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, "INSERT INTO memberships(org_id, user_id, role) VALUES($1,$2,'owner')", orgB, owner2); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, "INSERT INTO memberships(org_id, user_id, role) VALUES($1,$2,'member')", orgB, userID); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, `INSERT INTO subscriptions (org_id, plan, billing_cycle, status, provider, updated_at, ended_at)
			VALUES ($1,'tier3','monthly','canceled','stub', now(), now() - make_interval(days => 5))`, orgB); err != nil {
			t.Fatal(err)
		}
		if hadRecentInactive() {
			t.Fatal("a recent cancellation in an org the person is only a member of must not deny a trial")
		}
		// Promote our person to admin (org B keeps owner2, so the last-owner guard is fine).
		if _, err := admin.Exec(ctx, "UPDATE memberships SET role='admin' WHERE org_id=$1 AND user_id=$2", orgB, userID); err != nil {
			t.Fatal(err)
		}
		if !hadRecentInactive() {
			t.Fatal("a recent cancellation in an org the person admins must deny a trial")
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

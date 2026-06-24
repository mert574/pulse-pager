package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// Subscriptions are org-scoped (RLS on org_id) and go through WithOrg, RFC-018 4. The
// webhook sync path is the only writer; entitlement enforcement reads the plan off
// organizations.plan, which ApplySubscriptionEvent reconciles in the same transaction.

// ErrOrgNotFound is returned by ApplySubscriptionEvent when the event's org does not
// exist or is soft-deleted. The ingest treats it as permanent (logs and acks) so the
// provider does not retry an event that can never apply.
var ErrOrgNotFound = errors.New("billing: org not found")

const subscriptionColumns = `id, org_id, plan, billing_cycle, status, provider,
	provider_customer_id, provider_subscription_id, provider_price_id,
	current_period_end, cancel_at_period_end, created_at, updated_at`

func scanSubscription(row pgx.Row) (*domain.Subscription, error) {
	var s domain.Subscription
	err := row.Scan(&s.ID, &s.OrgID, &s.Plan, &s.BillingCycle, &s.Status, &s.Provider,
		&s.ProviderCustomerID, &s.ProviderSubscriptionID, &s.ProviderPriceID,
		&s.CurrentPeriodEnd, &s.CancelAtPeriodEnd, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetSubscriptionByOrg loads an org's subscription, or pgx.ErrNoRows if it has none.
func (p *Pool) GetSubscriptionByOrg(ctx context.Context, orgID int64) (*domain.Subscription, error) {
	var sub *domain.Subscription
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+subscriptionColumns+` FROM subscriptions WHERE org_id = $1`, orgID)
		v, err := scanSubscription(row)
		if err != nil {
			return err
		}
		sub = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// ApplySubscriptionEvent upserts the subscription and reconciles organizations.plan (the
// entitlement resolvers read it) in one transaction. The webhook inbox
// (RecordBillingEvent) owns dedup and the raw-payload record; this just makes the state
// change, so re-running it is harmless (the upsert is idempotent). Returns ErrOrgNotFound
// if the org is gone or soft-deleted.
//
// sub.Plan must already be a canonical tier (the ingest runs it through
// entitlements.ParsePlan so a bad provider value fails safe to free, never junk).
func (p *Pool) ApplySubscriptionEvent(ctx context.Context, sub *domain.Subscription) error {
	return p.WithOrg(ctx, sub.OrgID, func(tx pgx.Tx) error {
		if err := upsertSubscriptionTx(ctx, tx, sub); err != nil {
			return err
		}
		// Reconcile the org's plan from the subscription. organizations is not under
		// RLS, so this UPDATE runs fine inside the org-scoped tx. Only an active org is
		// touched; a missing/deleted org is a permanent error (the caller acks it).
		tag, err := tx.Exec(ctx,
			`UPDATE organizations SET plan = $2 WHERE id = $1 AND deleted_at IS NULL`,
			sub.OrgID, sub.Plan)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrOrgNotFound
		}
		return nil
	})
}

// UpdateSubscriptionPlan switches the plan/cycle/price on an org's subscription after
// an operator plan move (RFC-018 5.1). The webhook later confirms; this is the
// optimistic local write. Returns pgx.ErrNoRows if the org has no subscription.
func (p *Pool) UpdateSubscriptionPlan(ctx context.Context, orgID int64, plan, cycle, priceID string) error {
	return p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE subscriptions
			SET plan = $2, billing_cycle = $3, provider_price_id = $4, updated_at = now()
			WHERE org_id = $1`,
			orgID, plan, cycle, priceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

// SetSubscriptionCancelAtPeriodEnd marks an org's subscription to cancel at period end
// (RFC-018 5.2). Status stays active until the provider webhook flips it. Returns
// pgx.ErrNoRows if the org has no subscription.
func (p *Pool) SetSubscriptionCancelAtPeriodEnd(ctx context.Context, orgID int64) error {
	return p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE subscriptions SET cancel_at_period_end = true, updated_at = now()
			WHERE org_id = $1`, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

// CancelSubscriptionNow cancels immediately: the subscription goes to canceled and the
// org drops to Free in one transaction (RFC-018 5.2). organizations is not under RLS,
// so the plan update runs fine in the org-scoped tx. Returns pgx.ErrNoRows if the org
// has no subscription.
func (p *Pool) CancelSubscriptionNow(ctx context.Context, orgID int64) error {
	return p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE subscriptions
			SET status = 'canceled', cancel_at_period_end = false, updated_at = now()
			WHERE org_id = $1`, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		_, err = tx.Exec(ctx,
			`UPDATE organizations SET plan = 'tier1' WHERE id = $1 AND deleted_at IS NULL`, orgID)
		return err
	})
}

// upsertSubscriptionTx writes the subscription inside an existing tx (one row per org).
func upsertSubscriptionTx(ctx context.Context, tx pgx.Tx, s *domain.Subscription) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO subscriptions (org_id, plan, billing_cycle, status, provider,
			provider_customer_id, provider_subscription_id, provider_price_id,
			current_period_end, cancel_at_period_end, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
		ON CONFLICT (org_id) DO UPDATE SET
			plan = EXCLUDED.plan,
			billing_cycle = EXCLUDED.billing_cycle,
			status = EXCLUDED.status,
			provider = EXCLUDED.provider,
			provider_customer_id = EXCLUDED.provider_customer_id,
			provider_subscription_id = EXCLUDED.provider_subscription_id,
			provider_price_id = EXCLUDED.provider_price_id,
			current_period_end = EXCLUDED.current_period_end,
			cancel_at_period_end = EXCLUDED.cancel_at_period_end,
			updated_at = now()`,
		s.OrgID, s.Plan, s.BillingCycle, s.Status, s.Provider,
		s.ProviderCustomerID, s.ProviderSubscriptionID, s.ProviderPriceID,
		s.CurrentPeriodEnd, s.CancelAtPeriodEnd)
	return err
}

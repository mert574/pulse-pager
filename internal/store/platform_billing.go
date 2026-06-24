package store

import "context"

// PlatformBilling is the cross-org billing snapshot for the operator admin panel
// (RFC-018). Like PlatformMetrics it is not org-scoped: it aggregates every org's
// subscriptions and payments through SECURITY DEFINER functions that bypass RLS and
// return aggregate rows only, never row data. Money is in minor units (cents).
type PlatformBilling struct {
	// PaidOrgs is the number of active orgs on a paid plan (plan != tier1).
	PaidOrgs              int64
	SubscriptionsByStatus []SubscriptionStatusCount
	RevenueByCurrency     []CurrencyRevenue
}

// SubscriptionStatusCount is the number of subscriptions in one status.
type SubscriptionStatusCount struct {
	Status string
	Count  int64
}

// CurrencyRevenue is the mirrored revenue in one currency (minor units).
type CurrencyRevenue struct {
	Currency string
	Gross    int64
	Refunded int64
	Payments int64
}

// PlatformBilling reads the billing aggregates. Paid orgs count directly off the
// organizations table (not RLS-protected); subscriptions and payments go through the
// platform_*  SECURITY DEFINER functions so the cross-org aggregate bypasses RLS.
func (p *Pool) PlatformBilling(ctx context.Context) (*PlatformBilling, error) {
	b := &PlatformBilling{}

	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM organizations WHERE plan <> 'tier1' AND deleted_at IS NULL`).
		Scan(&b.PaidOrgs); err != nil {
		return nil, err
	}

	subRows, err := p.Query(ctx, `SELECT status, count FROM platform_subscription_counts() ORDER BY status`)
	if err != nil {
		return nil, err
	}
	defer subRows.Close()
	for subRows.Next() {
		var sc SubscriptionStatusCount
		if err := subRows.Scan(&sc.Status, &sc.Count); err != nil {
			return nil, err
		}
		b.SubscriptionsByStatus = append(b.SubscriptionsByStatus, sc)
	}
	if err := subRows.Err(); err != nil {
		return nil, err
	}

	revRows, err := p.Query(ctx, `SELECT currency, gross, refunded, payments FROM platform_payment_totals() ORDER BY currency`)
	if err != nil {
		return nil, err
	}
	defer revRows.Close()
	for revRows.Next() {
		var cr CurrencyRevenue
		if err := revRows.Scan(&cr.Currency, &cr.Gross, &cr.Refunded, &cr.Payments); err != nil {
			return nil, err
		}
		b.RevenueByCurrency = append(b.RevenueByCurrency, cr)
	}
	if err := revRows.Err(); err != nil {
		return nil, err
	}

	return b, nil
}

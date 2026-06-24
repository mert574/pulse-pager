package store

import "context"

// OrgByCustomer maps a provider customer id back to its org, for the webhook ingest
// before app.current_org is known (RFC-018 Phase 3 follow-on events that carry only
// the customer id). It calls the subscription_org_by_customer SECURITY DEFINER
// function, which bypasses subscriptions RLS and returns a scalar org id only. Returns
// 0 (and no error) when no subscription matches, so the caller can fall back.
//
// The dedup ledger writes (billing_events) live inside ApplySubscriptionEvent's
// transaction so the dedup row and the state change commit together.
func (p *Pool) OrgByCustomer(ctx context.Context, provider, customerID string) (int64, error) {
	var orgID *int64
	err := p.QueryRow(ctx,
		`SELECT subscription_org_by_customer($1, $2)`, provider, customerID).Scan(&orgID)
	if err != nil {
		return 0, err
	}
	if orgID == nil {
		return 0, nil
	}
	return *orgID, nil
}

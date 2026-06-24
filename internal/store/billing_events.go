package store

import (
	"context"
	"time"
)

// billing_events is the webhook inbox (RFC-018 8): every verified event is recorded
// here with its raw payload BEFORE we act on it, deduped on (provider, provider_event_id),
// and stamped processed once handled. This is a platform table (no org, no RLS), so it
// runs on the bare pool.

// RecordBillingEvent stores a verified webhook event idempotently, saving the raw
// payload. It returns alreadyProcessed=true when a previous delivery was already fully
// handled, so the caller can skip re-acting. A new or received-but-unprocessed event
// returns false (the caller should act, then call MarkBillingEventProcessed). Every
// event type is recorded, whether or not we act on it.
func (p *Pool) RecordBillingEvent(ctx context.Context, provider, eventID, eventType string, payload []byte) (alreadyProcessed bool, err error) {
	if _, err = p.Exec(ctx, `
		INSERT INTO billing_events (provider, provider_event_id, type, payload, received_at)
		VALUES ($1,$2,$3,$4::jsonb, now())
		ON CONFLICT (provider, provider_event_id) DO NOTHING`,
		provider, eventID, eventType, string(payload)); err != nil {
		return false, err
	}
	var processed *time.Time
	if err = p.QueryRow(ctx,
		`SELECT processed_at FROM billing_events WHERE provider = $1 AND provider_event_id = $2`,
		provider, eventID).Scan(&processed); err != nil {
		return false, err
	}
	return processed != nil, nil
}

// MarkBillingEventProcessed stamps an event handled, so a later redelivery is skipped.
func (p *Pool) MarkBillingEventProcessed(ctx context.Context, provider, eventID string) error {
	_, err := p.Exec(ctx,
		`UPDATE billing_events SET processed_at = now() WHERE provider = $1 AND provider_event_id = $2`,
		provider, eventID)
	return err
}

// OrgByCustomer maps a provider customer id back to its org, for the webhook ingest
// before app.current_org is known (RFC-018 Phase 3 follow-on events that carry only
// the customer id). It calls the subscription_org_by_customer SECURITY DEFINER
// function, which bypasses subscriptions RLS and returns a scalar org id only. Returns
// 0 (and no error) when no subscription matches, so the caller can fall back.
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

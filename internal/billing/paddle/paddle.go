// Package paddle is the Paddle (Merchant of Record) billing adapter (RFC-018 2),
// built on the official SDK github.com/PaddleHQ/paddle-go-sdk. It implements the
// billing.Provider seam so the rest of the app stays provider-agnostic.
//
// What's implemented against the live API: Checkout (create a transaction and return
// its hosted checkout URL), VerifyWebhook (SDK signature verifier + event mapping to
// billing.Event), CancelSubscription, and full Refund (a Paddle adjustment). Two
// operator-only flows are not wired yet and return billing.ErrNotImplemented, which the
// api treats gracefully (it still applies the local override): UpdateSubscription (the
// PatchField/proration plan move) and SetCustomPrice (creating a per-org non-catalog
// price). PortalURL also returns ErrNotImplemented until we store the Paddle customer id
// per org. Partial refunds need transaction line-item amounts, so they error for now;
// full refunds work.
package paddle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	paddle "github.com/PaddleHQ/paddle-go-sdk/v5"

	"pulse/internal/billing"
	"pulse/internal/domain"
)

// PriceMap builds the "<plan>:<cycle>" -> price_id map New expects from plan_prices rows.
func PriceMap(rows []*domain.PlanPrice) map[string]string {
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Plan+":"+r.Cycle] = r.ProviderPriceID
	}
	return m
}

// Config wires the adapter. Prices maps "<plan>:<cycle>" (e.g. "tier3:monthly") to the
// Paddle price id, loaded from plan_prices. BaseURL is empty for production.
type Config struct {
	APIKey        string
	BaseURL       string
	WebhookSecret string
	Prices        map[string]string
}

// Provider is the Paddle adapter.
type Provider struct {
	sdk      *paddle.SDK
	verifier *paddle.WebhookVerifier
	prices   map[string]string    // plan:cycle -> price_id
	byPrice  map[string]planCycle // price_id -> plan/cycle (reverse, for webhook mapping)
}

type planCycle struct{ plan, cycle string }

var _ billing.Provider = (*Provider)(nil)

// New builds the adapter. Defaults to the production base URL when none is given.
func New(cfg Config) (*Provider, error) {
	base := cfg.BaseURL
	if base == "" {
		base = paddle.ProductionBaseURL
	}
	sdk, err := paddle.New(cfg.APIKey, paddle.WithBaseURL(base))
	if err != nil {
		return nil, fmt.Errorf("paddle: %w", err)
	}
	byPrice := make(map[string]planCycle, len(cfg.Prices))
	for key, id := range cfg.Prices {
		plan, cycle, _ := strings.Cut(key, ":")
		byPrice[id] = planCycle{plan: plan, cycle: cycle}
	}
	return &Provider{
		sdk:      sdk,
		verifier: paddle.NewWebhookVerifier(cfg.WebhookSecret),
		prices:   cfg.Prices,
		byPrice:  byPrice,
	}, nil
}

// Name is the provider identifier used on the webhook path and stored on rows.
func (p *Provider) Name() string { return "paddle" }

// SignatureHeader is the header Paddle sends its webhook signature in.
func (p *Provider) SignatureHeader() string { return "Paddle-Signature" }

// Checkout creates an automatically-collected transaction for the plan's price and
// returns its hosted checkout URL. org_id rides along in custom_data so the webhook can
// tie the resulting subscription back to the org. The subscription is created by Paddle
// once the customer pays; our webhook then syncs it.
func (p *Provider) Checkout(ctx context.Context, orgID int64, plan, cycle string) (string, error) {
	priceID, ok := p.prices[plan+":"+cycle]
	if !ok {
		return "", fmt.Errorf("paddle: no price configured for %s/%s", plan, cycle)
	}
	mode := paddle.CollectionModeAutomatic
	txn, err := p.sdk.CreateTransaction(ctx, &paddle.CreateTransactionRequest{
		Items: []paddle.CreateTransactionItems{
			*paddle.NewCreateTransactionItemsTransactionItemFromCatalog(
				&paddle.TransactionItemFromCatalog{PriceID: priceID, Quantity: 1}),
		},
		CollectionMode: &mode,
		CustomData:     paddle.CustomData{"org_id": strconv.FormatInt(orgID, 10)},
		// nil URL => Paddle uses the account's default payment URL.
		Checkout: &paddle.TransactionCheckout{URL: nil},
	})
	if err != nil {
		return "", fmt.Errorf("paddle: create transaction: %w", err)
	}
	if txn.Checkout == nil || txn.Checkout.URL == nil || *txn.Checkout.URL == "" {
		return "", errors.New("paddle: no checkout url returned (set a default payment link and approve your domain)")
	}
	return *txn.Checkout.URL, nil
}

// CancelSubscription cancels at period end (default) or immediately.
func (p *Provider) CancelSubscription(ctx context.Context, providerSubID string, when billing.CancelWhen) error {
	eff := paddle.EffectiveFromNextBillingPeriod
	if when == billing.CancelImmediate {
		eff = paddle.EffectiveFromImmediately
	}
	_, err := p.sdk.CancelSubscription(ctx, &paddle.CancelSubscriptionRequest{
		SubscriptionID: providerSubID,
		EffectiveFrom:  &eff,
	})
	if err != nil {
		return fmt.Errorf("paddle: cancel subscription: %w", err)
	}
	return nil
}

// Refund issues a full refund for a transaction via a Paddle adjustment. providerPaymentID
// is the Paddle transaction id (txn_) we mirror in payments. Partial refunds need per-item
// amounts, so they are rejected for now.
func (p *Provider) Refund(ctx context.Context, providerPaymentID string, amount *billing.Money, reason string) error {
	if amount != nil {
		return errors.New("paddle: partial refunds are not supported yet (full refunds only)")
	}
	if reason == "" {
		reason = "requested by operator"
	}
	full := paddle.AdjustmentTypeFull
	_, err := p.sdk.CreateAdjustment(ctx, &paddle.CreateAdjustmentRequest{
		Action:        paddle.AdjustmentActionRefund,
		TransactionID: providerPaymentID,
		Type:          &full,
		Reason:        reason,
	})
	if err != nil {
		return fmt.Errorf("paddle: create refund adjustment: %w", err)
	}
	return nil
}

// PortalURL is not wired yet: it needs the Paddle customer id for the org, which we only
// learn from the first webhook. Returns ErrNotImplemented so the api degrades gracefully.
func (p *Provider) PortalURL(ctx context.Context, orgID int64) (string, error) {
	return "", billing.ErrNotImplemented
}

// UpdateSubscription (operator plan move) uses Paddle's PatchField/proration flow; not
// wired yet. The api still applies the local override when this returns ErrNotImplemented.
func (p *Provider) UpdateSubscription(ctx context.Context, providerSubID string, target billing.PlanChange) error {
	return billing.ErrNotImplemented
}

// SetCustomPrice (per-org non-catalog price for the Custom tier) is not wired yet.
func (p *Provider) SetCustomPrice(ctx context.Context, orgID int64, amount billing.Money, cycle string) (string, error) {
	return "", billing.ErrNotImplemented
}

// VerifyWebhook verifies the Paddle-Signature using the SDK verifier, then maps the
// event into a billing.Event. The signature scheme (and the verifier's replay window)
// is the SDK's, so we do not reimplement it.
func (p *Provider) VerifyWebhook(payload []byte, sig string) (billing.Event, error) {
	// The SDK verifier reads the raw body + Paddle-Signature header off a request, so
	// reconstruct one from what the ingest already read.
	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
	if err != nil {
		return billing.Event{}, err
	}
	req.Header.Set(p.SignatureHeader(), sig)
	ok, verr := p.verifier.Verify(req)
	if verr != nil || !ok {
		return billing.Event{}, billing.ErrBadSignature
	}

	var env struct {
		EventID   string          `json:"event_id"`
		EventType string          `json:"event_type"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return billing.Event{}, fmt.Errorf("paddle: webhook body: %w", err)
	}

	ev := billing.Event{ID: env.EventID, Type: env.EventType, Provider: p.Name()}
	// Strict, exact classification against the SDK's typed event names (payment code:
	// no prefix matching). Anything not in these sets stays EventKindUnknown.
	switch name := paddle.EventTypeName(env.EventType); {
	case subscriptionEvents[name]:
		ev.Kind = billing.EventKindSubscription
		p.mapSubscription(env.Data, &ev)
	case paymentEvents[name]:
		ev.Kind = billing.EventKindPayment
		p.mapTransaction(env.Data, &ev)
	}
	return ev, nil
}

// subscriptionEvents are the exact Paddle events that carry the subscription's current
// state, so we sync the org's subscription + plan on any of them.
var subscriptionEvents = map[paddle.EventTypeName]bool{
	paddle.EventTypeNameSubscriptionCreated:   true,
	paddle.EventTypeNameSubscriptionActivated: true,
	paddle.EventTypeNameSubscriptionUpdated:   true,
	paddle.EventTypeNameSubscriptionCanceled:  true,
	paddle.EventTypeNameSubscriptionPastDue:   true,
	paddle.EventTypeNameSubscriptionPaused:    true,
	paddle.EventTypeNameSubscriptionResumed:   true,
	paddle.EventTypeNameSubscriptionTrialing:  true,
	paddle.EventTypeNameSubscriptionImported:  true,
}

// paymentEvents are the exact Paddle transaction events that represent money captured,
// which we mirror into payments. Other transaction.* events (created, billed, canceled,
// payment_failed, ...) are stored but not mirrored. Refunds arrive as adjustment.* and
// are not mirrored yet.
var paymentEvents = map[paddle.EventTypeName]bool{
	paddle.EventTypeNameTransactionCompleted: true,
	paddle.EventTypeNameTransactionPaid:      true,
}

func (p *Provider) mapSubscription(data json.RawMessage, ev *billing.Event) {
	var s struct {
		ID                   string         `json:"id"`
		Status               string         `json:"status"`
		CustomerID           string         `json:"customer_id"`
		CustomData           map[string]any `json:"custom_data"`
		CurrentBillingPeriod *struct {
			EndsAt time.Time `json:"ends_at"`
		} `json:"current_billing_period"`
		ScheduledChange *struct {
			Action string `json:"action"`
		} `json:"scheduled_change"`
		Items []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return
	}
	ev.OrgID = orgIDFromCustomData(s.CustomData)
	ev.ProviderCustomerID = s.CustomerID
	ev.ProviderSubscriptionID = s.ID
	ev.Status = s.Status
	if len(s.Items) > 0 {
		ev.ProviderPriceID = s.Items[0].Price.ID
		if pc, ok := p.byPrice[ev.ProviderPriceID]; ok {
			ev.Plan = pc.plan
			ev.Cycle = pc.cycle
		}
	}
	if s.CurrentBillingPeriod != nil {
		t := s.CurrentBillingPeriod.EndsAt
		ev.CurrentPeriodEnd = &t
	}
	ev.CancelAtPeriodEnd = s.ScheduledChange != nil && s.ScheduledChange.Action == "cancel"
}

func (p *Provider) mapTransaction(data json.RawMessage, ev *billing.Event) {
	var t struct {
		ID         string         `json:"id"`
		Status     string         `json:"status"`
		CustomerID string         `json:"customer_id"`
		CustomData map[string]any `json:"custom_data"`
		Details    *struct {
			Totals *struct {
				GrandTotal   string `json:"grand_total"`
				CurrencyCode string `json:"currency_code"`
			} `json:"totals"`
		} `json:"details"`
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return
	}
	ev.OrgID = orgIDFromCustomData(t.CustomData)
	ev.ProviderCustomerID = t.CustomerID
	pay := &billing.EventPayment{ProviderPaymentID: t.ID, Status: t.Status}
	if t.Details != nil && t.Details.Totals != nil {
		minor, _ := strconv.ParseInt(t.Details.Totals.GrandTotal, 10, 64)
		pay.Amount = billing.Money{Minor: minor, Currency: t.Details.Totals.CurrencyCode}
	}
	ev.Payment = pay
}

// orgIDFromCustomData reads org_id out of Paddle custom_data, tolerating a string (how
// we set it) or a JSON number. Returns 0 when absent (the ingest then resolves by
// customer id).
func orgIDFromCustomData(cd map[string]any) int64 {
	v, ok := cd["org_id"]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case float64:
		return int64(x)
	default:
		return 0
	}
}

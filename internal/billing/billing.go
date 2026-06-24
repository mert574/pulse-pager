// Package billing is the provider-agnostic core for recurring payments (RFC-018).
// Pulse owns entitlement enforcement; the provider owns money movement, tax,
// invoices, proration, and dunning. Everything outside this package depends only on
// the Provider interface, so swapping Paddle for another provider is a new adapter,
// not a redesign (same seam as PULSE_BUS).
//
// Phase 1 ships the interface, a working stub adapter (the whole webhook sync path is
// testable without a real provider account), and a Paddle skeleton. Only VerifyWebhook
// is exercised in Phase 1; the operator/self-serve methods land in Phases 2-3.
package billing

import (
	"context"
	"errors"
	"time"
)

// ErrNotImplemented is returned by adapter methods that are not wired yet (the
// operator and self-serve calls, RFC-018 Phases 2-3). Phase 1 only uses VerifyWebhook.
var ErrNotImplemented = errors.New("billing: not implemented in phase 1")

// Money is an amount in minor units (cents) plus its ISO 4217 currency. Money is
// always mirrored from the provider, never computed in Pulse (RFC-018 8), so this is
// an integer, never a float.
type Money struct {
	Minor    int64
	Currency string
}

// CancelWhen says when a cancellation takes effect (RFC-018 5.2). Default period_end.
type CancelWhen string

const (
	CancelImmediate CancelWhen = "immediate"
	CancelPeriodEnd CancelWhen = "period_end"
)

// PlanChange is the target of an operator/self-serve plan move (RFC-018 5.1). Mode
// decides proration; it only matters for the paid case (Phase 2).
type PlanChange struct {
	Plan  string // tier1..tierCustom
	Cycle string // monthly | annual
	Mode  string // prorate_now | next_cycle
}

// Event is the normalized webhook payload every adapter produces from a provider
// callback. It is the one shape the ingest path understands, so adapters absorb each
// provider's wire format. OrgID comes from the provider's custom_data when present; a
// zero OrgID means "resolve via ProviderCustomerID" (a follow-on event that carries
// only the customer id, RFC-018 Phase 3).
type Event struct {
	ID       string // provider event id, the dedup anchor
	Type     string // e.g. subscription.activated, subscription.canceled, payment.succeeded
	Provider string // stub | paddle

	OrgID int64

	ProviderCustomerID     string
	ProviderSubscriptionID string
	ProviderPriceID        string

	Plan              string // tier1..tierCustom (validated against entitlements before persist)
	Cycle             string // monthly | annual
	Status            string // trialing | active | past_due | canceled
	CurrentPeriodEnd  *time.Time
	CancelAtPeriodEnd bool

	// Payment is set on payment events; the payments mirror is Phase 4, so Phase 1
	// ignores it. Kept here so the wire contract is stable across phases.
	Payment *EventPayment
}

// EventPayment is the money side of a payment event (mirrored only).
type EventPayment struct {
	ProviderPaymentID string
	Amount            Money
	Status            string
	Period            string
	HostedInvoiceURL  string
	RefundedAmount    Money
}

// Provider is the seam every adapter implements (RFC-018 3). The rest of the app
// depends only on this. Phase 1 only calls VerifyWebhook; the rest return
// ErrNotImplemented in the stub and the Paddle skeleton until their phases land.
type Provider interface {
	// Name is the provider id used on the webhook path and stored on rows (stub|paddle).
	Name() string
	// SignatureHeader is the HTTP header the webhook signature arrives in, so the
	// ingest reads the right one per provider.
	SignatureHeader() string

	Checkout(ctx context.Context, orgID int64, plan, cycle string) (url string, err error)
	PortalURL(ctx context.Context, orgID int64) (string, error)
	UpdateSubscription(ctx context.Context, providerSubID string, target PlanChange) error
	CancelSubscription(ctx context.Context, providerSubID string, when CancelWhen) error
	Refund(ctx context.Context, providerPaymentID string, amount *Money, reason string) error
	SetCustomPrice(ctx context.Context, orgID int64, amount Money, cycle string) (priceRef string, err error)
	VerifyWebhook(payload []byte, sig string) (Event, error)
}

// Package paddle is the Paddle (Merchant of Record) adapter for billing (RFC-018 2).
// Skeleton only: it implements the billing.Provider seam so the real Billing API
// client drops in here without touching callers. Every method is a TODO until the
// Paddle account and credentials are wired; the stub adapter carries Phase 1.
//
// Verification status: the webhook signature scheme in VerifyWebhook is verified
// against Paddle's current docs (cited there). The request/response shapes the other
// methods sketch (transactions for checkout, non-catalog prices, refunds) are NOT yet
// verified against the live Paddle Billing API — confirm each against the docs when the
// adapter is actually implemented rather than trusting these notes.
package paddle

import (
	"context"

	"pulse/internal/billing"
)

// Provider is the Paddle adapter. webhookSecret verifies the Paddle-Signature header.
type Provider struct {
	apiKey        string
	webhookSecret string
}

// New builds the Paddle adapter from its API key and webhook signing secret.
func New(apiKey, webhookSecret string) *Provider {
	return &Provider{apiKey: apiKey, webhookSecret: webhookSecret}
}

var _ billing.Provider = (*Provider)(nil)

// Name is the provider identifier used on the webhook path and stored on rows.
func (p *Provider) Name() string { return "paddle" }

// SignatureHeader is the header Paddle sends its webhook signature in.
func (p *Provider) SignatureHeader() string { return "Paddle-Signature" }

// VerifyWebhook will verify the Paddle-Signature header and map the event into a
// billing.Event. Scheme (verified against Paddle's docs, not guessed —
// https://developer.paddle.com/webhooks/signature-verification): the header is
// "ts=<unix>;h1=<hmac>"; h1 is HMAC-SHA256 over the string "<ts>:<raw_body>" (the
// timestamp and the RAW request body joined with a colon), keyed by the notification
// destination secret (prefixed pdl_ntfset_); compare with hmac.Equal and reject a ts
// older than ~5s (Paddle's default replay window). Read the raw body once, verify, then
// unmarshal.
func (p *Provider) VerifyWebhook(payload []byte, sig string) (billing.Event, error) {
	// TODO(RFC-018): implement the scheme documented above and map the Paddle event
	// schema into billing.Event.
	return billing.Event{}, billing.ErrNotImplemented
}

func (p *Provider) Checkout(ctx context.Context, orgID int64, plan, cycle string) (string, error) {
	// TODO(RFC-018): create a Paddle transaction / hosted checkout url.
	return "", billing.ErrNotImplemented
}

func (p *Provider) PortalURL(ctx context.Context, orgID int64) (string, error) {
	// TODO(RFC-018): return the Paddle customer portal url.
	return "", billing.ErrNotImplemented
}

func (p *Provider) UpdateSubscription(ctx context.Context, providerSubID string, target billing.PlanChange) error {
	// TODO(RFC-018): switch the subscription price (operator/self-serve plan move).
	return billing.ErrNotImplemented
}

func (p *Provider) CancelSubscription(ctx context.Context, providerSubID string, when billing.CancelWhen) error {
	// TODO(RFC-018): cancel immediately or at period end.
	return billing.ErrNotImplemented
}

func (p *Provider) Refund(ctx context.Context, providerPaymentID string, amount *billing.Money, reason string) error {
	// TODO(RFC-018): full or partial refund (MoR also reverses tax).
	return billing.ErrNotImplemented
}

func (p *Provider) SetCustomPrice(ctx context.Context, orgID int64, amount billing.Money, cycle string) (string, error) {
	// TODO(RFC-018): create a per-org non-catalog price for a Custom org.
	return "", billing.ErrNotImplemented
}

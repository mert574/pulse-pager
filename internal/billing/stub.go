package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Stub is a self-contained billing adapter for dev and tests. It implements the full
// VerifyWebhook so the whole sync path (webhook -> DB -> entitlements) runs without a
// real provider account; the operator/self-serve methods return ErrNotImplemented
// (they are Phases 2-3). The webhook signature mirrors Paddle's header shape
// "ts=<unix>;h1=<hex>" where the hex is hmac-sha256(secret, ts + "." + body), so the
// ingest's verify-before-parse handling is the same against the real adapter later.
type Stub struct {
	secret string
}

// NewStub builds the stub adapter with the webhook signing secret.
func NewStub(secret string) *Stub { return &Stub{secret: secret} }

var _ Provider = (*Stub)(nil)

// Name is the provider identifier used on the webhook path and stored on rows.
func (s *Stub) Name() string { return "stub" }

// StubSignatureHeader is the header the stub webhook signature arrives in.
const StubSignatureHeader = "X-Billing-Signature"

// SignatureHeader is the header the ingest reads the stub signature from.
func (s *Stub) SignatureHeader() string { return StubSignatureHeader }

// stubEnvelope is the stub's wire format. The real Paddle adapter parses Paddle's
// schema into the same Event; tests build this shape and sign it.
type stubEnvelope struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	CustomData struct {
		OrgID int64 `json:"org_id"`
	} `json:"custom_data"`
	Data struct {
		CustomerID        string     `json:"customer_id"`
		SubscriptionID    string     `json:"subscription_id"`
		PriceID           string     `json:"price_id"`
		Plan              string     `json:"plan"`
		Cycle             string     `json:"cycle"`
		Status            string     `json:"status"`
		CurrentPeriodEnd  *time.Time `json:"current_period_end"`
		CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	} `json:"data"`
	// Payment is set on payment.* events (RFC-018 4); nil on subscription events.
	Payment *struct {
		PaymentID        string `json:"payment_id"`
		Amount           int64  `json:"amount"`
		Currency         string `json:"currency"`
		Status           string `json:"status"`
		Period           string `json:"period"`
		HostedInvoiceURL string `json:"hosted_invoice_url"`
		RefundedAmount   int64  `json:"refunded_amount"`
	} `json:"payment"`
}

// VerifyWebhook checks the signature over the raw body, then parses the envelope into
// a normalized Event. It returns ErrBadSignature on a missing/forged signature so the
// ingest can answer 401, and a parse error (mapped to 400) on a malformed body.
func (s *Stub) VerifyWebhook(payload []byte, sig string) (Event, error) {
	ts, mac, err := parseStubSig(sig)
	if err != nil {
		return Event{}, err
	}
	expected := stubSign(s.secret, ts, payload)
	// Constant-time compare so a wrong signature can't be timed byte by byte.
	if !hmac.Equal([]byte(expected), []byte(mac)) {
		return Event{}, ErrBadSignature
	}

	var env stubEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Event{}, fmt.Errorf("billing: stub webhook body: %w", err)
	}
	ev := Event{
		ID:                     env.ID,
		Type:                   env.Type,
		Kind:                   stubEventKind(env.Type),
		Provider:               s.Name(),
		OrgID:                  env.CustomData.OrgID,
		ProviderCustomerID:     env.Data.CustomerID,
		ProviderSubscriptionID: env.Data.SubscriptionID,
		ProviderPriceID:        env.Data.PriceID,
		Plan:                   env.Data.Plan,
		Cycle:                  env.Data.Cycle,
		Status:                 env.Data.Status,
		CurrentPeriodEnd:       env.Data.CurrentPeriodEnd,
		CancelAtPeriodEnd:      env.Data.CancelAtPeriodEnd,
	}
	if env.Payment != nil {
		ev.Payment = &EventPayment{
			ProviderPaymentID: env.Payment.PaymentID,
			Amount:            Money{Minor: env.Payment.Amount, Currency: env.Payment.Currency},
			Status:            env.Payment.Status,
			Period:            env.Payment.Period,
			HostedInvoiceURL:  env.Payment.HostedInvoiceURL,
			RefundedAmount:    Money{Minor: env.Payment.RefundedAmount, Currency: env.Payment.Currency},
		}
	}
	return ev, nil
}

// ErrBadSignature is returned when the webhook signature is missing or does not match.
var ErrBadSignature = errors.New("billing: bad webhook signature")

// The stub mirrors Paddle's event vocabulary so tests are realistic. Exact sets, matched
// exactly (no prefixes): a type not in either set is EventKindUnknown (stored only).
var stubSubscriptionEvents = map[string]bool{
	"subscription.created":   true,
	"subscription.activated": true,
	"subscription.updated":   true,
	"subscription.canceled":  true,
	"subscription.past_due":  true,
	"subscription.paused":    true,
	"subscription.resumed":   true,
	"subscription.trialing":  true,
	"subscription.imported":  true,
}

var stubPaymentEvents = map[string]bool{
	"transaction.completed": true,
	"transaction.paid":      true,
}

func stubEventKind(eventType string) EventKind {
	switch {
	case stubSubscriptionEvents[eventType]:
		return EventKindSubscription
	case stubPaymentEvents[eventType]:
		return EventKindPayment
	default:
		return EventKindUnknown
	}
}

// parseStubSig reads the "ts=<unix>;h1=<hex>" header into its parts. A missing or
// malformed header is a bad signature (the ingest answers 401), never a pass.
func parseStubSig(sig string) (ts, mac string, err error) {
	for _, part := range strings.Split(sig, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "ts":
			ts = v
		case "h1":
			mac = v
		}
	}
	if ts == "" || mac == "" {
		return "", "", ErrBadSignature
	}
	return ts, mac, nil
}

// stubSign computes the hex hmac-sha256 over ts + "." + body. The ts is bound into the
// signature so it can't be altered; the real adapter additionally enforces a max age.
func stubSign(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// SignStubWebhook builds the header value a stub webhook caller sends, for tests and
// the dev curl smoke. ts is a unix-seconds string.
func SignStubWebhook(secret, ts string, body []byte) string {
	return "ts=" + ts + ";h1=" + stubSign(secret, ts, body)
}

// NowUnix is a tiny helper so tests/callers can stamp the signature timestamp.
func NowUnix() string { return strconv.FormatInt(time.Now().Unix(), 10) }

// The operator/self-serve methods below are functional stubs: they return plausible
// values (urls, a price ref) and succeed, so the whole flow (operator move, cancel,
// refund, checkout, portal) is testable end to end without a provider account. They do
// NOT move money; the real Paddle adapter does. Tests assert the call happened and the
// local state changed; the webhook is what reconciles real provider state.

// stubBaseURL is the fake hosted surface the stub points checkout/portal at.
const stubBaseURL = "https://stub.billing.local"

// Checkout returns a fake hosted-checkout url carrying the org/plan/cycle.
func (s *Stub) Checkout(_ context.Context, orgID int64, plan, cycle string) (string, error) {
	return fmt.Sprintf("%s/checkout?org=%d&plan=%s&cycle=%s", stubBaseURL, orgID, plan, cycle), nil
}

// PortalURL returns a fake customer-portal url for the org.
func (s *Stub) PortalURL(_ context.Context, orgID int64) (string, error) {
	return fmt.Sprintf("%s/portal?org=%d", stubBaseURL, orgID), nil
}

// UpdateSubscription pretends to switch the subscription price; the webhook reconciles.
func (s *Stub) UpdateSubscription(context.Context, string, PlanChange) error { return nil }

// CancelSubscription pretends to cancel; the webhook reconciles to canceled/free.
func (s *Stub) CancelSubscription(context.Context, string, CancelWhen) error { return nil }

// Refund pretends to refund; with a real MoR this also reverses tax.
func (s *Stub) Refund(context.Context, string, *Money, string) error { return nil }

// SetCustomPrice returns a fake per-org price ref derived from the amount (RFC-018 7).
func (s *Stub) SetCustomPrice(_ context.Context, orgID int64, amount Money, cycle string) (string, error) {
	return fmt.Sprintf("price_custom_%d_%d%s_%s", orgID, amount.Minor, strings.ToLower(amount.Currency), cycle), nil
}

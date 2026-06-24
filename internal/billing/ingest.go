package billing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/store"
)

// maxWebhookBody caps the request body. The endpoint is unauthenticated (only the
// signature gates it), so an unbounded read would be a memory-abuse vector.
const maxWebhookBody = 1 << 20 // 1 MiB

// EventApplier is the slice of the store the ingest needs. Kept narrow so the handler
// is unit-testable with a fake (store.Pool satisfies it).
type EventApplier interface {
	ApplySubscriptionEvent(ctx context.Context, providerEventID, eventType string, sub *domain.Subscription) (bool, error)
	ApplyPaymentEvent(ctx context.Context, providerEventID, eventType string, pay *domain.Payment) (bool, error)
	OrgByCustomer(ctx context.Context, provider, customerID string) (int64, error)
}

// Handler serves the signature-verified billing webhook. It is hand-wired outside the
// generated JSON contract (like /auth/*) and is the authoritative sync path: it
// verifies, normalizes via the provider adapter, and applies state atomically.
type Handler struct {
	provider Provider
	store    EventApplier
	log      *slog.Logger
}

// NewHandler wires the ingest with its configured provider and store.
func NewHandler(p Provider, s EventApplier, log *slog.Logger) *Handler {
	return &Handler{provider: p, store: s, log: log}
}

// ServeHTTP handles POST /billing/webhooks/{provider}. It answers 200 on success and
// on an ignored or already-processed event, 401 on a bad/missing signature, 400 on a
// malformed body, and 500 only on a transient error worth a provider retry.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.PathValue("provider") != h.provider.Name() {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	sig := r.Header.Get(h.provider.SignatureHeader())
	ev, err := h.provider.VerifyWebhook(body, sig)
	if err != nil {
		// A signature problem is a 401; anything else (a malformed body the adapter
		// could not parse) is a 400. Never log the payload.
		if errors.Is(err, ErrBadSignature) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		http.Error(w, "bad webhook", http.StatusBadRequest)
		return
	}

	// Subscription events drive the plan; payment events feed the mirror (RFC-018 4).
	// Anything else is acknowledged so the provider does not retry it.
	isSub := strings.HasPrefix(ev.Type, "subscription.")
	isPay := !isSub && ev.Payment != nil
	if !isSub && !isPay {
		h.log.Info("billing webhook ignored", "provider", ev.Provider, "event_id", ev.ID, "type", ev.Type)
		w.WriteHeader(http.StatusOK)
		return
	}

	orgID := ev.OrgID
	if orgID == 0 {
		orgID, err = h.store.OrgByCustomer(ctx, ev.Provider, ev.ProviderCustomerID)
		if err != nil {
			h.log.Error("billing webhook org lookup", "provider", ev.Provider, "event_id", ev.ID, "err", err)
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		if orgID == 0 {
			// Can't tie the event to an org and it isn't transient; ack so it stops.
			h.log.Warn("billing webhook unresolved org", "provider", ev.Provider, "event_id", ev.ID, "customer", ev.ProviderCustomerID)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	var applied bool
	if isSub {
		// Run the provider plan through ParsePlan so a bad value fails safe to free
		// rather than writing junk into organizations.plan. A canceled subscription
		// drops the org to free (RFC-018 5.2); other statuses keep the subscribed plan.
		plan := entitlements.ParsePlan(ev.Plan)
		if ev.Status == "canceled" {
			plan = entitlements.PlanTier1
		}
		applied, err = h.store.ApplySubscriptionEvent(ctx, ev.ID, ev.Type, &domain.Subscription{
			OrgID:                  orgID,
			Plan:                   string(plan),
			BillingCycle:           ev.Cycle,
			Status:                 ev.Status,
			Provider:               ev.Provider,
			ProviderCustomerID:     ev.ProviderCustomerID,
			ProviderSubscriptionID: ev.ProviderSubscriptionID,
			ProviderPriceID:        ev.ProviderPriceID,
			CurrentPeriodEnd:       ev.CurrentPeriodEnd,
			CancelAtPeriodEnd:      ev.CancelAtPeriodEnd,
		})
	} else {
		applied, err = h.store.ApplyPaymentEvent(ctx, ev.ID, ev.Type, &domain.Payment{
			OrgID:             orgID,
			Provider:          ev.Provider,
			ProviderPaymentID: ev.Payment.ProviderPaymentID,
			Amount:            ev.Payment.Amount.Minor,
			Currency:          ev.Payment.Amount.Currency,
			Status:            ev.Payment.Status,
			Period:            ev.Payment.Period,
			HostedInvoiceURL:  ev.Payment.HostedInvoiceURL,
			RefundedAmount:    ev.Payment.RefundedAmount.Minor,
		})
	}
	if err != nil {
		if errors.Is(err, store.ErrOrgNotFound) {
			// Permanent: the org is gone or soft-deleted. Ack so the provider stops.
			h.log.Warn("billing webhook org gone", "provider", ev.Provider, "event_id", ev.ID, "org_id", orgID)
			w.WriteHeader(http.StatusOK)
			return
		}
		h.log.Error("billing webhook apply", "provider", ev.Provider, "event_id", ev.ID, "org_id", orgID, "err", err)
		http.Error(w, "apply failed", http.StatusInternalServerError)
		return
	}

	h.log.Info("billing webhook handled", "provider", ev.Provider, "event_id", ev.ID,
		"type", ev.Type, "org_id", orgID, "applied", applied)
	w.WriteHeader(http.StatusOK)
}

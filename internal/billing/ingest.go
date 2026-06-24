package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

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
	// RecordBillingEvent saves the raw verified event (every type) before we act, and
	// reports whether a prior delivery was already processed (so we skip re-acting).
	RecordBillingEvent(ctx context.Context, provider, eventID, eventType string, payload []byte) (alreadyProcessed bool, err error)
	// MarkBillingEventProcessed stamps an event handled once we have acted on it.
	MarkBillingEventProcessed(ctx context.Context, provider, eventID string) error
	ApplySubscriptionEvent(ctx context.Context, sub *domain.Subscription) error
	ApplyPaymentEvent(ctx context.Context, pay *domain.Payment) error
	OrgByCustomer(ctx context.Context, provider, customerID string) (int64, error)
}

// Handler serves the signature-verified billing webhook. It is hand-wired outside the
// generated JSON contract (like /auth/*) and is the authoritative sync path: it
// verifies, persists the raw event, then applies state.
type Handler struct {
	provider Provider
	store    EventApplier
	log      *slog.Logger
}

// NewHandler wires the ingest with its configured provider and store.
func NewHandler(p Provider, s EventApplier, log *slog.Logger) *Handler {
	return &Handler{provider: p, store: s, log: log}
}

// ServeHTTP handles POST /billing/webhooks/{provider}. The flow is verify -> record the
// raw event (every type) -> act on the types we handle -> mark processed. It answers 200
// on success and on an ignored or already-processed event, 401 on a bad/missing
// signature, 400 on a malformed body, and 500 only on a transient error worth a retry.
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

	// Authenticity first: only verified events are persisted or acted on.
	sig := r.Header.Get(h.provider.SignatureHeader())
	ev, err := h.provider.VerifyWebhook(body, sig)
	if err != nil {
		if errors.Is(err, ErrBadSignature) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		http.Error(w, "bad webhook", http.StatusBadRequest)
		return
	}
	if ev.ID == "" {
		// No event id means we can't dedup or record it safely; ack so it stops.
		h.log.Warn("billing webhook missing event id", "provider", ev.Provider, "type", ev.Type)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Persist the raw event for EVERY type before acting (RFC-018 8). Idempotent: a
	// redelivery that was already processed short-circuits here.
	already, err := h.store.RecordBillingEvent(ctx, ev.Provider, ev.ID, ev.Type, body)
	if err != nil {
		h.log.Error("billing webhook record", "provider", ev.Provider, "event_id", ev.ID, "err", err)
		http.Error(w, "record failed", http.StatusInternalServerError)
		return
	}
	if already {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Act on the types we handle. Unknown types are stored above and need no action.
	switch err := h.act(ctx, ev); {
	case err == nil:
		if merr := h.store.MarkBillingEventProcessed(ctx, ev.Provider, ev.ID); merr != nil {
			// The state change succeeded; failing to stamp it only risks a harmless
			// reprocess (the applies are idempotent), so retry rather than lose it.
			h.log.Error("billing webhook mark processed", "provider", ev.Provider, "event_id", ev.ID, "err", merr)
			http.Error(w, "mark failed", http.StatusInternalServerError)
			return
		}
		h.log.Info("billing webhook handled", "provider", ev.Provider, "event_id", ev.ID, "type", ev.Type)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, store.ErrOrgNotFound):
		// Permanent: the org is gone. Stamp processed and ack so the provider stops.
		h.log.Warn("billing webhook org gone", "provider", ev.Provider, "event_id", ev.ID, "type", ev.Type)
		_ = h.store.MarkBillingEventProcessed(ctx, ev.Provider, ev.ID)
		w.WriteHeader(http.StatusOK)
	default:
		// Transient: leave it unprocessed so the redelivery retries.
		h.log.Error("billing webhook apply", "provider", ev.Provider, "event_id", ev.ID, "type", ev.Type, "err", err)
		http.Error(w, "apply failed", http.StatusInternalServerError)
	}
}

// act applies the event for the kinds we handle. The adapter has already classified the
// event by exact-matching the provider's event type, so we switch on that kind, never on
// a string prefix or the presence of a payload. Unknown kinds (and events we can't tie to
// an org) return nil, so they are acknowledged with the raw event already stored.
func (h *Handler) act(ctx context.Context, ev Event) error {
	if ev.Kind != EventKindSubscription && ev.Kind != EventKindPayment {
		return nil
	}

	orgID := ev.OrgID
	if orgID == 0 {
		var err error
		orgID, err = h.store.OrgByCustomer(ctx, ev.Provider, ev.ProviderCustomerID)
		if err != nil {
			return err // transient lookup failure -> retry
		}
		if orgID == 0 {
			h.log.Warn("billing webhook unresolved org", "provider", ev.Provider, "event_id", ev.ID, "customer", ev.ProviderCustomerID)
			return nil // can't resolve; acknowledged (raw event stored for debugging)
		}
	}

	switch ev.Kind {
	case EventKindSubscription:
		// Run the provider plan through ParsePlan so a bad value fails safe to free
		// rather than writing junk into organizations.plan. A canceled subscription
		// drops the org to free (RFC-018 5.2); other statuses keep the subscribed plan.
		plan := entitlements.ParsePlan(ev.Plan)
		if ev.Status == "canceled" {
			plan = entitlements.PlanTier1
		}
		return h.store.ApplySubscriptionEvent(ctx, &domain.Subscription{
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
	case EventKindPayment:
		if ev.Payment == nil {
			// A payment kind with no payment body is a contract violation; don't guess.
			return fmt.Errorf("billing: payment event %s has no payment data", ev.ID)
		}
		return h.store.ApplyPaymentEvent(ctx, &domain.Payment{
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
	default:
		return nil
	}
}

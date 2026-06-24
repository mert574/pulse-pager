package api

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authn"
	"pulse/internal/authz"
	"pulse/internal/billing"
	"pulse/internal/domain"
	"pulse/internal/events"
)

// AuditPublisher emits an audit.events record for an operator action (RFC-018 8). A
// nil publisher on the Server skips the emit; the action still happens.
type AuditPublisher interface {
	Audit(ctx context.Context, ev events.AuditEvent) error
}

// emitAudit best-effort publishes an operator billing action to audit.events. It never
// fails the request: the action already happened, the trail is secondary.
func (s *Server) emitAudit(ctx context.Context, orgID int64, actor, action string, detail map[string]string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Audit(ctx, events.AuditEvent{
		OrgID:      orgID,
		Actor:      actor,
		Action:     action,
		Detail:     detail,
		OccurredAt: time.Now().UTC(),
	})
}

// adminSubscriptionDTO maps a stored subscription to the admin-panel shape.
func adminSubscriptionDTO(sub *domain.Subscription) apigen.AdminSubscription {
	return apigen.AdminSubscription{
		OrgId:             strconv.FormatInt(sub.OrgID, 10),
		Plan:              apigen.Plan(sub.Plan),
		Status:            apigen.AdminSubscriptionStatus(sub.Status),
		BillingCycle:      apigen.AdminSubscriptionBillingCycle(sub.BillingCycle),
		Provider:          sub.Provider,
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
		CurrentPeriodEnd:  sub.CurrentPeriodEnd,
	}
}

// CancelAdminOrgSubscription cancels an org's subscription at the provider, then
// records the local state (RFC-018 5.2). Default period_end; immediate drops the org to
// Free now. 404 if the org has no subscription. The webhook later confirms.
func (s *Server) CancelAdminOrgSubscription(ctx context.Context, req apigen.CancelAdminOrgSubscriptionRequestObject) (apigen.CancelAdminOrgSubscriptionResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.CancelAdminOrgSubscription401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if !s.isPlatformAdmin(p.Email) {
		return apigen.CancelAdminOrgSubscription403JSONResponse{ForbiddenJSONResponse: forbidden("platform admin only")}, nil
	}
	orgID, err := strconv.ParseInt(req.OrgId, 10, 64)
	if err != nil {
		return apigen.CancelAdminOrgSubscription404JSONResponse{NotFoundJSONResponse: notFound("org not found")}, nil
	}

	when := billing.CancelPeriodEnd
	if req.Body != nil && req.Body.When != nil && *req.Body.When == apigen.Immediate {
		when = billing.CancelImmediate
	}

	sub, err := s.store.GetSubscriptionByOrg(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.CancelAdminOrgSubscription404JSONResponse{NotFoundJSONResponse: notFound("org has no subscription")}, nil
		}
		return nil, err
	}

	if s.billing != nil {
		if perr := s.billing.CancelSubscription(ctx, sub.ProviderSubscriptionID, when); perr != nil && !errors.Is(perr, billing.ErrNotImplemented) {
			return nil, perr
		}
	}

	if when == billing.CancelImmediate {
		err = s.store.CancelSubscriptionNow(ctx, orgID)
	} else {
		err = s.store.SetSubscriptionCancelAtPeriodEnd(ctx, orgID)
	}
	if err != nil {
		return nil, err
	}

	s.emitAudit(ctx, orgID, p.Email, "billing.cancel", map[string]string{"when": string(when)})

	sub, err = s.store.GetSubscriptionByOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return apigen.CancelAdminOrgSubscription200JSONResponse(adminSubscriptionDTO(sub)), nil
}

// RefundAdminOrgPayment refunds a payment at the provider (RFC-018 5.3). Full when no
// amount is given, partial otherwise. Irreversible. The payments mirror is updated by
// the provider webhook (Phase 4); this endpoint requests the refund and audits it.
func (s *Server) RefundAdminOrgPayment(ctx context.Context, req apigen.RefundAdminOrgPaymentRequestObject) (apigen.RefundAdminOrgPaymentResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.RefundAdminOrgPayment401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if !s.isPlatformAdmin(p.Email) {
		return apigen.RefundAdminOrgPayment403JSONResponse{ForbiddenJSONResponse: forbidden("platform admin only")}, nil
	}
	orgID, err := strconv.ParseInt(req.OrgId, 10, 64)
	if err != nil {
		return apigen.RefundAdminOrgPayment404JSONResponse{NotFoundJSONResponse: notFound("org not found")}, nil
	}
	if req.Body == nil || req.Body.PaymentId == "" {
		return apigen.RefundAdminOrgPayment422JSONResponse{ValidationFailedJSONResponse: validationFailed("payment_id is required")}, nil
	}
	if s.billing == nil {
		return apigen.RefundAdminOrgPayment422JSONResponse{ValidationFailedJSONResponse: validationFailed("billing provider not configured")}, nil
	}

	var amount *billing.Money
	if req.Body.Amount != nil {
		currency := "USD"
		if req.Body.Currency != nil && *req.Body.Currency != "" {
			currency = *req.Body.Currency
		}
		amount = &billing.Money{Minor: *req.Body.Amount, Currency: currency}
	}
	reason := ""
	if req.Body.Reason != nil {
		reason = *req.Body.Reason
	}

	if err := s.billing.Refund(ctx, req.Body.PaymentId, amount, reason); err != nil {
		return nil, err
	}

	detail := map[string]string{"payment_id": req.Body.PaymentId}
	if amount != nil {
		detail["amount"] = strconv.FormatInt(amount.Minor, 10)
	}
	s.emitAudit(ctx, orgID, p.Email, "billing.refund", detail)

	return apigen.RefundAdminOrgPayment200JSONResponse(apigen.AdminRefund{
		PaymentId: req.Body.PaymentId,
		Status:    "refund_requested",
	}), nil
}

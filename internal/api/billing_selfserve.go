package api

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authz"
	"pulse/internal/billing"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
)

// Self-serve billing (RFC-018 6): two thin endpoints that hand back a provider-hosted
// URL. Checkout buys a paid plan; the portal manages card / invoices / self-cancel.
// Both are org-scoped and owner/admin only (ActionManageBilling). The subscription
// itself lands via the provider webhook, not here.

// trialDenyWindowDays is how recently a person must have had a subscription end for us to
// withhold a new free trial (RFC-018 anti-abuse). Within this window they get the
// trialless price and no trial badge; after it, they're treated as a fresh customer.
const trialDenyWindowDays = 35

// trialEligible reports whether the person may get a free trial: false when they recently
// controlled a subscription that ended, across any org they own or admin (per-person, not
// just this org). Used by checkout (to pick the price) and entitlements (to hide the
// trial badge), so the two never disagree.
func (s *Server) trialEligible(ctx context.Context, userID int64) (bool, error) {
	had, err := s.store.PersonHadRecentInactiveSubscription(ctx, userID, trialDenyWindowDays)
	if err != nil {
		return false, err
	}
	return !had, nil
}

// CreateBillingCheckout returns a hosted-checkout URL for a paid plan. Custom is never
// self-serve (RFC-018 7), so only tier2/tier3 are accepted.
func (s *Server) CreateBillingCheckout(ctx context.Context, req apigen.CreateBillingCheckoutRequestObject) (apigen.CreateBillingCheckoutResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CreateBillingCheckout401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageBilling, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.CreateBillingCheckout403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if req.Body == nil {
		return apigen.CreateBillingCheckout422JSONResponse{ValidationFailedJSONResponse: validationFailed("plan and cycle are required")}, nil
	}
	plan := string(req.Body.Plan)
	if plan != string(entitlements.PlanTier2) && plan != string(entitlements.PlanTier3) {
		return apigen.CreateBillingCheckout422JSONResponse{ValidationFailedJSONResponse: validationFailed("only tier2 or tier3 can be bought self-serve")}, nil
	}
	if s.billing == nil {
		return apigen.CreateBillingCheckout422JSONResponse{ValidationFailedJSONResponse: validationFailed("billing is not configured")}, nil
	}

	withTrial, err := s.trialEligible(ctx, p.UserID)
	if err != nil {
		return nil, err
	}

	url, err := s.billing.Checkout(ctx, p.OrgID, plan, string(req.Body.Cycle), withTrial)
	if err != nil {
		if errors.Is(err, billing.ErrNotImplemented) {
			return apigen.CreateBillingCheckout422JSONResponse{ValidationFailedJSONResponse: validationFailed("checkout is not available yet")}, nil
		}
		return nil, err
	}
	return apigen.CreateBillingCheckout200JSONResponse{Url: url}, nil
}

// CreateBillingPortal returns the provider customer-portal URL for the org.
func (s *Server) CreateBillingPortal(ctx context.Context, req apigen.CreateBillingPortalRequestObject) (apigen.CreateBillingPortalResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.CreateBillingPortal401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionManageBilling, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.CreateBillingPortal403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	if s.billing == nil {
		return apigen.CreateBillingPortal422JSONResponse{ValidationFailedJSONResponse: validationFailed("billing is not configured")}, nil
	}

	// The portal is a per-customer provider page, so the org needs a subscription with a
	// provider customer id first. A Free/never-subscribed org has nothing to manage.
	sub, err := s.store.GetSubscriptionByOrg(ctx, p.OrgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.CreateBillingPortal422JSONResponse{ValidationFailedJSONResponse: validationFailed("no billing to manage on this plan")}, nil
		}
		return nil, err
	}
	if sub.ProviderCustomerID == "" {
		return apigen.CreateBillingPortal422JSONResponse{ValidationFailedJSONResponse: validationFailed("no billing to manage on this plan")}, nil
	}

	url, err := s.billing.PortalURL(ctx, sub.ProviderCustomerID, sub.ProviderSubscriptionID)
	if err != nil {
		if errors.Is(err, billing.ErrNotImplemented) {
			return apigen.CreateBillingPortal422JSONResponse{ValidationFailedJSONResponse: validationFailed("the customer portal is not available yet")}, nil
		}
		return nil, err
	}
	return apigen.CreateBillingPortal200JSONResponse{Url: url}, nil
}

// ListBillingPayments returns the org's mirrored payments for the billing screen
// (RFC-018 4). Owner/admin only (ActionViewBilling), like the entitlements read.
func (s *Server) ListBillingPayments(ctx context.Context, _ apigen.ListBillingPaymentsRequestObject) (apigen.ListBillingPaymentsResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.ListBillingPayments401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionViewBilling, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.ListBillingPayments403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}
	payments, err := s.store.ListPayments(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.Payment, 0, len(payments))
	for _, pay := range payments {
		out = append(out, paymentDTO(pay))
	}
	return apigen.ListBillingPayments200JSONResponse(out), nil
}

// paymentDTO maps a stored payment to the wire shape.
func paymentDTO(p *domain.Payment) apigen.Payment {
	dto := apigen.Payment{
		Id:             strconv.FormatInt(p.ID, 10),
		Provider:       p.Provider,
		Amount:         p.Amount,
		Currency:       p.Currency,
		Status:         p.Status,
		RefundedAmount: p.RefundedAmount,
		CreatedAt:      p.CreatedAt,
	}
	if p.Period != "" {
		dto.Period = &p.Period
	}
	if p.HostedInvoiceURL != "" {
		dto.HostedInvoiceUrl = &p.HostedInvoiceURL
	}
	return dto
}

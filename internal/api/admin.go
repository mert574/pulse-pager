package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authn"
	"pulse/internal/authz"
	"pulse/internal/billing"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
)

// adminAuth gates the admin routes. When Cloudflare Access is configured, it
// verifies the CF Access token and injects an admin principal carrying the verified
// email, so a customer app-session cookie can't reach the admin endpoint (the
// session is ignored on this path). When CF Access is not configured (local/dev),
// it falls back to the normal session Identify. Either way the handler still checks
// the email against the platform admin allowlist.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	if s.cfAccess == nil {
		return s.auth.Identify(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, err := s.cfAccess.Verify(r.Context(), r.Header.Get(authn.CFAccessHeader))
		if err != nil {
			writeEnvelope(w, http.StatusUnauthorized, "unauthenticated", "cloudflare access required")
			return
		}
		p := authn.Principal{Kind: authz.ActorHuman, Email: email}
		next.ServeHTTP(w, r.WithContext(authn.WithPrincipal(r.Context(), p)))
	})
}

// This file implements the operator admin panel (one endpoint for now): platform-
// wide totals across every org. It is not org-scoped and sits outside the per-org
// RBAC. Access is the PULSE_PLATFORM_ADMINS email allowlist, checked here on every
// request, so the is_platform_admin flag on /me is only a UI hint, never the gate.

// GetAdminMetrics returns the cross-org totals for the admin panel. A signed-in
// human whose email is not in the allowlist gets a 403; everyone else a 401.
func (s *Server) GetAdminMetrics(ctx context.Context, _ apigen.GetAdminMetricsRequestObject) (apigen.GetAdminMetricsResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.GetAdminMetrics401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if !s.isPlatformAdmin(p.Email) {
		return apigen.GetAdminMetrics403JSONResponse{ForbiddenJSONResponse: forbidden("platform admin only")}, nil
	}

	m, err := s.store.PlatformMetrics(ctx)
	if err != nil {
		return nil, err
	}

	byPlan := make([]apigen.AdminPlanCount, 0, len(m.OrgsByPlan))
	for _, pc := range m.OrgsByPlan {
		byPlan = append(byPlan, apigen.AdminPlanCount{Plan: apigen.Plan(pc.Plan), Count: int(pc.Count)})
	}
	byType := make([]apigen.AdminTypeCount, 0, len(m.MonitorsByType))
	for _, mc := range m.MonitorsByType {
		byType = append(byType, apigen.AdminTypeCount{Type: apigen.MonitorType(mc.Type), Count: int(mc.Count)})
	}
	signups := make([]apigen.AdminSignupPoint, 0, len(m.Signups))
	for _, sp := range m.Signups {
		signups = append(signups, apigen.AdminSignupPoint{Date: sp.Date, Users: int(sp.Users), Orgs: int(sp.Orgs)})
	}

	var medianTTFM *int
	if m.MedianTimeToFirstMonitorSeconds != nil {
		v := int(*m.MedianTimeToFirstMonitorSeconds)
		medianTTFM = &v
	}

	return apigen.GetAdminMetrics200JSONResponse{
		Users:                           int(m.Users),
		Orgs:                            int(m.Orgs),
		MonitorsTotal:                   int(m.MonitorsTotal),
		MonitorsEnabled:                 int(m.MonitorsEnabled),
		MonitorsDisabled:                int(m.MonitorsTotal - m.MonitorsEnabled),
		Channels:                        int(m.Channels),
		OrgsWithMonitor:                 int(m.OrgsWithMonitor),
		MedianTimeToFirstMonitorSeconds: medianTTFM,
		ActiveOrgs7d:                    int(m.ActiveOrgs7d),
		OrgsByPlan:                      byPlan,
		MonitorsByType:                  byType,
		Signups:                         signups,
	}, nil
}

// GetAdminBilling returns the cross-org billing summary for the admin panel (RFC-018):
// paid orgs, subscriptions by status, and mirrored revenue by currency. Same allowlist
// check as the other admin endpoints.
func (s *Server) GetAdminBilling(ctx context.Context, _ apigen.GetAdminBillingRequestObject) (apigen.GetAdminBillingResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.GetAdminBilling401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if !s.isPlatformAdmin(p.Email) {
		return apigen.GetAdminBilling403JSONResponse{ForbiddenJSONResponse: forbidden("platform admin only")}, nil
	}

	b, err := s.store.PlatformBilling(ctx)
	if err != nil {
		return nil, err
	}

	subs := make([]apigen.AdminSubscriptionStatusCount, 0, len(b.SubscriptionsByStatus))
	for _, sc := range b.SubscriptionsByStatus {
		subs = append(subs, apigen.AdminSubscriptionStatusCount{Status: sc.Status, Count: int(sc.Count)})
	}
	rev := make([]apigen.AdminCurrencyRevenue, 0, len(b.RevenueByCurrency))
	for _, cr := range b.RevenueByCurrency {
		rev = append(rev, apigen.AdminCurrencyRevenue{
			Currency: cr.Currency,
			Gross:    cr.Gross,
			Refunded: cr.Refunded,
			Payments: int(cr.Payments),
		})
	}
	return apigen.GetAdminBilling200JSONResponse{
		PaidOrgs:              int(b.PaidOrgs),
		SubscriptionsByStatus: subs,
		RevenueByCurrency:     rev,
	}, nil
}

// adminOrgDTO maps a store org to the admin-panel shape (id, name, slug, plan).
func adminOrgDTO(o *domain.Organization) apigen.AdminOrg {
	return apigen.AdminOrg{
		Id:        strconv.FormatInt(o.ID, 10),
		Name:      o.Name,
		Slug:      o.Slug,
		Plan:      apigen.Plan(o.Plan),
		CreatedAt: o.CreatedAt,
	}
}

// ListAdminOrgs returns every active org with its plan, so an admin can see and
// change which plan an org is on. Same allowlist check as the metrics endpoint.
func (s *Server) ListAdminOrgs(ctx context.Context, _ apigen.ListAdminOrgsRequestObject) (apigen.ListAdminOrgsResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.ListAdminOrgs401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if !s.isPlatformAdmin(p.Email) {
		return apigen.ListAdminOrgs403JSONResponse{ForbiddenJSONResponse: forbidden("platform admin only")}, nil
	}

	orgs, err := s.store.AdminListOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.AdminOrg, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, adminOrgDTO(o))
	}
	return apigen.ListAdminOrgs200JSONResponse(out), nil
}

// SetAdminOrgPlan moves an org's plan (RFC-018 5.1). It always sets the operator
// override (organizations.plan), so an org with no subscription keeps working exactly
// as before. When the org has a subscription it also tells the provider to switch the
// price (the customer is never prompted) and writes the optimistic local subscription
// state; the webhook later confirms. For tierCustom with a custom_amount it creates a
// per-org price first (RFC-018 7). Validates the plan against the known set so a bad
// value is a 422, and 404s an org that does not exist.
func (s *Server) SetAdminOrgPlan(ctx context.Context, req apigen.SetAdminOrgPlanRequestObject) (apigen.SetAdminOrgPlanResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.SetAdminOrgPlan401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if !s.isPlatformAdmin(p.Email) {
		return apigen.SetAdminOrgPlan403JSONResponse{ForbiddenJSONResponse: forbidden("platform admin only")}, nil
	}
	if req.Body == nil {
		return apigen.SetAdminOrgPlan422JSONResponse{ValidationFailedJSONResponse: validationFailed("plan is required")}, nil
	}
	orgID, err := strconv.ParseInt(req.OrgId, 10, 64)
	if err != nil {
		return apigen.SetAdminOrgPlan404JSONResponse{NotFoundJSONResponse: notFound("org not found")}, nil
	}
	plan := string(req.Body.Plan)
	if !entitlements.IsKnownPlan(plan) {
		return apigen.SetAdminOrgPlan422JSONResponse{ValidationFailedJSONResponse: validationFailed("unknown plan")}, nil
	}

	cycle := "monthly"
	if req.Body.Cycle != nil && *req.Body.Cycle != "" {
		cycle = string(*req.Body.Cycle)
	}
	mode := "next_cycle"
	if req.Body.Mode != nil && *req.Body.Mode != "" {
		mode = string(*req.Body.Mode)
	}

	// If the org already has a subscription, drive the provider too (RFC-018 5.1). An
	// org with no subscription is the operator-override / comp case: only the local
	// plan changes, no provider call. A provider that has not built the method yet
	// (the Paddle skeleton) is logged and skipped so the override still applies.
	sub, subErr := s.store.GetSubscriptionByOrg(ctx, orgID)
	hasSub := subErr == nil
	if subErr != nil && !errors.Is(subErr, pgx.ErrNoRows) {
		return nil, subErr
	}

	if hasSub && s.billing != nil {
		priceID := sub.ProviderPriceID
		if plan == string(entitlements.PlanTierCustom) && req.Body.CustomAmount != nil {
			currency := "USD"
			if req.Body.CustomCurrency != nil && *req.Body.CustomCurrency != "" {
				currency = *req.Body.CustomCurrency
			}
			ref, perr := s.billing.SetCustomPrice(ctx, orgID, billing.Money{Minor: *req.Body.CustomAmount, Currency: currency}, cycle)
			if perr != nil && !errors.Is(perr, billing.ErrNotImplemented) {
				return nil, perr
			}
			if ref != "" {
				priceID = ref
			}
		}
		if perr := s.billing.UpdateSubscription(ctx, sub.ProviderSubscriptionID, billing.PlanChange{Plan: plan, Cycle: cycle, Mode: mode}); perr != nil && !errors.Is(perr, billing.ErrNotImplemented) {
			return nil, perr
		}
		if err := s.store.UpdateSubscriptionPlan(ctx, orgID, plan, cycle, priceID); err != nil {
			return nil, err
		}
	}

	if err := s.store.SetOrganizationPlan(ctx, orgID, plan); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apigen.SetAdminOrgPlan404JSONResponse{NotFoundJSONResponse: notFound("org not found")}, nil
		}
		return nil, err
	}

	s.emitAudit(ctx, orgID, p.Email, "billing.plan_set", map[string]string{"plan": plan, "cycle": cycle, "mode": mode})

	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return apigen.SetAdminOrgPlan200JSONResponse(adminOrgDTO(org)), nil
}

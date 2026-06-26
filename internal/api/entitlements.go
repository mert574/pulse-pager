package api

import (
	"context"

	"pulse/internal/apigen"
	"pulse/internal/authz"
	"pulse/internal/entitlements"
)

// This file implements the entitlements/usage slice (PRD-006 7): the per-org plan +
// usage-vs-caps read that drives the billing/usage screen and the upgrade prompts,
// and the public plan catalog the FE renders as a comparison table.
//
// Authz: viewing billing and usage is owner/admin only (PRD-006 9, master 4:
// "View billing and usage" = owner + admin; "Manage billing" = owner only). Member
// and viewer get a 403; they still feel limits, but through the per-field upsell
// errors on writes (monitor_limit_reached, seat_limit_reached, ...), not this read
// (PRD-006 9). The role gate runs through authz.Can (ActionViewBilling), never
// reimplemented here.
//
// Usage is counted from the real resource tables, never a stored tally, so it cannot
// drift (PRD-006 2.3/4): monitors_used = enabled monitors, seats_used = accepted
// members + reserved pending invites, status_pages_used = the page count. The caps
// and floors come from the entitlements resolvers (the same ones the write gates
// use), so the meter and the gate can never disagree.
//
// Plan changes are NOT here: they are Phase 2 (Stripe), set internally by an operator
// until then (PRD-006 6/8.3). This slice is the usage data + catalog only.

// GetEntitlements returns the active org's plan with usage vs caps and the plan
// floors/flags (PRD-006 7.1). Owner/admin only; member/viewer is 403.
func (s *Server) GetEntitlements(ctx context.Context, _ apigen.GetEntitlementsRequestObject) (apigen.GetEntitlementsResponseObject, error) {
	p, ok := s.humanInOrg(ctx)
	if !ok {
		return apigen.GetEntitlements401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if d := authz.Can(p.Actor(), authz.ActionViewBilling, authz.Resource{OrgID: p.OrgID}); !d.Allowed {
		return apigen.GetEntitlements403JSONResponse{ForbiddenJSONResponse: forbidden(d.Reason)}, nil
	}

	plan := s.orgPlan(ctx, p.OrgID)

	// Usage: counted from the resource tables (PRD-006 4), not a stored tally.
	monitorsUsed, err := s.store.CountEnabledMonitors(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	seats, err := s.seatUsage(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}
	statusPagesUsed, err := s.store.CountStatusPages(ctx, p.OrgID)
	if err != nil {
		return nil, err
	}

	// Caps and floors: the same resolvers the write gates use, so the meter and the
	// gate can never disagree.
	limits := s.monitors.MonitorLimits(p.OrgID, plan)

	// Whether this org still qualifies for a free trial, so the billing page can hide the
	// trial badge once the org is on a paid plan or the person recently had a subscription
	// (RFC-018).
	trialEligible, err := s.trialEligible(ctx, p.UserID, plan)
	if err != nil {
		return nil, err
	}

	return apigen.GetEntitlements200JSONResponse(apigen.Entitlements{
		Plan:                 apigen.Plan(plan),
		MonitorsUsed:         monitorsUsed,
		MonitorsCap:          limits.MonitorsCap,
		SeatsUsed:            seats.Used,
		SeatsCap:             seats.Cap,
		StatusPagesUsed:      statusPagesUsed,
		StatusPagesCap:       s.statusPages.StatusPageCap(p.OrgID, plan),
		MinIntervalSeconds:   limits.EffectiveIntervalFloor(),
		RetentionDays:        entitlements.Retention(plan),
		RegionsAllowed:       limits.RegionsAllowed,
		RegionsPerMonitorCap: limits.RegionsPerMonitorCap,
		CustomDomainAllowed:  entitlements.CustomDomainAllowed(plan),
		ApiAccessAllowed:     entitlements.APIAccessAllowed(plan),
		ApiWriteAllowed:      entitlements.APIWriteAllowed(plan),
		FailureSnapshot:      s.ents.For(p.OrgID).FailureSnapshot,
		TrialEligible:        trialEligible,
	}), nil
}

// ListPlans returns the public plan catalog: the tiers and their limits (PRD-006 3)
// so the FE renders a comparison/upgrade table without hardcoding. It is reference
// config, not per-org data, so it needs a session but no org and no role gate; the
// route is registered behind Identify only (router.go).
func (s *Server) ListPlans(_ context.Context, _ apigen.ListPlansRequestObject) (apigen.ListPlansResponseObject, error) {
	catalog := entitlements.Catalog()
	out := make([]apigen.PlanCatalogEntry, 0, len(catalog))
	for _, e := range catalog {
		out = append(out, apigen.PlanCatalogEntry{
			Plan:                 apigen.Plan(e.Plan),
			MonitorsCap:          e.MonitorsCap,
			MinIntervalSeconds:   e.MinIntervalSeconds,
			SeatsCap:             e.SeatsCap,
			StatusPagesCap:       e.StatusPagesCap,
			RetentionDays:        e.RetentionDays,
			RegionsAllowed:       e.RegionsAllowed,
			RegionsPerMonitorCap: e.RegionsPerMonitorCap,
			CustomDomainAllowed:  e.CustomDomainAllowed,
			ApiAccessAllowed:     e.APIAccessAllowed,
			ApiWriteAllowed:      e.APIWriteAllowed,
			ApiRatePerMin:        e.APIRatePerMin,
			ChannelTypes:         e.ChannelTypes,
		})
	}
	return apigen.ListPlans200JSONResponse(out), nil
}

// orgPlan reads the org's current billing tier from its stored plan (operator-set
// until Stripe lands, PRD-006 6/8.3). The resolvers take the plan, so this is the one
// place that resolves it. A load error or unknown value falls back to Free, so a
// missing/garbled row can never grant more than the free tier.
func (s *Server) orgPlan(ctx context.Context, orgID int64) entitlements.Plan {
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		return entitlements.PlanTier1
	}
	return entitlements.ParsePlan(org.Plan)
}

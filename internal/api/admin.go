package api

import (
	"context"
	"net/http"

	"pulse/internal/apigen"
	"pulse/internal/authn"
	"pulse/internal/authz"
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

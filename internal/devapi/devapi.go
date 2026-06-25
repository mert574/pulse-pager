// Package devapi is a development-only implementation of the generated API
// contract (internal/apigen, from api/openapi/v1.yaml). It serves a fake session
// and in-memory sample data so the SPA is browsable before the real api and auth
// (RFC-003/RFC-012) exist. Because it implements the same generated
// StrictServerInterface the real api will, it is contract-conformant rather than
// throwaway: when the real api lands, the SPA does not change. Wired into cmd/api
// only when PULSE_DEV_AUTH is set; needs no Postgres/Redis/Kafka. Do not ship it.
package devapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"pulse/internal/apigen"
	"pulse/internal/entitlements"
)

const (
	atCookie   = "pulse_at"
	csrfCookie = "pulse_csrf"
	devSession = "dev-session"
	devOrgID   = "org_dev"
	devOrgPlan = "tier3"
)

// monRow is a stored monitor plus the derived bits the list view needs.
type monRow struct {
	m            apigen.Monitor
	status       apigen.CoverageStatus
	lastCheckAt  *time.Time
	lastLatency  *int
	incidentOpen bool
	downReason   apigen.FailureReason // for the sample incident + last-failure
}

// server implements apigen.StrictServerInterface with in-memory sample data.
type server struct {
	mu        sync.Mutex
	monitors  map[string]*monRow
	channels  map[string]*apigen.Channel
	adminOrgs []apigen.AdminOrg
	nextID    int
	log       *slog.Logger
}

// Handler returns the dev API: the generated contract under /api/v1 (cookie-gated),
// plus the hand-wired auth-plane routes, the served spec, and Swagger UI.
func Handler(log *slog.Logger) http.Handler {
	s := &server{monitors: map[string]*monRow{}, channels: map[string]*apigen.Channel{}, log: log}
	s.seed()

	apiMux := http.NewServeMux()
	apigen.HandlerFromMuxWithBaseURL(apigen.NewStrictHandler(s, nil), apiMux, "/api/v1")

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	root.HandleFunc("GET /auth/{provider}/login", s.login)
	root.HandleFunc("POST /auth/refresh", s.refresh)
	root.HandleFunc("POST /auth/logout", s.logout)
	root.HandleFunc("GET /api/v1/openapi.json", s.openapiJSON)
	root.HandleFunc("GET /api/docs", s.swaggerUI)
	root.Handle("/api/v1/", s.requireAuth(apiMux))

	return s.logRequests(root)
}

// --- StrictServerInterface: account ---

func (s *server) GetMe(context.Context, apigen.GetMeRequestObject) (apigen.GetMeResponseObject, error) {
	return apigen.GetMe200JSONResponse(s.devMe()), nil
}

// devMe is the fake signed-in user the dev API returns from /me and the profile
// update echo. The orgs list is the single dev workspace.
func (s *server) devMe() apigen.Me {
	return apigen.Me{
		UserId:   "usr_dev",
		Email:    "dev@pulse.local",
		Name:     "Dev User",
		Locale:   "en",
		Timezone: "UTC",
		Orgs:     s.devOrgs(),
		// The dev user is a platform admin so the admin panel is browsable without
		// real infra or an allowlist (devapi is dev-only and never runs in prod).
		IsPlatformAdmin: true,
	}
}

// GetAdminMetrics serves sample platform totals so the admin panel renders in
// dev-auth mode. Static numbers, no store; the real counts live in internal/api.
func (s *server) GetAdminMetrics(context.Context, apigen.GetAdminMetricsRequestObject) (apigen.GetAdminMetricsResponseObject, error) {
	signups := make([]apigen.AdminSignupPoint, 0, 30)
	for i := 29; i >= 0; i-- {
		// vary the sample by day index so the trend looks alive (no time calls here).
		day := 30 - i
		signups = append(signups, apigen.AdminSignupPoint{
			Date:  "2026-06-" + twoDigit(day),
			Users: (i % 4),
			Orgs:  (i % 3),
		})
	}
	medianTTFM := 3600
	return apigen.GetAdminMetrics200JSONResponse{
		Users:                           42,
		Orgs:                            17,
		MonitorsTotal:                   128,
		MonitorsEnabled:                 113,
		MonitorsDisabled:                15,
		Channels:                        54,
		OrgsWithMonitor:                 12,
		MedianTimeToFirstMonitorSeconds: &medianTTFM,
		ActiveOrgs7d:                    9,
		OrgsByPlan: []apigen.AdminPlanCount{
			{Plan: "tier1", Count: 9},
			{Plan: "tier2", Count: 4},
			{Plan: "tier3", Count: 3},
			{Plan: "tierCustom", Count: 1},
		},
		MonitorsByType: []apigen.AdminTypeCount{
			{Type: "http", Count: 104},
			{Type: "ssl", Count: 24},
		},
		Signups: signups,
	}, nil
}

// ListAdminOrgs returns the in-memory sample orgs so the admin plan editor renders
// in dev-auth mode. No store; UpdateAdminOrgPlan mutates this same slice.
func (s *server) ListAdminOrgs(context.Context, apigen.ListAdminOrgsRequestObject) (apigen.ListAdminOrgsResponseObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]apigen.AdminOrg, len(s.adminOrgs))
	copy(out, s.adminOrgs)
	return apigen.ListAdminOrgs200JSONResponse(out), nil
}

// SetAdminOrgPlan changes a sample org's plan in-process so the dev UI reflects
// the edit on reload. 404s an unknown id, 422s an unknown plan, matching the real api.
func (s *server) SetAdminOrgPlan(_ context.Context, req apigen.SetAdminOrgPlanRequestObject) (apigen.SetAdminOrgPlanResponseObject, error) {
	if req.Body == nil || !entitlements.IsKnownPlan(string(req.Body.Plan)) {
		return apigen.SetAdminOrgPlan422JSONResponse{ValidationFailedJSONResponse: validationFailed("unknown plan")}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.adminOrgs {
		if s.adminOrgs[i].Id == req.OrgId {
			s.adminOrgs[i].Plan = req.Body.Plan
			return apigen.SetAdminOrgPlan200JSONResponse(s.adminOrgs[i]), nil
		}
	}
	return apigen.SetAdminOrgPlan404JSONResponse{NotFoundJSONResponse: notFound("org not found")}, nil
}

// CancelAdminOrgSubscription returns a sample canceled subscription so the dev admin UI
// renders the cancel flow without a provider or DB (RFC-018 5.2).
func (s *server) CancelAdminOrgSubscription(_ context.Context, req apigen.CancelAdminOrgSubscriptionRequestObject) (apigen.CancelAdminOrgSubscriptionResponseObject, error) {
	status := apigen.AdminSubscriptionStatus("active")
	cancelAtEnd := true
	if req.Body != nil && req.Body.When != nil && *req.Body.When == apigen.Immediate {
		status = "canceled"
		cancelAtEnd = false
	}
	return apigen.CancelAdminOrgSubscription200JSONResponse(apigen.AdminSubscription{
		OrgId:             req.OrgId,
		Plan:              devOrgPlan,
		Status:            status,
		BillingCycle:      "monthly",
		Provider:          "stub",
		CancelAtPeriodEnd: cancelAtEnd,
	}), nil
}

// RefundAdminOrgPayment acknowledges a refund in the dev stub (RFC-018 5.3).
func (s *server) RefundAdminOrgPayment(_ context.Context, req apigen.RefundAdminOrgPaymentRequestObject) (apigen.RefundAdminOrgPaymentResponseObject, error) {
	if req.Body == nil || req.Body.PaymentId == "" {
		return apigen.RefundAdminOrgPayment422JSONResponse{ValidationFailedJSONResponse: validationFailed("payment_id is required")}, nil
	}
	return apigen.RefundAdminOrgPayment200JSONResponse(apigen.AdminRefund{
		PaymentId: req.Body.PaymentId,
		Status:    "refund_requested",
	}), nil
}

// GetAdminBilling returns a sample cross-org billing summary for the dev admin panel.
func (s *server) GetAdminBilling(_ context.Context, _ apigen.GetAdminBillingRequestObject) (apigen.GetAdminBillingResponseObject, error) {
	return apigen.GetAdminBilling200JSONResponse{
		PaidOrgs: 2,
		SubscriptionsByStatus: []apigen.AdminSubscriptionStatusCount{
			{Status: "active", Count: 2},
			{Status: "past_due", Count: 1},
		},
		RevenueByCurrency: []apigen.AdminCurrencyRevenue{
			{Currency: "USD", Gross: 5700, Refunded: 1900, Payments: 3},
		},
	}, nil
}

// CreateBillingCheckout returns a fake hosted-checkout URL in the dev stub (RFC-018 6).
func (s *server) CreateBillingCheckout(_ context.Context, req apigen.CreateBillingCheckoutRequestObject) (apigen.CreateBillingCheckoutResponseObject, error) {
	if req.Body == nil {
		return apigen.CreateBillingCheckout422JSONResponse{ValidationFailedJSONResponse: validationFailed("plan and cycle are required")}, nil
	}
	return apigen.CreateBillingCheckout200JSONResponse{
		Url: "https://stub.billing.local/checkout?plan=" + string(req.Body.Plan),
	}, nil
}

// CreateBillingPortal returns a fake customer-portal URL in the dev stub (RFC-018 6).
func (s *server) CreateBillingPortal(_ context.Context, _ apigen.CreateBillingPortalRequestObject) (apigen.CreateBillingPortalResponseObject, error) {
	return apigen.CreateBillingPortal200JSONResponse{Url: "https://stub.billing.local/portal"}, nil
}

// ListBillingPayments returns a couple of sample payments so the dev billing screen
// renders the invoices section (RFC-018 4).
func (s *server) ListBillingPayments(_ context.Context, _ apigen.ListBillingPaymentsRequestObject) (apigen.ListBillingPaymentsResponseObject, error) {
	inv := "https://stub.billing.local/invoice/dev-1"
	period := "2026-06"
	return apigen.ListBillingPayments200JSONResponse{
		{
			Id: "1", Provider: "stub", Amount: 1900, Currency: "USD",
			Status: "paid", Period: &period, HostedInvoiceUrl: &inv, RefundedAmount: 0,
			CreatedAt: time.Now().UTC().Add(-24 * time.Hour),
		},
	}, nil
}

// twoDigit zero-pads 1..31 for the sample dates.
func twoDigit(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func (s *server) devOrgs() []apigen.OrgMembership {
	return []apigen.OrgMembership{
		{OrgId: devOrgID, Name: "Dev Workspace", Slug: "dev", Role: "owner", Plan: devOrgPlan},
	}
}

// UpdateMe echoes the patched fields onto the fake user so the SPA sees a 200 with
// the updated profile. Nothing is persisted (dev mode has no store).
func (s *server) UpdateMe(_ context.Context, req apigen.UpdateMeRequestObject) (apigen.UpdateMeResponseObject, error) {
	me := s.devMe()
	if req.Body != nil {
		if req.Body.Name != nil {
			me.Name = *req.Body.Name
		}
		if req.Body.Locale != nil {
			me.Locale = *req.Body.Locale
		}
		if req.Body.Timezone != nil {
			me.Timezone = *req.Body.Timezone
		}
	}
	return apigen.UpdateMe200JSONResponse(me), nil
}

// ListMyIdentities returns a single fake linked identity so the account screen has
// something to render in dev mode.
func (s *server) ListMyIdentities(context.Context, apigen.ListMyIdentitiesRequestObject) (apigen.ListMyIdentitiesResponseObject, error) {
	return apigen.ListMyIdentities200JSONResponse([]apigen.Identity{
		{Provider: "github", ProviderUserId: "dev-12345", CreatedAt: time.Now().UTC().Add(-24 * time.Hour)},
	}), nil
}

// UnlinkMyIdentity is a no-op success in dev mode.
func (s *server) UnlinkMyIdentity(context.Context, apigen.UnlinkMyIdentityRequestObject) (apigen.UnlinkMyIdentityResponseObject, error) {
	return apigen.UnlinkMyIdentity204Response{}, nil
}

// ListOrgs returns the dev workspace as the only org.
func (s *server) ListOrgs(context.Context, apigen.ListOrgsRequestObject) (apigen.ListOrgsResponseObject, error) {
	return apigen.ListOrgs200JSONResponse(s.devOrgs()), nil
}

// CreateOrg fakes org creation: it echoes a new owned org without persisting.
func (s *server) CreateOrg(_ context.Context, req apigen.CreateOrgRequestObject) (apigen.CreateOrgResponseObject, error) {
	if req.Body == nil || req.Body.Name == "" {
		return apigen.CreateOrg422JSONResponse{ValidationFailedJSONResponse: validationFailed("name required")}, nil
	}
	slug := "new-org"
	if req.Body.Slug != nil && *req.Body.Slug != "" {
		slug = *req.Body.Slug
	}
	return apigen.CreateOrg201JSONResponse(apigen.OrgMembership{
		OrgId: s.newID("org"), Name: req.Body.Name, Slug: slug, Role: "owner", Plan: "tier1",
	}), nil
}

// GetOrg returns the dev workspace for the dev org id, else a 403 (mirrors the real
// non-member behavior so the SPA handles it the same way).
func (s *server) GetOrg(_ context.Context, req apigen.GetOrgRequestObject) (apigen.GetOrgResponseObject, error) {
	if req.OrgId != devOrgID {
		return apigen.GetOrg403JSONResponse{ForbiddenJSONResponse: forbidden("not a member of this org")}, nil
	}
	return apigen.GetOrg200JSONResponse(s.devOrgs()[0]), nil
}

func (s *server) LogoutAll(context.Context, apigen.LogoutAllRequestObject) (apigen.LogoutAllResponseObject, error) {
	return apigen.LogoutAll204Response{}, nil
}

func (s *server) GetEntitlements(_ context.Context, _ apigen.GetEntitlementsRequestObject) (apigen.GetEntitlementsResponseObject, error) {
	s.mu.Lock()
	used := len(s.monitors)
	s.mu.Unlock()
	return apigen.GetEntitlements200JSONResponse(apigen.Entitlements{
		Plan:                 devOrgPlan,
		MonitorsUsed:         used,
		MonitorsCap:          100,
		SeatsUsed:            1,
		SeatsCap:             10,
		StatusPagesUsed:      0,
		StatusPagesCap:       3,
		MinIntervalSeconds:   60,
		RetentionDays:        90,
		RegionsAllowed:       []string{"eu-central", "us-west", "us-east"},
		RegionsPerMonitorCap: 4,
		CustomDomainAllowed:  true,
		ApiAccessAllowed:     true,
		ApiWriteAllowed:      true,
		FailureSnapshot:      true,
		TrialEligible:        true,
	}), nil
}

// ListPlans returns the public plan catalog from the entitlements package so the dev
// SPA renders the same comparison/upgrade table the real api serves (PRD-006 3).
func (s *server) ListPlans(_ context.Context, _ apigen.ListPlansRequestObject) (apigen.ListPlansResponseObject, error) {
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

// --- members + invitations (dev sample data, nothing persisted) ---

// ListMembers returns the dev user plus one sample teammate so the members screen
// has rows to render.
func (s *server) ListMembers(context.Context, apigen.ListMembersRequestObject) (apigen.ListMembersResponseObject, error) {
	return apigen.ListMembers200JSONResponse([]apigen.Member{
		{UserId: "usr_dev", Email: "dev@pulse.local", Name: "Dev User", Role: "owner", JoinedAt: time.Now().UTC().Add(-72 * time.Hour)},
		{UserId: "usr_teammate", Email: "teammate@pulse.local", Name: "Sam Teammate", Role: "member", JoinedAt: time.Now().UTC().Add(-24 * time.Hour)},
	}), nil
}

// ChangeMemberRole echoes the requested role onto the sample teammate.
func (s *server) ChangeMemberRole(_ context.Context, req apigen.ChangeMemberRoleRequestObject) (apigen.ChangeMemberRoleResponseObject, error) {
	if req.Body == nil {
		return apigen.ChangeMemberRole422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	return apigen.ChangeMemberRole200JSONResponse(apigen.Member{
		UserId: req.UserId, Email: "teammate@pulse.local", Name: "Sam Teammate", Role: req.Body.Role, JoinedAt: time.Now().UTC().Add(-24 * time.Hour),
	}), nil
}

// RemoveMember is a no-op success in dev mode.
func (s *server) RemoveMember(context.Context, apigen.RemoveMemberRequestObject) (apigen.RemoveMemberResponseObject, error) {
	return apigen.RemoveMember204Response{}, nil
}

// LeaveOrg is a no-op success in dev mode.
func (s *server) LeaveOrg(context.Context, apigen.LeaveOrgRequestObject) (apigen.LeaveOrgResponseObject, error) {
	return apigen.LeaveOrg204Response{}, nil
}

// TransferOwnership is a no-op success in dev mode.
func (s *server) TransferOwnership(_ context.Context, req apigen.TransferOwnershipRequestObject) (apigen.TransferOwnershipResponseObject, error) {
	if req.Body == nil || req.Body.UserId == "" {
		return apigen.TransferOwnership422JSONResponse{ValidationFailedJSONResponse: validationFailed("user_id required")}, nil
	}
	return apigen.TransferOwnership204Response{}, nil
}

// ListInvitations returns one sample pending invitation.
func (s *server) ListInvitations(context.Context, apigen.ListInvitationsRequestObject) (apigen.ListInvitationsResponseObject, error) {
	return apigen.ListInvitations200JSONResponse([]apigen.Invitation{
		{Id: "inv_dev", Email: "pending@pulse.local", Role: "member", State: "pending", CreatedAt: time.Now().UTC().Add(-2 * time.Hour), ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour)},
	}), nil
}

// CreateInvitation echoes a new pending invitation without sending email.
func (s *server) CreateInvitation(_ context.Context, req apigen.CreateInvitationRequestObject) (apigen.CreateInvitationResponseObject, error) {
	if req.Body == nil || req.Body.Email == "" {
		return apigen.CreateInvitation422JSONResponse{ValidationFailedJSONResponse: validationFailed("email required")}, nil
	}
	return apigen.CreateInvitation201JSONResponse(apigen.Invitation{
		Id: s.newID("inv"), Email: string(req.Body.Email), Role: req.Body.Role, State: "pending",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour),
	}), nil
}

// RevokeInvitation is a no-op success in dev mode.
func (s *server) RevokeInvitation(context.Context, apigen.RevokeInvitationRequestObject) (apigen.RevokeInvitationResponseObject, error) {
	return apigen.RevokeInvitation204Response{}, nil
}

// ResendInvitation echoes the sample invitation with a refreshed expiry.
func (s *server) ResendInvitation(_ context.Context, req apigen.ResendInvitationRequestObject) (apigen.ResendInvitationResponseObject, error) {
	return apigen.ResendInvitation200JSONResponse(apigen.Invitation{
		Id: req.Id, Email: "pending@pulse.local", Role: "member", State: "pending",
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour), ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour),
	}), nil
}

// GetInvitationPreview returns a sample preview for any token in dev mode.
func (s *server) GetInvitationPreview(_ context.Context, req apigen.GetInvitationPreviewRequestObject) (apigen.GetInvitationPreviewResponseObject, error) {
	inviter := "Dev User"
	return apigen.GetInvitationPreview200JSONResponse(apigen.InvitationPreview{
		OrgName: "Dev Workspace", Role: "member", State: "pending", Email: "pending@pulse.local", InviterName: &inviter,
	}), nil
}

// AcceptInvitation returns the dev workspace membership in dev mode.
func (s *server) AcceptInvitation(context.Context, apigen.AcceptInvitationRequestObject) (apigen.AcceptInvitationResponseObject, error) {
	return apigen.AcceptInvitation200JSONResponse(s.devOrgs()[0]), nil
}

// --- api keys ---

// ListAPIKeys returns a sample key (metadata only) in dev mode.
func (s *server) ListAPIKeys(context.Context, apigen.ListAPIKeysRequestObject) (apigen.ListAPIKeysResponseObject, error) {
	return apigen.ListAPIKeys200JSONResponse([]apigen.APIKey{
		{Id: "key_dev", Name: "dev key", Prefix: "pulse_sk_devkey", Role: "member", CreatedAt: time.Now().UTC().Add(-24 * time.Hour)},
	}), nil
}

// CreateAPIKey echoes a new key with a fake one-time secret in dev mode.
func (s *server) CreateAPIKey(_ context.Context, req apigen.CreateAPIKeyRequestObject) (apigen.CreateAPIKeyResponseObject, error) {
	if req.Body == nil || req.Body.Name == "" {
		return apigen.CreateAPIKey422JSONResponse{ValidationFailedJSONResponse: validationFailed("name required")}, nil
	}
	if req.Body.Role != "member" && req.Body.Role != "admin" {
		return apigen.CreateAPIKey422JSONResponse{ValidationFailedJSONResponse: validationFailed("role must be member or admin")}, nil
	}
	return apigen.CreateAPIKey201JSONResponse(apigen.APIKeyCreated{
		Key: apigen.APIKey{
			Id: s.newID("key"), Name: req.Body.Name, Prefix: "pulse_sk_devnew", Role: req.Body.Role, CreatedAt: time.Now().UTC(),
		},
		Secret: "pulse_sk_dev-secret-shown-once",
	}), nil
}

// RevokeAPIKey is a no-op success in dev mode.
func (s *server) RevokeAPIKey(context.Context, apigen.RevokeAPIKeyRequestObject) (apigen.RevokeAPIKeyResponseObject, error) {
	return apigen.RevokeAPIKey204Response{}, nil
}

// --- outbound webhooks (dev stubs) ---

// devWebhook is a static sample webhook (secret never shown in list/get).
func devWebhook() apigen.OutboundWebhook {
	return apigen.OutboundWebhook{
		Id:        "wh_dev",
		Url:       "https://example.com/pulse-hook",
		Enabled:   true,
		Events:    []apigen.OutboundWebhookEvent{"monitor.down", "monitor.recovery"},
		CreatedAt: time.Now().UTC().Add(-24 * time.Hour),
		UpdatedAt: time.Now().UTC().Add(-24 * time.Hour),
	}
}

func (s *server) ListWebhooks(context.Context, apigen.ListWebhooksRequestObject) (apigen.ListWebhooksResponseObject, error) {
	return apigen.ListWebhooks200JSONResponse([]apigen.OutboundWebhook{devWebhook()}), nil
}

func (s *server) GetWebhook(context.Context, apigen.GetWebhookRequestObject) (apigen.GetWebhookResponseObject, error) {
	return apigen.GetWebhook200JSONResponse(devWebhook()), nil
}

func (s *server) CreateWebhook(_ context.Context, req apigen.CreateWebhookRequestObject) (apigen.CreateWebhookResponseObject, error) {
	if req.Body == nil || req.Body.Url == "" {
		return apigen.CreateWebhook422JSONResponse{ValidationFailedJSONResponse: validationFailed("url required")}, nil
	}
	w := devWebhook()
	w.Id = s.newID("wh")
	w.Url = req.Body.Url
	if req.Body.Events != nil {
		w.Events = *req.Body.Events
	}
	return apigen.CreateWebhook201JSONResponse(apigen.OutboundWebhookCreated{
		Webhook: w,
		Secret:  "whsec_dev-secret-shown-once",
	}), nil
}

func (s *server) UpdateWebhook(_ context.Context, req apigen.UpdateWebhookRequestObject) (apigen.UpdateWebhookResponseObject, error) {
	if req.Body == nil || req.Body.Url == "" {
		return apigen.UpdateWebhook422JSONResponse{ValidationFailedJSONResponse: validationFailed("url required")}, nil
	}
	w := devWebhook()
	w.Url = req.Body.Url
	if req.Body.Events != nil {
		w.Events = *req.Body.Events
	}
	return apigen.UpdateWebhook200JSONResponse(w), nil
}

func (s *server) RotateWebhookSecret(context.Context, apigen.RotateWebhookSecretRequestObject) (apigen.RotateWebhookSecretResponseObject, error) {
	return apigen.RotateWebhookSecret200JSONResponse(apigen.OutboundWebhookCreated{
		Webhook: devWebhook(),
		Secret:  "whsec_dev-rotated-secret-shown-once",
	}), nil
}

func (s *server) DeleteWebhook(context.Context, apigen.DeleteWebhookRequestObject) (apigen.DeleteWebhookResponseObject, error) {
	return apigen.DeleteWebhook204Response{}, nil
}

// --- monitors ---

func (s *server) ListMonitors(_ context.Context, _ apigen.ListMonitorsRequestObject) (apigen.ListMonitorsResponseObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]apigen.MonitorListItem, 0, len(s.monitors))
	for _, row := range s.monitors {
		item := apigen.MonitorListItem{
			Id:            row.m.Id,
			Type:          row.m.Type,
			Name:          row.m.Name,
			Url:           row.m.Url,
			Enabled:       row.m.Enabled,
			Status:        row.status,
			LastCheckAt:   row.lastCheckAt,
			LastLatencyMs: row.lastLatency,
			IncidentOpen:  row.incidentOpen,
		}
		if row.m.Cert != nil {
			item.CertExpiresAt = &row.m.Cert.NotAfter
		}
		items = append(items, item)
	}
	return apigen.ListMonitors200JSONResponse(items), nil
}

func (s *server) GetMonitor(_ context.Context, req apigen.GetMonitorRequestObject) (apigen.GetMonitorResponseObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.monitors[req.Id]
	if !ok {
		return apigen.GetMonitor404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
	}
	return apigen.GetMonitor200JSONResponse(row.m), nil
}

func (s *server) CreateMonitor(_ context.Context, req apigen.CreateMonitorRequestObject) (apigen.CreateMonitorResponseObject, error) {
	if req.Body == nil {
		return apigen.CreateMonitor422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := monitorFromInput(s.newID("mon"), *req.Body)
	s.monitors[m.Id] = &monRow{m: m, status: "pending"}
	return apigen.CreateMonitor201JSONResponse(m), nil
}

func (s *server) UpdateMonitor(_ context.Context, req apigen.UpdateMonitorRequestObject) (apigen.UpdateMonitorResponseObject, error) {
	if req.Body == nil {
		return apigen.UpdateMonitor422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.monitors[req.Id]
	if !ok {
		return apigen.UpdateMonitor404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
	}
	created := row.m.CreatedAt
	row.m = monitorFromInput(row.m.Id, *req.Body)
	row.m.CreatedAt = created
	row.m.UpdatedAt = time.Now().UTC()
	return apigen.UpdateMonitor200JSONResponse(row.m), nil
}

func (s *server) DeleteMonitor(_ context.Context, req apigen.DeleteMonitorRequestObject) (apigen.DeleteMonitorResponseObject, error) {
	s.mu.Lock()
	delete(s.monitors, req.Id)
	s.mu.Unlock()
	return apigen.DeleteMonitor204Response{}, nil
}

func (s *server) CheckNow(_ context.Context, req apigen.CheckNowRequestObject) (apigen.CheckNowResponseObject, error) {
	s.mu.Lock()
	m, ok := s.monitors[req.Id]
	s.mu.Unlock()
	if !ok {
		return apigen.CheckNow404JSONResponse{NotFoundJSONResponse: notFound("monitor not found")}, nil
	}
	// Dev stub: accept the check and report every region scheduled, matching the real
	// async contract. The dev region-states endpoint then reports them complete.
	regions := m.m.Regions
	if len(regions) == 0 {
		regions = []string{"eu-central"}
	}
	now := time.Now().UTC()
	out := make([]apigen.RegionState, 0, len(regions))
	for _, r := range regions {
		out = append(out, apigen.RegionState{Region: r, State: "scheduled", UpdatedAt: now})
	}
	return apigen.CheckNow202JSONResponse(apigen.CheckNowAccepted{MonitorId: req.Id, Regions: out}), nil
}

// GetMonitorRegionStates is the dev stub for the live region-state poll: it reports
// every region of every monitor as a recent successful check, so the SPA renders chips
// without any pipeline.
func (s *server) GetMonitorRegionStates(_ context.Context, req apigen.GetMonitorRegionStatesRequestObject) (apigen.GetMonitorRegionStatesResponseObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	healthy := true
	out := apigen.MonitorRegionStates{Monitors: map[string][]apigen.RegionState{}}
	for id, m := range s.monitors {
		if req.Params.MonitorId != nil && *req.Params.MonitorId != "" && *req.Params.MonitorId != id {
			continue
		}
		regions := m.m.Regions
		if len(regions) == 0 {
			regions = []string{"eu-central"}
		}
		states := make([]apigen.RegionState, 0, len(regions))
		for _, r := range regions {
			states = append(states, apigen.RegionState{
				Region: r, State: "done", Healthy: &healthy,
				StatusCode: intptr(200), LatencyMs: intptr(120), UpdatedAt: now,
			})
		}
		out.Monitors[id] = states
	}
	return apigen.GetMonitorRegionStates200JSONResponse(out), nil
}

func (s *server) ListResults(_ context.Context, req apigen.ListResultsRequestObject) (apigen.ListResultsResponseObject, error) {
	// Sample several regions per tick so dev-auth shows the grouped run row (one row
	// per scheduled_at) with its expandable per-region detail. Each region of a tick
	// shares scheduled_at but runs a moment apart with its own latency.
	regions := []string{"eu-central", "us-west", "us-east"}
	items := make([]apigen.CheckResult, 0, 20*len(regions))
	base := time.Now().UTC()
	for i := 0; i < 20; i++ {
		scheduled := base.Add(-time.Duration(i) * 5 * time.Minute)
		for j, region := range regions {
			healthy := (i+j)%7 != 0
			res := apigen.CheckResult{
				Id:          "res_" + strconv.Itoa(i) + "_" + region,
				MonitorId:   req.Id,
				Region:      region,
				ScheduledAt: scheduled,
				CheckedAt:   scheduled.Add(time.Duration(j) * 200 * time.Millisecond),
				Healthy:     healthy,
				LatencyMs:   intptr(90 + i*3 + j*40),
			}
			if healthy {
				res.StatusCode = intptr(200)
			} else {
				reason := apigen.FailureReason("status_mismatch")
				res.StatusCode = intptr(503)
				res.FailureReason = &reason
			}
			items = append(items, res)
		}
	}
	return apigen.ListResults200JSONResponse(apigen.PageCheckResult{Items: items}), nil
}

func (s *server) ListMonitorIncidents(_ context.Context, req apigen.ListMonitorIncidentsRequestObject) (apigen.ListMonitorIncidentsResponseObject, error) {
	s.mu.Lock()
	row, ok := s.monitors[req.Id]
	items := []apigen.Incident{}
	if ok {
		items = incidentsFor(row)
	}
	s.mu.Unlock()
	return apigen.ListMonitorIncidents200JSONResponse(apigen.PageIncident{Items: items}), nil
}

func (s *server) GetLastFailure(_ context.Context, req apigen.GetLastFailureRequestObject) (apigen.GetLastFailureResponseObject, error) {
	s.mu.Lock()
	row, ok := s.monitors[req.Id]
	s.mu.Unlock()
	if !ok || row.status != "down" {
		return apigen.GetLastFailure404Response{}, nil
	}
	return apigen.GetLastFailure200JSONResponse(apigen.FailureSnapshot{
		MonitorId:  req.Id,
		CheckedAt:  time.Now().UTC().Add(-30 * time.Minute),
		StatusCode: intptr(503),
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"X-Request-Id": {"dev-" + req.Id},
		},
		Body:      `{"status":"unhealthy","detail":"sample captured response"}`,
		Truncated: false,
	}), nil
}

// --- status pages (dev sample data, nothing persisted) ---

// devStatusPage is one sample page so the editor has something to render. The public
// projection is derived from the dev monitors so the public preview is plausible.
func (s *server) devStatusPage(id, name, slug string, published bool) apigen.StatusPage {
	now := time.Now().UTC()
	return apigen.StatusPage{
		Id: id, OrgId: devOrgID, Name: name, Slug: slug,
		LogoUrl: "", AccentColor: "#4f46e5", Theme: "light",
		State: stateOf(published),
		DisplayMonitors: []apigen.StatusPageMonitorEntry{
			{MonitorId: "mon_1", DisplayName: "Website", Order: 0},
			{MonitorId: "mon_2", DisplayName: "API", Order: 1},
		},
		CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now,
	}
}

func stateOf(published bool) apigen.StatusPageState {
	if published {
		return "published"
	}
	return "draft"
}

func (s *server) ListStatusPages(context.Context, apigen.ListStatusPagesRequestObject) (apigen.ListStatusPagesResponseObject, error) {
	return apigen.ListStatusPages200JSONResponse([]apigen.StatusPage{
		s.devStatusPage("sp_1", "Public Status", "acme", true),
	}), nil
}

func (s *server) GetStatusPage(_ context.Context, req apigen.GetStatusPageRequestObject) (apigen.GetStatusPageResponseObject, error) {
	return apigen.GetStatusPage200JSONResponse(s.devStatusPage(string(req.Id), "Public Status", "acme", true)), nil
}

func (s *server) CreateStatusPage(_ context.Context, req apigen.CreateStatusPageRequestObject) (apigen.CreateStatusPageResponseObject, error) {
	if req.Body == nil || req.Body.Name == "" || req.Body.Slug == "" {
		return apigen.CreateStatusPage422JSONResponse{ValidationFailedJSONResponse: validationFailed("name and slug required")}, nil
	}
	sp := s.devStatusPage(s.newID("sp"), req.Body.Name, req.Body.Slug, false)
	sp.DisplayMonitors = req.Body.DisplayMonitors
	return apigen.CreateStatusPage201JSONResponse(sp), nil
}

func (s *server) UpdateStatusPage(_ context.Context, req apigen.UpdateStatusPageRequestObject) (apigen.UpdateStatusPageResponseObject, error) {
	if req.Body == nil || req.Body.Name == "" || req.Body.Slug == "" {
		return apigen.UpdateStatusPage422JSONResponse{ValidationFailedJSONResponse: validationFailed("name and slug required")}, nil
	}
	sp := s.devStatusPage(string(req.Id), req.Body.Name, req.Body.Slug, false)
	sp.DisplayMonitors = req.Body.DisplayMonitors
	return apigen.UpdateStatusPage200JSONResponse(sp), nil
}

func (s *server) PublishStatusPage(_ context.Context, req apigen.PublishStatusPageRequestObject) (apigen.PublishStatusPageResponseObject, error) {
	published := req.Body != nil && req.Body.Published
	return apigen.PublishStatusPage200JSONResponse(s.devStatusPage(string(req.Id), "Public Status", "acme", published)), nil
}

func (s *server) DeleteStatusPage(context.Context, apigen.DeleteStatusPageRequestObject) (apigen.DeleteStatusPageResponseObject, error) {
	return apigen.DeleteStatusPage204Response{}, nil
}

func (s *server) GetPublicStatusPage(_ context.Context, req apigen.GetPublicStatusPageRequestObject) (apigen.GetPublicStatusPageResponseObject, error) {
	if req.Slug == "" {
		return apigen.GetPublicStatusPage404JSONResponse{NotFoundJSONResponse: notFound("status page not found")}, nil
	}
	now := time.Now().UTC()
	full := apigen.PublicUptime{Uptime24h: 100, Uptime7d: 99.8, Uptime90d: 99.5, Has24h: true, Has7d: true, Has90d: true}
	return apigen.GetPublicStatusPage200JSONResponse(apigen.PublicStatusPage{
		Name: "Acme Status", Slug: req.Slug, AccentColor: "#4f46e5", Theme: "light",
		Banner: "partial_outage", UptimeMaxWindow: "90d",
		Monitors: []apigen.PublicDisplayedMonitor{
			{DisplayName: "Website", Status: "operational", Uptime: full, History: []apigen.PublicHistoryPoint{{At: now, Up: true}}},
			{DisplayName: "API", Status: "down", Uptime: full, History: []apigen.PublicHistoryPoint{{At: now, Up: false}}},
		},
		Incidents: []apigen.PublicIncident{
			{DisplayName: "API", StartedAt: now.Add(-30 * time.Minute), Resolved: false},
		},
	}), nil
}

// --- channels ---

func (s *server) ListChannels(_ context.Context, _ apigen.ListChannelsRequestObject) (apigen.ListChannelsResponseObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]apigen.Channel, 0, len(s.channels))
	for _, c := range s.channels {
		items = append(items, *c)
	}
	return apigen.ListChannels200JSONResponse(items), nil
}

func (s *server) CreateChannel(_ context.Context, req apigen.CreateChannelRequestObject) (apigen.CreateChannelResponseObject, error) {
	in := apigen.ChannelInput{}
	if req.Body != nil {
		in = *req.Body
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := apigen.Channel{Id: s.newID("chan"), OrgId: devOrgID, Name: in.Name, Type: in.Type, Enabled: in.Enabled, Config: in.Config}
	s.channels[c.Id] = &c
	return apigen.CreateChannel201JSONResponse(c), nil
}

func (s *server) UpdateChannel(_ context.Context, req apigen.UpdateChannelRequestObject) (apigen.UpdateChannelResponseObject, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.channels[req.Id]
	if !ok {
		return apigen.UpdateChannel404JSONResponse{NotFoundJSONResponse: notFound("channel not found")}, nil
	}
	if req.Body != nil {
		existing.Name = req.Body.Name
		existing.Type = req.Body.Type
		existing.Enabled = req.Body.Enabled
		existing.Config = req.Body.Config
	}
	return apigen.UpdateChannel200JSONResponse(*existing), nil
}

func (s *server) DeleteChannel(_ context.Context, req apigen.DeleteChannelRequestObject) (apigen.DeleteChannelResponseObject, error) {
	s.mu.Lock()
	delete(s.channels, req.Id)
	s.mu.Unlock()
	return apigen.DeleteChannel204Response{}, nil
}

func (s *server) TestChannel(context.Context, apigen.TestChannelRequestObject) (apigen.TestChannelResponseObject, error) {
	return apigen.TestChannel204Response{}, nil
}

// --- incidents ---

func (s *server) ListIncidents(_ context.Context, _ apigen.ListIncidentsRequestObject) (apigen.ListIncidentsResponseObject, error) {
	s.mu.Lock()
	items := []apigen.Incident{}
	for _, row := range s.monitors {
		items = append(items, incidentsFor(row)...)
	}
	s.mu.Unlock()
	return apigen.ListIncidents200JSONResponse(apigen.PageIncident{Items: items}), nil
}

func (s *server) GetIncident(_ context.Context, req apigen.GetIncidentRequestObject) (apigen.GetIncidentResponseObject, error) {
	s.mu.Lock()
	inc, ok := s.findIncident(req.Id)
	s.mu.Unlock()
	if !ok {
		return apigen.GetIncident404JSONResponse{NotFoundJSONResponse: notFound("incident not found")}, nil
	}
	return apigen.GetIncident200JSONResponse(incidentDetailFor(inc)), nil
}

func (s *server) AddIncidentAnnotation(_ context.Context, req apigen.AddIncidentAnnotationRequestObject) (apigen.AddIncidentAnnotationResponseObject, error) {
	if req.Body == nil || strings.TrimSpace(req.Body.Note) == "" {
		return apigen.AddIncidentAnnotation422JSONResponse{ValidationFailedJSONResponse: validationFailed("note is required")}, nil
	}
	s.mu.Lock()
	_, ok := s.findIncident(req.Id)
	s.mu.Unlock()
	if !ok {
		return apigen.AddIncidentAnnotation404JSONResponse{NotFoundJSONResponse: notFound("incident not found")}, nil
	}
	return apigen.AddIncidentAnnotation201JSONResponse(apigen.IncidentAnnotation{
		Id:           s.newID("anno"),
		IncidentId:   req.Id,
		AuthorUserId: "usr_dev",
		Note:         req.Body.Note,
		CreatedAt:    time.Now().UTC(),
	}), nil
}

func (s *server) CloseIncident(_ context.Context, req apigen.CloseIncidentRequestObject) (apigen.CloseIncidentResponseObject, error) {
	s.mu.Lock()
	inc, ok := s.findIncident(req.Id)
	s.mu.Unlock()
	if !ok {
		return apigen.CloseIncident404JSONResponse{NotFoundJSONResponse: notFound("incident not found")}, nil
	}
	detail := incidentDetailFor(inc)
	ended := time.Now().UTC()
	detail.EndedAt = &ended
	reason := apigen.CloseReasonManual
	detail.CloseReason = &reason
	secs := int(ended.Sub(detail.StartedAt).Seconds())
	detail.DurationSeconds = &secs
	return apigen.CloseIncident200JSONResponse(detail), nil
}

// findIncident scans the dev monitors for the synthetic incident with the given id.
// Caller holds s.mu.
func (s *server) findIncident(id string) (apigen.Incident, bool) {
	for _, row := range s.monitors {
		for _, inc := range incidentsFor(row) {
			if inc.Id == id {
				return inc, true
			}
		}
	}
	return apigen.Incident{}, false
}

// --- auth-plane (hand-wired, not in the JSON contract) ---

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	setCookie(w, atCookie, devSession, true)
	setCookie(w, csrfCookie, "dev-csrf-token", false)
	dest := r.URL.Query().Get("return_to")
	if len(dest) == 0 || dest[0] != '/' {
		dest = "/"
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

func (s *server) refresh(w http.ResponseWriter, r *http.Request) {
	if !isAuthed(r) {
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "no session")
		return
	}
	setCookie(w, atCookie, devSession, true)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) logout(w http.ResponseWriter, _ *http.Request) {
	clearCookie(w, atCookie)
	clearCookie(w, csrfCookie)
	w.WriteHeader(http.StatusNoContent)
}

// requireAuth gates the /api/v1 contract on the dev session cookie, returning the
// standard 401 envelope so the SPA falls back to the login view.
func (s *server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAuthed(r) {
			writeErr(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Debug("dev api request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// openapiJSON serves the embedded spec so the contract the SPA sees matches the build.
func (s *server) openapiJSON(w http.ResponseWriter, _ *http.Request) {
	spec, err := apigen.GetSwagger()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "spec unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(spec)
}

func (s *server) swaggerUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(swaggerHTML))
}

// --- sample data + helpers ---

func (s *server) seed() {
	now := time.Now().UTC()
	lat1, check1 := 95, now.Add(-2*time.Minute)
	s.monitors["mon_1"] = &monRow{
		m:           baseMonitor("mon_1", "Marketing site", "https://example.com", 60, true, []string{"eu-central"}),
		status:      "up",
		lastCheckAt: &check1,
		lastLatency: &lat1,
	}
	lat2, check2 := 540, now.Add(-1*time.Minute)
	s.monitors["mon_2"] = &monRow{
		m:            baseMonitor("mon_2", "Prod API health", "https://api.example.com/health", 60, true, []string{"eu-central", "us-west"}),
		status:       "down",
		lastCheckAt:  &check2,
		lastLatency:  &lat2,
		incidentOpen: true,
		downReason:   "status_mismatch",
	}
	s.monitors["mon_3"] = &monRow{
		m:      baseMonitor("mon_3", "Staging", "https://staging.example.com", 300, false, []string{"eu-central"}),
		status: "disabled",
	}
	s.monitors["mon_4"] = &monRow{
		m:      sslMonitor("mon_4", "example.com cert", "example.com", 5),
		status: "up",
	}
	s.channels["chan_1"] = &apigen.Channel{
		Id: "chan_1", OrgId: devOrgID, Name: "Eng Slack", Type: "slack", Enabled: true,
		Config: map[string]any{"webhook_url_set": true},
	}
	// sample orgs for the admin panel's plan editor; static, mutated in-process.
	s.adminOrgs = []apigen.AdminOrg{
		{Id: devOrgID, Name: "Dev Workspace", Slug: "dev", Plan: devOrgPlan, CreatedAt: now.Add(-90 * 24 * time.Hour)},
		{Id: "org_acme", Name: "Acme Inc", Slug: "acme", Plan: "tier2", CreatedAt: now.Add(-30 * 24 * time.Hour)},
		{Id: "org_globex", Name: "Globex", Slug: "globex", Plan: "tier1", CreatedAt: now.Add(-7 * 24 * time.Hour)},
		{Id: "org_initech", Name: "Initech", Slug: "initech", Plan: "tierCustom", CreatedAt: now.Add(-2 * 24 * time.Hour)},
	}
	s.nextID = 100
}

func baseMonitor(id, name, url string, interval int, enabled bool, regions []string) apigen.Monitor {
	now := time.Now().UTC()
	return apigen.Monitor{
		Id: id, OrgId: devOrgID, Type: "http", Name: name, Url: url, Method: "GET",
		Headers: []apigen.MonitorHeader{}, ExpectedStatusCodes: "200",
		TimeoutSeconds: 10, IntervalSeconds: interval, Enabled: enabled, FailureThreshold: 1,
		NotificationChannelIds: []string{}, Regions: regions, DownPolicy: "quorum",
		CreatedAt: now, UpdatedAt: now,
	}
}

// sslMonitor builds a sample ssl monitor with a cert detail so the dev SPA can
// render the certificate card (BACKLOG: SSL-expiry).
func sslMonitor(id, name, host string, daysLeft int) apigen.Monitor {
	m := baseMonitor(id, name, host, 86400, true, []string{"eu-central"})
	m.Type = "ssl"
	now := time.Now().UTC()
	notAfter := now.AddDate(0, 0, daysLeft)
	m.Cert = &apigen.CertInfo{
		Subject:   host,
		Issuer:    "R3",
		NotBefore: now.AddDate(0, 0, -30),
		NotAfter:  notAfter,
		DnsNames:  []string{host, "www." + host},
		Serial:    "03:a1:b2:c3:d4",
	}
	return m
}

func monitorFromInput(id string, in apigen.MonitorInput) apigen.Monitor {
	if in.Headers == nil {
		in.Headers = []apigen.MonitorHeader{}
	}
	if in.NotificationChannelIds == nil {
		in.NotificationChannelIds = []string{}
	}
	if len(in.Regions) == 0 {
		in.Regions = []string{"eu-central"}
	}
	if in.DownPolicy == "" {
		in.DownPolicy = "quorum"
	}
	now := time.Now().UTC()
	if in.Type == "" {
		in.Type = "http"
	}
	return apigen.Monitor{
		Id: id, OrgId: devOrgID, Type: in.Type, Name: in.Name, Url: in.Url, Method: in.Method,
		Headers: in.Headers, Body: in.Body, ExpectedStatusCodes: in.ExpectedStatusCodes,
		TimeoutSeconds: in.TimeoutSeconds, IntervalSeconds: in.IntervalSeconds, Enabled: in.Enabled,
		MaxLatencyMs: in.MaxLatencyMs, BodyContains: in.BodyContains, FailureThreshold: in.FailureThreshold,
		NotificationChannelIds: in.NotificationChannelIds, Regions: in.Regions, DownPolicy: in.DownPolicy,
		CreatedAt: now, UpdatedAt: now,
	}
}

// incidentsFor builds the sample incident for a down monitor. Caller holds s.mu.
func incidentsFor(row *monRow) []apigen.Incident {
	if row.status != "down" {
		return []apigen.Incident{}
	}
	return []apigen.Incident{{
		Id:          "inc_" + row.m.Id,
		MonitorId:   row.m.Id,
		StartedAt:   time.Now().UTC().Add(-30 * time.Minute),
		CauseReason: row.downReason,
	}}
}

// incidentDetailFor maps a synthetic incident to the detail shape with one sample
// annotation, so the SPA's incident-detail view has something to render in dev mode.
func incidentDetailFor(inc apigen.Incident) apigen.IncidentDetail {
	return apigen.IncidentDetail{
		Id:              inc.Id,
		MonitorId:       inc.MonitorId,
		StartedAt:       inc.StartedAt,
		EndedAt:         inc.EndedAt,
		CauseReason:     inc.CauseReason,
		CloseReason:     inc.CloseReason,
		DurationSeconds: inc.DurationSeconds,
		Annotations: []apigen.IncidentAnnotation{{
			Id:           "anno_dev",
			IncidentId:   inc.Id,
			AuthorUserId: "usr_dev",
			Note:         "Investigating the failing health checks.",
			CreatedAt:    inc.StartedAt.Add(5 * time.Minute),
		}},
	}
}

func (s *server) newID(prefix string) string {
	s.nextID++
	return prefix + "_" + strconv.Itoa(s.nextID)
}

func notFound(msg string) apigen.NotFoundJSONResponse {
	return apigen.NotFoundJSONResponse{Error: apigen.Error{Code: "not_found", Message: msg}}
}

func validationFailed(msg string) apigen.ValidationFailedJSONResponse {
	return apigen.ValidationFailedJSONResponse{Error: apigen.Error{Code: "validation_failed", Message: msg}}
}

func forbidden(msg string) apigen.ForbiddenJSONResponse {
	return apigen.ForbiddenJSONResponse{Error: apigen.Error{Code: "forbidden", Message: msg}}
}

func intptr(i int) *int { return &i }

func isAuthed(r *http.Request) bool {
	c, err := r.Cookie(atCookie)
	return err == nil && c.Value == devSession
}

func setCookie(w http.ResponseWriter, name, value string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: httpOnly, SameSite: http.SameSiteLaxMode})
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apigen.ErrorResponse{Error: apigen.Error{Code: code, Message: msg}})
}

const swaggerHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Pulse Pager API (dev)</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css"></head>
<body><div id="ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>window.onload=()=>SwaggerUIBundle({url:"/api/v1/openapi.json",dom_id:"#ui"});</script>
</body></html>`

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"pulse/internal/apigen"
	"pulse/internal/authn"
	"pulse/internal/domain"
)

// Router builds the real control-plane HTTP surface (RFC-003 / RFC-012):
//
//   - the auth-plane routes (login, callback, refresh, logout, logout-all, jwks),
//     hand-wired because they are redirects and non-JSON, not part of the JSON
//     resource contract (the spec note in v1.yaml keeps them out of the spec);
//   - the generated JSON resource routes for the identity slice (me, account, orgs),
//     each behind the right middleware: Identify for the authenticated routes,
//     Identify + RequireOrg for /orgs/{orgId}.
//
// Unauthenticated: /auth/* and /.well-known/jwks.json. Authenticated: everything
// under /api/v1.
func (s *Server) Router() http.Handler {
	// The strict handler turns *Server into the generated ServerInterface, writing
	// the typed responses. We control its error handling so a handler error and a
	// request-decode error both produce the localizable envelope, not a bare 400/500.
	si := apigen.NewStrictHandlerWithOptions(s, nil, apigen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  envelopeError(http.StatusUnprocessableEntity, "validation_failed"),
		ResponseErrorHandlerFunc: envelopeError(http.StatusInternalServerError, "internal"),
	})
	wrapper := apigen.ServerInterfaceWrapper{
		Handler:          si,
		ErrorHandlerFunc: envelopeError(http.StatusBadRequest, "bad_request"),
	}

	identify := s.auth.Identify
	requireOrg := s.auth.RequireOrg

	mux := http.NewServeMux()

	// --- auth-plane (hand-wired, unauthenticated) ---
	mux.HandleFunc("GET /auth/{provider}/login", s.handleLogin)
	mux.HandleFunc("GET /auth/{provider}/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/refresh", s.handleRefresh)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("POST /auth/logout-all", s.handleLogoutAll)
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
	mux.HandleFunc("GET /auth/jwks", s.handleJWKS)
	// Link-start needs a session, so it runs behind Identify.
	mux.Handle("GET /auth/{provider}/link", identify(http.HandlerFunc(s.handleLinkStart)))
	// Passwordless email login (RFC-003): start emails a one-time link, verify
	// consumes the token and mints the session. Both are unauthenticated like the
	// rest of the auth-plane: start is enumeration-safe and rate-limited, verify is
	// a single-use token from the email.
	mux.HandleFunc("POST /auth/email/start", s.handleEmailStart)
	mux.HandleFunc("GET /auth/email/verify", s.handleEmailVerify)

	// dev-login (local/dev only): registered ONLY when DevLogin is on. In production
	// it is off, so the route is absent and a request gets a 404. It signs a developer
	// in with a real account, no OAuth creds needed (see devlogin.go).
	if s.devLogin {
		mux.HandleFunc("POST /auth/dev/login", s.handleDevLogin)
	}

	// --- JSON resource routes: identity slice (authenticated) ---
	// account / me: Identify only (self-scoped, no org).
	mux.Handle("GET /api/v1/me", identify(http.HandlerFunc(wrapper.GetMe)))
	mux.Handle("PATCH /api/v1/me", identify(http.HandlerFunc(wrapper.UpdateMe)))
	mux.Handle("GET /api/v1/me/identities", identify(http.HandlerFunc(wrapper.ListMyIdentities)))
	mux.Handle("DELETE /api/v1/me/identities/{provider}", identify(http.HandlerFunc(wrapper.UnlinkMyIdentity)))
	mux.Handle("POST /api/v1/account/logout-all", identify(http.HandlerFunc(wrapper.LogoutAll)))

	// plans: the public plan catalog is reference config (PRD-006 3); it needs a
	// session but no org and no role gate (Identify only).
	mux.Handle("GET /api/v1/plans", identify(http.HandlerFunc(wrapper.ListPlans)))

	// admin panel: platform-wide totals, not org-scoped. adminAuth verifies the
	// Cloudflare Access identity (or falls back to the session in local/dev); the
	// handler then enforces the PULSE_PLATFORM_ADMINS allowlist and 403s a non-admin.
	mux.Handle("GET /api/v1/admin/metrics", s.adminAuth(http.HandlerFunc(wrapper.GetAdminMetrics)))
	// admin: cross-org billing summary (paid orgs, subscription statuses, revenue).
	mux.Handle("GET /api/v1/admin/billing", s.adminAuth(http.HandlerFunc(wrapper.GetAdminBilling)))
	// admin: list every org and set an org's plan by hand (operator override
	// alongside Paddle self-serve billing). Same adminAuth + allowlist as the metrics endpoint.
	mux.Handle("GET /api/v1/admin/orgs", s.adminAuth(http.HandlerFunc(wrapper.ListAdminOrgs)))
	mux.Handle("PUT /api/v1/admin/orgs/{orgId}/plan", s.adminAuth(http.HandlerFunc(wrapper.SetAdminOrgPlan)))
	// admin billing: cancel a subscription and refund a payment (RFC-018 5.2/5.3). Same
	// adminAuth + allowlist; both call the provider and are audited.
	mux.Handle("POST /api/v1/admin/orgs/{orgId}/subscription/cancel", s.adminAuth(http.HandlerFunc(wrapper.CancelAdminOrgSubscription)))
	mux.Handle("POST /api/v1/admin/orgs/{orgId}/refund", s.adminAuth(http.HandlerFunc(wrapper.RefundAdminOrgPayment)))

	// orgs: list/create are per-user (Identify only).
	mux.Handle("GET /api/v1/orgs", identify(http.HandlerFunc(wrapper.ListOrgs)))
	mux.Handle("POST /api/v1/orgs", identify(http.HandlerFunc(wrapper.CreateOrg)))
	// get-one is org-scoped: RequireOrg checks membership and stamps the role, so a
	// non-member is 403 before the handler runs.
	mux.Handle("GET /api/v1/orgs/{orgId}", identify(requireOrg(http.HandlerFunc(wrapper.GetOrg))))

	// entitlements: org-scoped usage vs caps for the billing/usage screen. RequireOrg
	// checks membership and stamps the role; the handler runs the authz.Can gate
	// (view billing = owner/admin, PRD-006 9).
	mux.Handle("GET /api/v1/orgs/{orgId}/entitlements", identify(requireOrg(http.HandlerFunc(wrapper.GetEntitlements))))

	// self-serve billing: hosted checkout to buy a paid plan and the customer portal to
	// manage it (RFC-018 6). Org-scoped; the handlers run the ActionManageBilling gate
	// (owner/admin). Both return a provider-hosted URL the FE redirects to.
	mux.Handle("POST /api/v1/orgs/{orgId}/billing/checkout", identify(requireOrg(http.HandlerFunc(wrapper.CreateBillingCheckout))))
	mux.Handle("POST /api/v1/orgs/{orgId}/billing/portal", identify(requireOrg(http.HandlerFunc(wrapper.CreateBillingPortal))))
	// billing payments mirror for the billing screen (RFC-018 4); owner/admin read.
	mux.Handle("GET /api/v1/orgs/{orgId}/billing/payments", identify(requireOrg(http.HandlerFunc(wrapper.ListBillingPayments))))

	// --- members + invitations (org-scoped: Identify + RequireOrg) ---
	// RequireOrg checks membership and stamps the role; the handlers then run the
	// authz.Can role gate (view/invite/manage/change/remove/transfer) per PRD-001 7.2.
	mux.Handle("GET /api/v1/orgs/{orgId}/members", identify(requireOrg(http.HandlerFunc(wrapper.ListMembers))))
	// /members/me must be registered alongside /members/{userId}; the stdlib mux
	// prefers the literal "me" segment over the {userId} wildcard, so leave is matched
	// before the generic member routes.
	mux.Handle("DELETE /api/v1/orgs/{orgId}/members/me", identify(requireOrg(http.HandlerFunc(wrapper.LeaveOrg))))
	mux.Handle("PATCH /api/v1/orgs/{orgId}/members/{userId}", identify(requireOrg(http.HandlerFunc(wrapper.ChangeMemberRole))))
	mux.Handle("DELETE /api/v1/orgs/{orgId}/members/{userId}", identify(requireOrg(http.HandlerFunc(wrapper.RemoveMember))))
	mux.Handle("POST /api/v1/orgs/{orgId}/transfer-ownership", identify(requireOrg(http.HandlerFunc(wrapper.TransferOwnership))))

	// --- api keys (org-scoped: Identify + RequireOrg) ---
	// RequireOrg checks membership and stamps the role; the handlers run the authz.Can
	// gate (manage_api_keys = owner/admin, PRD-001 7.2 / PRD-005 2).
	mux.Handle("GET /api/v1/orgs/{orgId}/api-keys", identify(requireOrg(http.HandlerFunc(wrapper.ListAPIKeys))))
	mux.Handle("POST /api/v1/orgs/{orgId}/api-keys", identify(requireOrg(http.HandlerFunc(wrapper.CreateAPIKey))))
	mux.Handle("DELETE /api/v1/orgs/{orgId}/api-keys/{id}", identify(requireOrg(http.HandlerFunc(wrapper.RevokeAPIKey))))

	// --- outbound webhooks (org-scoped: Identify + RequireOrg) ---
	// RequireOrg checks membership and stamps the role; the handlers run the authz.Can
	// gate (manage_webhooks = owner/admin, PRD-005 4.6 / 7.4). The signing secret is
	// returned once on create and rotate-secret, redacted otherwise.
	mux.Handle("GET /api/v1/orgs/{orgId}/webhooks", identify(requireOrg(http.HandlerFunc(wrapper.ListWebhooks))))
	mux.Handle("POST /api/v1/orgs/{orgId}/webhooks", identify(requireOrg(http.HandlerFunc(wrapper.CreateWebhook))))
	mux.Handle("GET /api/v1/orgs/{orgId}/webhooks/{id}", identify(requireOrg(http.HandlerFunc(wrapper.GetWebhook))))
	mux.Handle("PUT /api/v1/orgs/{orgId}/webhooks/{id}", identify(requireOrg(http.HandlerFunc(wrapper.UpdateWebhook))))
	mux.Handle("DELETE /api/v1/orgs/{orgId}/webhooks/{id}", identify(requireOrg(http.HandlerFunc(wrapper.DeleteWebhook))))
	mux.Handle("POST /api/v1/orgs/{orgId}/webhooks/{id}/rotate-secret", identify(requireOrg(http.HandlerFunc(wrapper.RotateWebhookSecret))))

	// --- API docs (NO auth) ---
	// The OpenAPI spec is the public API reference, so the spec and the Swagger UI are
	// unauthenticated. They are registered OUTSIDE Identify (RFC-012 8.3, PRD-005).
	mux.HandleFunc("GET /api/openapi.json", s.handleOpenAPISpec)
	mux.HandleFunc("GET /api/openapi.yaml", s.handleOpenAPISpec)
	mux.HandleFunc("GET /api/docs", s.handleSwaggerUI)

	mux.Handle("GET /api/v1/orgs/{orgId}/invitations", identify(requireOrg(http.HandlerFunc(wrapper.ListInvitations))))
	mux.Handle("POST /api/v1/orgs/{orgId}/invitations", identify(requireOrg(http.HandlerFunc(wrapper.CreateInvitation))))
	mux.Handle("DELETE /api/v1/orgs/{orgId}/invitations/{id}", identify(requireOrg(http.HandlerFunc(wrapper.RevokeInvitation))))
	mux.Handle("POST /api/v1/orgs/{orgId}/invitations/{id}/resend", identify(requireOrg(http.HandlerFunc(wrapper.ResendInvitation))))

	// --- channels (org-scoped: Identify + RequireOrg) ---
	// RequireOrg checks membership and stamps the role; the handlers then run the
	// authz.Can gate (ActionManageChannels = member+, PRD-003 / PRD-001 7.2). The
	// channel-type catalog drives the config forms; the test endpoint sends a one-off.
	mux.Handle("GET /api/v1/orgs/{orgId}/channel-types", identify(requireOrg(http.HandlerFunc(wrapper.GetChannelTypes))))
	mux.Handle("GET /api/v1/orgs/{orgId}/channels", identify(requireOrg(http.HandlerFunc(wrapper.ListChannels))))
	mux.Handle("POST /api/v1/orgs/{orgId}/channels", identify(requireOrg(http.HandlerFunc(wrapper.CreateChannel))))
	mux.Handle("PUT /api/v1/orgs/{orgId}/channels/{id}", identify(requireOrg(http.HandlerFunc(wrapper.UpdateChannel))))
	mux.Handle("DELETE /api/v1/orgs/{orgId}/channels/{id}", identify(requireOrg(http.HandlerFunc(wrapper.DeleteChannel))))
	mux.Handle("POST /api/v1/orgs/{orgId}/channels/{id}/test", identify(requireOrg(http.HandlerFunc(wrapper.TestChannel))))

	// --- monitors (org-scoped: Identify + RequireOrg) ---
	// RequireOrg checks membership and stamps the role; the handlers then run the
	// authz.Can gate (view = any member, create/edit/delete/check = member+, PRD-001 7.2).
	mux.Handle("GET /api/v1/orgs/{orgId}/monitors", identify(requireOrg(http.HandlerFunc(wrapper.ListMonitors))))
	mux.Handle("POST /api/v1/orgs/{orgId}/monitors", identify(requireOrg(http.HandlerFunc(wrapper.CreateMonitor))))
	mux.Handle("GET /api/v1/orgs/{orgId}/monitors/{id}", identify(requireOrg(http.HandlerFunc(wrapper.GetMonitor))))
	mux.Handle("PUT /api/v1/orgs/{orgId}/monitors/{id}", identify(requireOrg(http.HandlerFunc(wrapper.UpdateMonitor))))
	mux.Handle("DELETE /api/v1/orgs/{orgId}/monitors/{id}", identify(requireOrg(http.HandlerFunc(wrapper.DeleteMonitor))))
	mux.Handle("POST /api/v1/orgs/{orgId}/monitors/{id}/check", identify(requireOrg(http.HandlerFunc(wrapper.CheckNow))))
	mux.Handle("GET /api/v1/orgs/{orgId}/monitors/{id}/results", identify(requireOrg(http.HandlerFunc(wrapper.ListResults))))
	mux.Handle("GET /api/v1/orgs/{orgId}/monitors/{id}/incidents", identify(requireOrg(http.HandlerFunc(wrapper.ListMonitorIncidents))))
	// Live per-region check state for the org's monitors, polled by the frontend to
	// render a chip per region (RFC-004 §9). Any member may view (it is a read).
	mux.Handle("GET /api/v1/orgs/{orgId}/monitor-region-states", identify(requireOrg(http.HandlerFunc(wrapper.GetMonitorRegionStates))))

	// --- incidents (org-scoped: Identify + RequireOrg) ---
	// list/get/annotate = any member (view_monitoring); close = owner/admin
	// (close_incident, PRD-001 7.2). The handlers run the authz.Can gate.
	mux.Handle("GET /api/v1/orgs/{orgId}/incidents", identify(requireOrg(http.HandlerFunc(wrapper.ListIncidents))))
	mux.Handle("GET /api/v1/orgs/{orgId}/incidents/{id}", identify(requireOrg(http.HandlerFunc(wrapper.GetIncident))))
	mux.Handle("POST /api/v1/orgs/{orgId}/incidents/{id}/annotations", identify(requireOrg(http.HandlerFunc(wrapper.AddIncidentAnnotation))))
	mux.Handle("POST /api/v1/orgs/{orgId}/incidents/{id}/close", identify(requireOrg(http.HandlerFunc(wrapper.CloseIncident))))

	// --- status pages (org-scoped: Identify + RequireOrg) ---
	// RequireOrg checks membership and stamps the role; the handlers run the authz.Can
	// gate (view = any member, create/edit/publish/select-monitors = member+, PRD-004 10).
	mux.Handle("GET /api/v1/orgs/{orgId}/status-pages", identify(requireOrg(http.HandlerFunc(wrapper.ListStatusPages))))
	mux.Handle("POST /api/v1/orgs/{orgId}/status-pages", identify(requireOrg(http.HandlerFunc(wrapper.CreateStatusPage))))
	mux.Handle("GET /api/v1/orgs/{orgId}/status-pages/{id}", identify(requireOrg(http.HandlerFunc(wrapper.GetStatusPage))))
	mux.Handle("PUT /api/v1/orgs/{orgId}/status-pages/{id}", identify(requireOrg(http.HandlerFunc(wrapper.UpdateStatusPage))))
	mux.Handle("DELETE /api/v1/orgs/{orgId}/status-pages/{id}", identify(requireOrg(http.HandlerFunc(wrapper.DeleteStatusPage))))
	mux.Handle("PUT /api/v1/orgs/{orgId}/status-pages/{id}/publish", identify(requireOrg(http.HandlerFunc(wrapper.PublishStatusPage))))

	// --- public status page (NO auth, NO org context) ---
	// The published page is public by design (PRD-004 6/10). It is registered OUTSIDE
	// Identify/RequireOrg so it needs no session and no org; the store reads it on the
	// public-page capability and returns only the privacy-safe projection (PRD-004 3.6).
	mux.Handle("GET /api/v1/public/status-pages/{slug}", http.HandlerFunc(wrapper.GetPublicStatusPage))

	// --- accept flow (token-based, NOT org-scoped) ---
	// Preview is pre-login: it reads the invitation through the token capability with
	// no session and no org (RFC-003 2.6, PRD-001 6.5).
	mux.Handle("GET /api/v1/invitations/{token}", http.HandlerFunc(wrapper.GetInvitationPreview))
	// Accept needs a session (Identify only) so the verified email can be matched
	// against the invited email; the org is taken from the invitation, not the URL.
	mux.Handle("POST /api/v1/invitations/{token}/accept", identify(http.HandlerFunc(wrapper.AcceptInvitation)))

	return mux
}

// --- API docs handlers (unauthenticated; the spec is the public API reference) ---

// handleOpenAPISpec serves the OpenAPI spec built into the binary (RFC-012 8.3), so
// the served contract always matches the build. It serves JSON at /api/openapi.json
// and YAML at /api/openapi.yaml, picking the format from the URL suffix. Swagger UI
// loads this.
func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	specJSON, err := publicSpecJSON()
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, "internal", "spec unavailable")
		return
	}
	if strings.HasSuffix(r.URL.Path, ".yaml") || strings.HasSuffix(r.URL.Path, ".yml") {
		var doc any
		if err := json.Unmarshal(specJSON, &doc); err != nil {
			writeEnvelope(w, http.StatusInternalServerError, "internal", "spec unavailable")
			return
		}
		out, err := yaml.Marshal(doc)
		if err != nil {
			writeEnvelope(w, http.StatusInternalServerError, "internal", "spec unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(specJSON)
}

// publicSpecJSON is the OpenAPI spec with operator-only surface removed: paths
// tagged "admin" and the Admin* schemas. The full spec drives codegen (the FE
// client and Go stubs generate from api/openapi/v1.yaml at build time), but the
// publicly served spec and Swagger UI must not advertise the admin endpoints or
// their response shapes. Filtering by tag/name keeps this automatic as admin grows.
func publicSpecJSON() ([]byte, error) {
	full, err := apigen.GetSpecJSON()
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(full, &doc); err != nil {
		return nil, err
	}
	if paths, ok := doc["paths"].(map[string]any); ok {
		for path, item := range paths {
			if operationHasTag(item, "admin") {
				delete(paths, path)
			}
		}
	}
	if comps, ok := doc["components"].(map[string]any); ok {
		if schemas, ok := comps["schemas"].(map[string]any); ok {
			for name := range schemas {
				if strings.HasPrefix(name, "Admin") {
					delete(schemas, name)
				}
			}
		}
	}
	return json.Marshal(doc)
}

// operationHasTag reports whether any operation on a path item carries the tag.
func operationHasTag(pathItem any, tag string) bool {
	ops, ok := pathItem.(map[string]any)
	if !ok {
		return false
	}
	for _, op := range ops {
		opMap, ok := op.(map[string]any)
		if !ok {
			continue
		}
		tags, ok := opMap["tags"].([]any)
		if !ok {
			continue
		}
		for _, t := range tags {
			if s, _ := t.(string); s == tag {
				return true
			}
		}
	}
	return false
}

// handleSwaggerUI serves the Swagger UI page that loads the served spec. The UI
// assets come from the swagger-ui CDN (matching the dev API), so the page is a small
// self-contained HTML that points at /api/openapi.yaml.
func (s *Server) handleSwaggerUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(swaggerHTML))
}

// swaggerHTML is the Swagger UI shell, loading the UI bundle from the CDN and the
// served spec from /api/openapi.yaml.
const swaggerHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Pulse Pager API</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css"></head>
<body><div id="ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>window.onload=()=>SwaggerUIBundle({url:"/api/openapi.yaml",dom_id:"#ui"});</script>
</body></html>`

// --- auth-plane handlers ---

// handleLogin starts a social login: it makes the per-attempt state, stores the
// flow in Redis, sets the state cookie, and redirects the browser to the IdP
// (RFC-003 2.5). The optional return_to query is validated against the internal
// path allowlist inside StartLogin.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	provider := domain.IdentityProvider(r.PathValue("provider"))
	returnTo := r.URL.Query().Get("return_to")
	state, redirectURL, err := s.login.StartLogin(r.Context(), provider, authn.FlowLogin, returnTo, 0)
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, "invalid_provider", "unknown or misconfigured provider")
		return
	}
	s.cookies.SetOAuthState(w, state, 10*time.Minute)
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// handleLinkStart starts a manual account-link flow for the signed-in user: it
// attaches the provider identity to the current user at the callback (RFC-003 2.4).
func (s *Server) handleLinkStart(w http.ResponseWriter, r *http.Request) {
	p, ok := authn.FromContext(r.Context())
	if !ok || p.Kind != "human" {
		writeEnvelope(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
		return
	}
	provider := domain.IdentityProvider(r.PathValue("provider"))
	returnTo := r.URL.Query().Get("return_to")
	state, redirectURL, err := s.login.StartLogin(r.Context(), provider, authn.FlowLink, returnTo, p.UserID)
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, "invalid_provider", "unknown or misconfigured provider")
		return
	}
	s.cookies.SetOAuthState(w, state, 10*time.Minute)
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// handleCallback handles the IdP redirect: it cross-checks the state cookie against
// the query state, exchanges the code for the verified profile, resolves the user
// (linking or first-sign-in), mints the session cookies, and redirects into the app
// (RFC-003 2.5). The state cookie is single-use and cleared here.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	s.cookies.ClearOAuthState(w)

	var cookieState string
	if c, err := r.Cookie(authn.OAuthStateCookie); err == nil {
		cookieState = c.Value
	}
	queryState := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")

	res, err := s.login.HandleCallback(r.Context(), cookieState, queryState, code)
	if err != nil {
		// State/nonce mismatch, unverified email, or exchange failure all abort the
		// flow with no session. Send the browser to a login-failed page.
		s.redirectToApp(w, r, "/login?error=auth_failed")
		return
	}

	if err := s.issueSession(w, r, res.UserID, res.Email); err != nil {
		s.redirectToApp(w, r, "/login?error=session_failed")
		return
	}

	dest := res.ReturnTo
	if dest == "" {
		dest = "/"
	}
	if res.IsNew {
		dest = "/onboarding"
	}
	s.redirectToApp(w, r, dest)
}

// handleRefresh rotates the refresh cookie and mints a fresh access cookie
// (RFC-003 4.1). Reuse detection revokes the family and clears the session.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(authn.RefreshCookie)
	if err != nil || c.Value == "" {
		writeEnvelope(w, http.StatusUnauthorized, "unauthenticated", "no refresh token")
		return
	}
	rot, err := s.refresh.Rotate(r.Context(), c.Value)
	if err != nil {
		// Reuse or invalid: clear the session and force re-login.
		s.cookies.ClearSession(w)
		writeEnvelope(w, http.StatusUnauthorized, "unauthenticated", "refresh failed, please sign in again")
		return
	}
	access, accessExp, err := s.jwt.Issue(rot.UserID, "")
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, "internal", "could not issue token")
		return
	}
	csrf, _ := newCSRF()
	s.cookies.SetSession(w, access, rot.Raw, csrf, accessExp, rot.Expires)
	w.WriteHeader(http.StatusNoContent)
}

// handleLogout revokes the presented refresh family (this device) and clears the
// session cookies (RFC-003 4.3).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(authn.RefreshCookie); err == nil && c.Value != "" {
		_ = s.refresh.RevokeFamily(r.Context(), c.Value)
	}
	s.cookies.ClearSession(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleLogoutAll revokes every refresh family for the user (all devices) and
// clears this browser's cookies (RFC-003 4.3). It needs to resolve the user, which
// it does from the access cookie/JWT without going through the full Identify chain
// so it can still run when the access token is near expiry.
func (s *Server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	var userID int64
	if c, err := r.Cookie(authn.AccessCookie); err == nil && c.Value != "" {
		if vt, verr := s.jwt.Verify(c.Value); verr == nil {
			userID = vt.UserID
		}
	}
	if userID == 0 {
		// Fall back to the refresh token's owner so a stale access token still works.
		if c, err := r.Cookie(authn.RefreshCookie); err == nil && c.Value != "" {
			if rot, rerr := s.refresh.Rotate(r.Context(), c.Value); rerr == nil {
				userID = rot.UserID
			}
		}
	}
	if userID == 0 {
		writeEnvelope(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
		return
	}
	_ = s.refresh.RevokeAll(r.Context(), userID)
	s.cookies.ClearSession(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleJWKS serves the public key set so a verifier can pick the right key by kid
// (RFC-003 3.5).
func (s *Server) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	b, err := s.jwt.JWKS()
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, "internal", "jwks unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(b)
}

// --- session minting ---

// issueSession mints a refresh token (new family) and a fresh access JWT, then sets
// the access/refresh/CSRF cookies (RFC-003 4.4). The user's email is read once for
// the access-token claim.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID int64, email string) error {
	issued, err := s.refresh.Issue(r.Context(), userID)
	if err != nil {
		return err
	}
	access, accessExp, err := s.jwt.Issue(userID, email)
	if err != nil {
		return err
	}
	csrf, err := newCSRF()
	if err != nil {
		return err
	}
	s.cookies.SetSession(w, access, issued.Raw, csrf, accessExp, issued.Expires)
	return nil
}

// redirectToApp redirects to a path under the app base URL. dest is already a safe
// relative path (the login flow validated return_to); this just joins it onto the
// SPA origin so the browser lands in the app.
func (s *Server) redirectToApp(w http.ResponseWriter, r *http.Request, dest string) {
	if dest == "" || dest[0] != '/' {
		dest = "/"
	}
	target := dest
	if s.appBaseURL != "" {
		target = s.appBaseURL + dest
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// --- error envelope writers (RFC-012 / RFC-014 localizable {code, message}) ---

// writeEnvelope writes the localizable error envelope as JSON.
func writeEnvelope(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apigen.ErrorResponse{Error: apigen.Error{Code: code, Message: msg}})
}

// envelopeError adapts writeEnvelope to the (w, r, err) signature the generated
// wrapper and strict handler call on a decode/encode error.
func envelopeError(status int, code string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		msg := "request failed"
		if err != nil {
			msg = err.Error()
		}
		writeEnvelope(w, status, code, msg)
	}
}

// newCSRF mints a CSRF token for the double-submit cookie (RFC-003 4.5).
func newCSRF() (string, error) {
	return authn.NewCSRFToken()
}

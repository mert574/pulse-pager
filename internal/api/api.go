// Package api is the real control-plane HTTP implementation of the generated API
// contract (internal/apigen, from api/openapi/v1.yaml). Unlike internal/devapi
// (the fake used with PULSE_DEV_AUTH), this wires the real authn login/callback/
// refresh/logout, JWT/JWKS, the Authenticator middleware (Identify for
// authenticated routes, RequireOrg for /orgs/{orgId} routes), authz.Can where a
// role check is needed, and the Postgres store for reads/writes.
//
// Scope here is identity + onboarding (RFC-003 / PRD-001): auth flow, session,
// current user, account, and orgs create/list/get. Members and invitations are a
// later feature, so those StrictServerInterface methods that fall outside this
// slice (monitors, channels, incidents, entitlements) are not implemented in this
// package yet; cmd/api mounts only the identity routes from this server.
package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"pulse/internal/apigen"
	"pulse/internal/authn"
	"pulse/internal/authz"
	"pulse/internal/billing"
	"pulse/internal/checkstate"
	"pulse/internal/domain"
	"pulse/internal/entitlements"
	"pulse/internal/notify"
	"pulse/internal/store"
)

// Server holds the dependencies the identity handlers need. It implements the
// subset of apigen.StrictServerInterface this slice owns (me, account, orgs); the
// auth-plane routes (login/callback/refresh/logout/jwks) are hand-wired in router.go
// because they are redirects and non-JSON, not part of the JSON resource contract.
type Server struct {
	store   *store.Pool
	login   *authn.LoginService
	jwt     *authn.JWTIssuer
	refresh *authn.RefreshService
	cookies authn.CookieConfig
	auth    *authn.Authenticator
	// keys verifies API keys; the revoke handler calls InvalidateAPIKey on it so a
	// revoked key misses the cache and the next request sees the revoked row.
	keys *authn.APIKeyVerifier

	// seats decides the per-org seat cap so the invite path can block an over-seat
	// invite without hardcoding plan limits (PRD-001 5.2). Nil falls back to the
	// default per-plan resolver.
	seats entitlements.SeatResolver
	// monitors decides the per-org monitor cap, interval floor, and region set so the
	// create/update path enforces the plan without hardcoding limits (PRD-006). Nil
	// falls back to the default per-plan resolver.
	monitors entitlements.MonitorResolver
	// statusPages decides the per-org status-page count cap so the create path blocks
	// over the plan without hardcoding limits (PRD-004 2.3). Nil falls back to the
	// default per-plan resolver.
	statusPages entitlements.StatusPageResolver
	// ents resolves feature flags (e.g. failure-snapshot) so check-now captures a
	// response only when the plan allows it (RFC-009). Nil falls back to AllOn.
	ents entitlements.Resolver
	// jobs enqueues check jobs for check-now (the same pipeline scheduled checks use),
	// so a manual check fans out per region through the worker. Nil = skip enqueue
	// (dev/test); the request still returns 202 with the regions scheduled.
	jobs CheckJobPublisher
	// state is the live per-(monitor,region) check-state store (Redis). Check-now marks
	// regions scheduled on it and the region-states endpoint reads it. Nil = skip.
	state checkstate.MultiStore
	// changed publishes monitor.changed so the live schedule tracks a create/update/
	// enable/disable/delete (RFC-002, PRD-006 5). Nil = skip (dev/test without a bus).
	changed MonitorPublisher
	// mailer sends the invite email with the tokenized accept link. Nil falls back
	// to a no-op so the flow still completes (the link is logged).
	mailer notify.Mailer

	// registry is the channel-type registry the channel CRUD reads: it builds the
	// type catalog, validates config, and tells the store which config keys are
	// secret (so the same keys are encrypted at rest and redacted on read). Defaults
	// to notify.Default() (all built-in providers).
	registry *notify.Registry
	// tester sends a one-off test message for POST /channels/{id}/test. Defaults to
	// a notify.Manager; a test can stub it.
	tester channelTester
	// cooldown gates manual check-now per monitor so the button cannot be used as
	// free high-frequency monitoring (PRD-006). Backed by Redis so the cooldown
	// survives an api restart (the service is killed often on CD); nil disables it
	// (dev/test), in which case manual checks run with no cooldown.
	cooldown CheckNowGate

	// appBaseURL is where the SPA lives; the OAuth callback redirects back into it,
	// and the invite accept link points at its /invitations/{token} route.
	appBaseURL string

	// devLogin, when true, registers POST /auth/dev/login so a developer can sign in
	// locally without OAuth creds. Dev-only; never true in production. When false the
	// route is not registered at all, so it 404s.
	devLogin bool

	// platformAdmins is the lowercased email set allowed into the operator admin
	// panel (GET /admin/metrics). Empty means no admins, so the panel is closed by
	// default (fails safe). Checked on every admin request, not just at nav time.
	platformAdmins map[string]bool

	// cfAccess verifies the Cloudflare Access token on the admin origin. Non-nil only
	// when CF Access is configured; then the admin endpoint authorizes off the
	// verified CF Access identity instead of an app session. Nil = fall back to the
	// normal session + allowlist (local/dev).
	cfAccess *authn.CFAccessVerifier

	// billing is the payment provider used by the operator and self-serve billing
	// endpoints (RFC-018). Nil = no provider wired (dev/test); the handlers then apply
	// the local override only and skip the provider call.
	billing billing.Provider
	// audit emits audit.events for operator billing actions (RFC-018 8). Nil skips the
	// emit (dev/test without a bus); the action still happens.
	audit AuditPublisher
}

// Config is what the caller (cmd/api) passes to build the Server.
type Config struct {
	Store      *store.Pool
	Login      *authn.LoginService
	JWT        *authn.JWTIssuer
	Refresh    *authn.RefreshService
	Cookies    authn.CookieConfig
	Auth       *authn.Authenticator
	Keys       *authn.APIKeyVerifier
	AppBaseURL string
	// DevLogin registers the guarded dev-login route (POST /auth/dev/login). Dev-only,
	// default false; must stay false in production so the route does not exist.
	DevLogin bool
	// Seats and Mailer are optional; New fills sane defaults when they are nil.
	Seats  entitlements.SeatResolver
	Mailer notify.Mailer
	// Monitors, Ents, and Changed are optional; New fills sane defaults when they are
	// nil (the monitor handlers run with the default plan limits, all features on, and
	// no scheduler publish).
	Monitors    entitlements.MonitorResolver
	StatusPages entitlements.StatusPageResolver
	Ents        entitlements.Resolver
	Changed     MonitorPublisher
	// Jobs enqueues check-now jobs; State is the live region-state store; CheckNow is
	// the Redis cooldown gate. All optional: nil disables that piece (dev/test).
	Jobs     CheckJobPublisher
	State    checkstate.MultiStore
	CheckNow CheckNowGate
	// PlatformAdmins is the lowercased email allowlist for the admin panel. Empty
	// disables it (no admins). Optional.
	PlatformAdmins []string
	// CFAccess verifies the Cloudflare Access token on the admin endpoint. Optional;
	// nil means the admin endpoint uses the normal session + allowlist (local/dev).
	CFAccess *authn.CFAccessVerifier
	// Billing is the payment provider for the operator/self-serve billing endpoints
	// (RFC-018). Optional; nil applies the local override only and skips provider calls.
	Billing billing.Provider
	// Audit emits audit.events for operator billing actions. Optional; nil skips it.
	Audit AuditPublisher
}

// New builds the identity API server.
func New(cfg Config) *Server {
	seats := cfg.Seats
	if seats == nil {
		seats = entitlements.DefaultSeats{}
	}
	mailer := cfg.Mailer
	if mailer == nil {
		mailer = notify.LogMailer{}
	}
	monitors := cfg.Monitors
	if monitors == nil {
		monitors = entitlements.DefaultMonitors{}
	}
	statusPages := cfg.StatusPages
	if statusPages == nil {
		statusPages = entitlements.DefaultStatusPages{}
	}
	ents := cfg.Ents
	if ents == nil {
		ents = entitlements.AllOn{}
	}
	reg := notify.Default()
	admins := make(map[string]bool, len(cfg.PlatformAdmins))
	for _, e := range cfg.PlatformAdmins {
		admins[strings.ToLower(e)] = true
	}
	return &Server{
		store:          cfg.Store,
		login:          cfg.Login,
		jwt:            cfg.JWT,
		refresh:        cfg.Refresh,
		cookies:        cfg.Cookies,
		auth:           cfg.Auth,
		keys:           cfg.Keys,
		seats:          seats,
		monitors:       monitors,
		statusPages:    statusPages,
		ents:           ents,
		jobs:           cfg.Jobs,
		state:          cfg.State,
		changed:        cfg.Changed,
		mailer:         mailer,
		registry:       reg,
		tester:         notify.NewManager(nil, nil),
		cooldown:       cfg.CheckNow,
		appBaseURL:     cfg.AppBaseURL,
		devLogin:       cfg.DevLogin,
		platformAdmins: admins,
		cfAccess:       cfg.CFAccess,
		billing:        cfg.Billing,
		audit:          cfg.Audit,
	}
}

// --- StrictServerInterface: account / me ---

// GetMe returns the signed-in user plus the orgs they belong to and the role in
// each (RFC-003 6, PRD-001 7.3). The principal is set by the Identify middleware.
func (s *Server) GetMe(ctx context.Context, _ apigen.GetMeRequestObject) (apigen.GetMeResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.GetMe401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	me, err := s.buildMe(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	return apigen.GetMe200JSONResponse(*me), nil
}

// UpdateMe writes the editable profile fields (name, locale, timezone). An omitted
// field is left unchanged. Self-scoped: the actor is the subject, so no role check.
func (s *Server) UpdateMe(ctx context.Context, req apigen.UpdateMeRequestObject) (apigen.UpdateMeResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.UpdateMe401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if req.Body == nil {
		return apigen.UpdateMe422JSONResponse{ValidationFailedJSONResponse: validationFailed("body required")}, nil
	}
	u, err := s.store.GetUser(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if req.Body.Name != nil {
		if *req.Body.Name == "" {
			return apigen.UpdateMe422JSONResponse{ValidationFailedJSONResponse: validationFailed("name cannot be empty")}, nil
		}
		u.Name = *req.Body.Name
	}
	if req.Body.Locale != nil {
		u.Locale = *req.Body.Locale
	}
	if req.Body.Timezone != nil {
		u.Timezone = *req.Body.Timezone
	}
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return nil, err
	}
	me, err := s.buildMe(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	return apigen.UpdateMe200JSONResponse(*me), nil
}

// ListMyIdentities returns the social identities linked to the signed-in user.
func (s *Server) ListMyIdentities(ctx context.Context, _ apigen.ListMyIdentitiesRequestObject) (apigen.ListMyIdentitiesResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.ListMyIdentities401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	idns, err := s.store.ListIdentitiesForUser(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.Identity, 0, len(idns))
	for _, idn := range idns {
		out = append(out, apigen.Identity{
			Provider:       apigen.IdentityProviderName(idn.Provider),
			ProviderUserId: idn.ProviderUserID,
			CreatedAt:      idn.CreatedAt,
		})
	}
	return apigen.ListMyIdentities200JSONResponse(out), nil
}

// UnlinkMyIdentity removes a social identity from the signed-in user. It refuses to
// remove the last identity, so the user is never left with no way to sign in (409).
func (s *Server) UnlinkMyIdentity(ctx context.Context, req apigen.UnlinkMyIdentityRequestObject) (apigen.UnlinkMyIdentityResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.UnlinkMyIdentity401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	idns, err := s.store.ListIdentitiesForUser(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	provider := domain.IdentityProvider(req.Provider)
	var target *domain.UserIdentity
	for _, idn := range idns {
		if idn.Provider == provider {
			target = idn
			break
		}
	}
	if target == nil {
		return apigen.UnlinkMyIdentity404JSONResponse{NotFoundJSONResponse: notFound("identity not linked")}, nil
	}
	if len(idns) <= 1 {
		return apigen.UnlinkMyIdentity409JSONResponse{ConflictJSONResponse: conflict("cannot unlink your only sign-in method")}, nil
	}
	if _, err := s.store.UnlinkIdentity(ctx, p.UserID, provider); err != nil {
		return nil, err
	}
	return apigen.UnlinkMyIdentity204Response{}, nil
}

// LogoutAll revokes every refresh family for the user and clears the cookies, the
// "log out of all devices" lever (RFC-003 4.3). Self-scoped.
func (s *Server) LogoutAll(ctx context.Context, _ apigen.LogoutAllRequestObject) (apigen.LogoutAllResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.LogoutAll401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if err := s.refresh.RevokeAll(ctx, p.UserID); err != nil {
		return nil, err
	}
	// Revoking every refresh family is the security part: no device can refresh a
	// new access token. The access JWT expires on its own short TTL. The cookies on
	// this browser are cleared by the hand-wired POST /auth/logout-all route, which
	// the SPA calls alongside this contract endpoint (see router.go).
	return apigen.LogoutAll204Response{}, nil
}

// --- StrictServerInterface: orgs ---

// ListOrgs returns the orgs the signed-in user belongs to, with the role and plan
// in each (PRD-001 7.3). User-scoped, not org-scoped.
func (s *Server) ListOrgs(ctx context.Context, _ apigen.ListOrgsRequestObject) (apigen.ListOrgsResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.ListOrgs401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	memberships, err := s.orgMemberships(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	return apigen.ListOrgs200JSONResponse(memberships), nil
}

// CreateOrg creates a new org with the caller as owner (PRD-001 7.2: any role can
// create an org; it is per-user, not org-scoped). It mints a slug from the name
// when none is given, retrying on a slug clash.
func (s *Server) CreateOrg(ctx context.Context, req apigen.CreateOrgRequestObject) (apigen.CreateOrgResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.CreateOrg401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	if req.Body == nil || req.Body.Name == "" {
		return apigen.CreateOrg422JSONResponse{ValidationFailedJSONResponse: validationFailed("name required")}, nil
	}
	slug := ""
	if req.Body.Slug != nil {
		slug = *req.Body.Slug
	}
	org, role, err := s.store.CreateOrgWithOwner(ctx, req.Body.Name, slug, p.UserID)
	if err != nil {
		if errors.Is(err, store.ErrSlugTaken) {
			return apigen.CreateOrg422JSONResponse{ValidationFailedJSONResponse: validationFailed("slug already taken")}, nil
		}
		return nil, err
	}
	return apigen.CreateOrg201JSONResponse(orgMembershipDTO(org, role)), nil
}

// GetOrg returns one org the caller is a member of (RFC-003 6.2). RequireOrg has
// already checked membership and stamped the role on the principal, so a non-member
// is 403 before this runs.
func (s *Server) GetOrg(ctx context.Context, req apigen.GetOrgRequestObject) (apigen.GetOrgResponseObject, error) {
	p, ok := authn.FromContext(ctx)
	if !ok || p.Kind != authz.ActorHuman {
		return apigen.GetOrg401JSONResponse{UnauthorizedJSONResponse: unauthorized("sign in required")}, nil
	}
	orgID, err := strconv.ParseInt(req.OrgId, 10, 64)
	if err != nil {
		return apigen.GetOrg403JSONResponse{ForbiddenJSONResponse: forbidden("not a member of this org")}, nil
	}
	org, err := s.store.GetOrganization(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Do not leak existence: a missing org reads the same as not-a-member.
			return apigen.GetOrg403JSONResponse{ForbiddenJSONResponse: forbidden("not a member of this org")}, nil
		}
		return nil, err
	}
	return apigen.GetOrg200JSONResponse(orgMembershipDTO(org, p.Role)), nil
}

// --- helpers ---

// buildMe loads the user and their org memberships into the Me DTO.
func (s *Server) buildMe(ctx context.Context, userID int64) (*apigen.Me, error) {
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	memberships, err := s.orgMemberships(ctx, userID)
	if err != nil {
		return nil, err
	}
	var avatar *string
	if u.AvatarURL != "" {
		a := u.AvatarURL
		avatar = &a
	}
	return &apigen.Me{
		UserId:          strconv.FormatInt(u.ID, 10),
		Email:           u.Email,
		Name:            u.Name,
		AvatarUrl:       avatar,
		Locale:          u.Locale,
		Timezone:        u.Timezone,
		Orgs:            memberships,
		IsPlatformAdmin: s.isPlatformAdmin(u.Email),
	}, nil
}

// isPlatformAdmin reports whether an email is in the platform admin allowlist
// (PULSE_PLATFORM_ADMINS). Case-insensitive; empty allowlist means no admins.
func (s *Server) isPlatformAdmin(email string) bool {
	return s.platformAdmins[strings.ToLower(strings.TrimSpace(email))]
}

// orgMemberships loads the user's orgs and the role in each. It reads the role per
// org from the membership; a missing role is skipped (the org list and the role
// list are sourced together, so this is defensive).
func (s *Server) orgMemberships(ctx context.Context, userID int64) ([]apigen.OrgMembership, error) {
	orgs, err := s.store.ListOrganizationsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]apigen.OrgMembership, 0, len(orgs))
	for _, org := range orgs {
		m, err := s.store.GetMembership(ctx, userID, org.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return nil, err
		}
		out = append(out, orgMembershipDTO(org, m.Role))
	}
	return out, nil
}

// orgMembershipDTO maps an org + role to the API shape. The plan is the org's stored
// tier (operator-set until Stripe lands), normalized to a known plan.
func orgMembershipDTO(org *domain.Organization, role domain.Role) apigen.OrgMembership {
	return apigen.OrgMembership{
		OrgId: strconv.FormatInt(org.ID, 10),
		Name:  org.Name,
		Slug:  org.Slug,
		Role:  apigen.Role(role),
		Plan:  apigen.Plan(entitlements.ParsePlan(org.Plan)),
	}
}

// --- error envelope helpers (RFC-012 / RFC-014 localizable {code, message}) ---

func unauthorized(msg string) apigen.UnauthorizedJSONResponse {
	return apigen.UnauthorizedJSONResponse{Error: apigen.Error{Code: "unauthenticated", Message: msg}}
}

func forbidden(msg string) apigen.ForbiddenJSONResponse {
	return apigen.ForbiddenJSONResponse{Error: apigen.Error{Code: "forbidden", Message: msg}}
}

func notFound(msg string) apigen.NotFoundJSONResponse {
	return apigen.NotFoundJSONResponse{Error: apigen.Error{Code: "not_found", Message: msg}}
}

func conflict(msg string) apigen.ConflictJSONResponse {
	return apigen.ConflictJSONResponse{Error: apigen.Error{Code: "conflict", Message: msg}}
}

// conflictCode is conflict with a specific localizable code (e.g. monitor_disabled) so
// the frontend can tell apart the reasons a 409 was returned.
func conflictCode(code, msg string) apigen.ConflictJSONResponse {
	return apigen.ConflictJSONResponse{Error: apigen.Error{Code: code, Message: msg}}
}

// apiWriteForbidden is the 403 for an API key on a read-only plan attempting a write
// (PRD-006). It carries an upgrade hint so a client can surface the upsell.
func apiWriteForbidden() apigen.ForbiddenJSONResponse {
	fields := map[string]string{"upgrade": "api_write_not_allowed"}
	return apigen.ForbiddenJSONResponse{Error: apigen.Error{
		Code:    "api_write_not_allowed",
		Message: "your plan's API access is read-only; upgrade to trigger checks via the API",
		Fields:  &fields,
	}}
}

func validationFailed(msg string) apigen.ValidationFailedJSONResponse {
	return apigen.ValidationFailedJSONResponse{Error: apigen.Error{Code: "validation_failed", Message: msg}}
}

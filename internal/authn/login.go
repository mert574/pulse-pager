package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
	"pulse/internal/store"
)

// Login + callback orchestration (RFC-003 2.2 - 2.5). It builds the authorize
// redirect (PKCE/state/nonce stored short-term), handles the callback (validate
// state/nonce, exchange code, fetch verified profile), then resolves to a Pulse
// user: link by verified email, or create user + personal org + owner membership
// atomically (PRD-001 3.2). It does not write cookies itself; the caller takes the
// returned tokens and uses CookieConfig to set them.

const flowTTL = 10 * time.Minute // state/PKCE record lifetime (RFC-003 2.2)

// FlowKind separates a fresh login from a manual link flow (RFC-003 2.4). A link
// flow requires an active session at the callback.
type FlowKind string

const (
	FlowLogin FlowKind = "login"
	FlowLink  FlowKind = "link"
)

// flowState is the per-attempt record stored in Redis keyed by state (RFC-003 2.2).
type flowState struct {
	Provider domain.IdentityProvider `json:"provider"`
	Verifier string                  `json:"verifier"`
	Nonce    string                  `json:"nonce"`
	Flow     FlowKind                `json:"flow"`
	ReturnTo string                  `json:"return_to"`
	// LinkUserID is the signed-in user to attach the identity to, for a link flow.
	LinkUserID int64 `json:"link_user_id,omitempty"`
}

// flowStore is the Redis seam for the short-lived flow record. *kv.Client
// satisfies it. The record is single-use: the callback deletes it on consume.
type flowStore interface {
	GetCache(ctx context.Context, key string) (string, bool, error)
	SetCache(ctx context.Context, key, value string, ttl time.Duration) error
	DelCache(ctx context.Context, key string) error
}

// userStore is the subset of *store.Pool the user resolution needs.
type userStore interface {
	GetIdentity(ctx context.Context, provider domain.IdentityProvider, providerUserID string) (*domain.UserIdentity, error)
	GetUserByEmail(ctx context.Context, email string) (*domain.User, error)
	LinkIdentity(ctx context.Context, idn *domain.UserIdentity) (int64, error)
	CreateUserWithPersonalOrg(ctx context.Context, u *domain.User, idn *domain.UserIdentity, orgName, orgSlug string) (*store.FirstSignIn, error)
	SetLastLogin(ctx context.Context, userID int64) error
}

// LoginService orchestrates the social-login flow.
type LoginService struct {
	providers map[domain.IdentityProvider]Provider
	flows     flowStore
	users     userStore
	now       func() time.Time
}

// NewLoginService builds the service from the registered providers, the Redis flow
// store, and the user store.
func NewLoginService(providers []Provider, flows flowStore, users userStore) *LoginService {
	m := make(map[domain.IdentityProvider]Provider, len(providers))
	for _, p := range providers {
		m[p.Name()] = p
	}
	return &LoginService{providers: m, flows: flows, users: users, now: time.Now}
}

func flowKey(state string) string { return "oauth_flow:" + state }

// StartLogin makes the per-attempt state/nonce/verifier, stores the flow record in
// Redis, and returns the state (for the cross-check cookie) and the authorize
// redirect URL (RFC-003 2.5). returnTo is validated against the internal-path
// allowlist; an external or empty value falls back to "/". For a link flow pass
// linkUserID (the signed-in user) so the callback attaches to them.
func (s *LoginService) StartLogin(ctx context.Context, provider domain.IdentityProvider, flow FlowKind, returnTo string, linkUserID int64) (state, redirectURL string, err error) {
	p, ok := s.providers[provider]
	if !ok {
		return "", "", fmt.Errorf("unknown provider %q", provider)
	}
	state, err = newOpaqueToken()
	if err != nil {
		return "", "", err
	}
	nonce, err := newOpaqueToken()
	if err != nil {
		return "", "", err
	}
	verifier, err := newOpaqueToken()
	if err != nil {
		return "", "", err
	}
	rec := flowState{
		Provider:   provider,
		Verifier:   verifier,
		Nonce:      nonce,
		Flow:       flow,
		ReturnTo:   safeReturnTo(returnTo),
		LinkUserID: linkUserID,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return "", "", err
	}
	if err := s.flows.SetCache(ctx, flowKey(state), string(b), flowTTL); err != nil {
		return "", "", err
	}
	return state, p.AuthCodeURL(state, nonce, pkceChallenge(verifier)), nil
}

// CallbackResult is the outcome of a successful callback: the resolved user, whether
// it was a brand-new account (so the caller can route into onboarding), and the
// validated return_to.
type CallbackResult struct {
	UserID   int64
	Email    string
	IsNew    bool
	OrgID    int64 // the personal org for a new user, else 0
	ReturnTo string
}

// ErrStateMismatch is returned when the callback state is unknown/expired or the
// cookie state does not match the query state (RFC-003 2.2). The flow aborts with
// no session.
var ErrStateMismatch = errors.New("oauth state mismatch")

// ErrLinkNeedsSession is returned when a manual-link callback arrives with no
// signed-in user, so a forwarded callback cannot attach a stranger's provider
// (RFC-003 2.4).
var ErrLinkNeedsSession = errors.New("link flow requires an active session")

// HandleCallback validates the flow, exchanges the code for the verified profile,
// and resolves to a Pulse user (RFC-003 2.5). cookieState is the value from the
// pulse_oauth_state cookie, queryState/code are from the callback query. The state
// record is single-use: it is deleted on consume regardless of outcome.
func (s *LoginService) HandleCallback(ctx context.Context, cookieState, queryState, code string) (*CallbackResult, error) {
	if queryState == "" || cookieState != queryState {
		return nil, ErrStateMismatch
	}
	raw, ok, err := s.flows.GetCache(ctx, flowKey(queryState))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrStateMismatch
	}
	// Single use: consume the record now.
	_ = s.flows.DelCache(ctx, flowKey(queryState))

	var rec flowState
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, ErrStateMismatch
	}
	p, ok := s.providers[rec.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", rec.Provider)
	}

	profile, err := p.Exchange(ctx, code, rec.Verifier, rec.Nonce)
	if err != nil {
		return nil, err // includes ErrEmailNotVerified and nonce mismatch
	}

	if rec.Flow == FlowLink {
		return s.resolveLink(ctx, rec.LinkUserID, profile, rec.ReturnTo)
	}
	return s.resolveLogin(ctx, profile, rec.ReturnTo)
}

// resolveLogin matches the profile to a user: by identity (returning user), then by
// verified email (auto-link), else creates a new user + personal org (RFC-003 2.3,
// 2.4). It stamps last_login on success.
func (s *LoginService) resolveLogin(ctx context.Context, profile *Profile, returnTo string) (*CallbackResult, error) {
	// 1. Returning user: identity already linked.
	idn, err := s.users.GetIdentity(ctx, profile.Provider, profile.ProviderUserID)
	if err == nil {
		_ = s.users.SetLastLogin(ctx, idn.UserID)
		return &CallbackResult{UserID: idn.UserID, Email: profile.Email, ReturnTo: returnTo}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// 2. Auto-link on verified-email match: an existing user with this verified email.
	existing, err := s.users.GetUserByEmail(ctx, profile.Email)
	if err == nil {
		newIdn := &domain.UserIdentity{
			UserID:         existing.ID,
			Provider:       profile.Provider,
			ProviderUserID: profile.ProviderUserID,
		}
		if _, err := s.users.LinkIdentity(ctx, newIdn); err != nil {
			return nil, fmt.Errorf("auto-link identity: %w", err)
		}
		_ = s.users.SetLastLogin(ctx, existing.ID)
		return &CallbackResult{UserID: existing.ID, Email: profile.Email, ReturnTo: returnTo}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// 3. New user: create user + personal org + owner membership atomically.
	u := &domain.User{
		Email:         profile.Email,
		EmailVerified: true,
		Name:          profile.Name,
		AvatarURL:     profile.AvatarURL,
	}
	idnNew := &domain.UserIdentity{Provider: profile.Provider, ProviderUserID: profile.ProviderUserID}
	name, slug := personalOrgNaming(profile)
	res, err := s.users.CreateUserWithPersonalOrg(ctx, u, idnNew, name, slug)
	if err != nil {
		return nil, fmt.Errorf("first sign-in: %w", err)
	}
	return &CallbackResult{UserID: res.UserID, Email: profile.Email, IsNew: true, OrgID: res.OrgID, ReturnTo: returnTo}, nil
}

// resolveLink attaches the provider identity to the already-signed-in user (RFC-003
// 2.4 manual link). It refuses if there is no session, and the unique index on
// (provider, provider_user_id) refuses linking an account already owned by another
// user (I5), surfaced as the LinkIdentity error.
func (s *LoginService) resolveLink(ctx context.Context, linkUserID int64, profile *Profile, returnTo string) (*CallbackResult, error) {
	if linkUserID == 0 {
		return nil, ErrLinkNeedsSession
	}
	// If this provider account is already linked, only allow if it is the same user.
	if existing, err := s.users.GetIdentity(ctx, profile.Provider, profile.ProviderUserID); err == nil {
		if existing.UserID != linkUserID {
			return nil, errors.New("this provider account is already linked to another user")
		}
		return &CallbackResult{UserID: linkUserID, Email: profile.Email, ReturnTo: returnTo}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err := s.users.LinkIdentity(ctx, &domain.UserIdentity{
		UserID:         linkUserID,
		Provider:       profile.Provider,
		ProviderUserID: profile.ProviderUserID,
	}); err != nil {
		return nil, fmt.Errorf("link identity: %w", err)
	}
	return &CallbackResult{UserID: linkUserID, Email: profile.Email, ReturnTo: returnTo}, nil
}

// personalOrgNaming derives the personal org name and a slug from the profile
// (PRD-001 3.2: "Dev's workspace"). The slug adds a short random suffix so it is
// unique even when two people share a name; the caller may retry on a slug clash.
func personalOrgNaming(profile *Profile) (name, slug string) {
	base := profile.Name
	if base == "" {
		base = strings.Split(profile.Email, "@")[0]
	}
	name = base + "'s workspace"
	suffix, _ := newOpaqueToken()
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	slug = slugify(base) + "-" + strings.ToLower(suffix)
	return name, slug
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "workspace"
	}
	return out
}

// safeReturnTo validates a return_to against the internal-path allowlist: it must be
// a single-slash-prefixed relative path, never an external origin or a //host (open
// redirect mitigation, RFC-003 2.2 / section 10). Anything else falls back to "/".
func safeReturnTo(returnTo string) string {
	if returnTo == "" || returnTo[0] != '/' {
		return "/"
	}
	if strings.HasPrefix(returnTo, "//") || strings.HasPrefix(returnTo, "/\\") {
		return "/"
	}
	return returnTo
}

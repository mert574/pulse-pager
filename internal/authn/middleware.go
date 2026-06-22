package authn

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/authz"
	"pulse/internal/domain"
)

// Request -> principal resolution (RFC-003 section 6). The Identify middleware
// accepts either the JWT access cookie/bearer or an API key, yielding the
// authenticated principal. For a human it then resolves the active org from the URL
// /orgs/{orgId} (validated against membership) and loads the role (Redis-cached). An
// API-key request fixes the org and role from the key and skips org/role resolution.

// Principal is the resolved request context authz reads: who, in which org, with
// what role (RFC-003 6.1). It maps directly onto authz.Actor.
type Principal struct {
	Kind   authz.ActorKind
	UserID int64 // human
	KeyID  int64 // api key
	Email  string
	OrgID  int64
	Role   domain.Role
}

// Actor builds the authz.Actor for the role gate from the principal.
func (p Principal) Actor() authz.Actor {
	return authz.Actor{
		Kind:   p.Kind,
		UserID: p.UserID,
		KeyID:  p.KeyID,
		OrgID:  p.OrgID,
		Role:   p.Role,
	}
}

type principalKey struct{}

// FromContext returns the principal set by the middleware, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// WithPrincipal returns a context carrying the principal (exported for tests and
// handlers that build a context directly).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// membershipStore is the subset of *store.Pool the org/role resolution needs.
type membershipStore interface {
	GetMembership(ctx context.Context, userID, orgID int64) (*domain.Membership, error)
}

// roleCache is the subset of *kv.Client used to cache the membership role
// (RFC-003 6.3). May be nil to skip caching.
type roleCache interface {
	GetCache(ctx context.Context, key string) (string, bool, error)
	SetCache(ctx context.Context, key, value string, ttl time.Duration) error
	DelCache(ctx context.Context, key string) error
}

const memberRoleTTL = 5 * time.Minute // RFC-003 6.3

// Authenticator wires the JWT verify, the API-key verify, the membership/role
// lookup, and the error-envelope writer into the request middleware.
type Authenticator struct {
	jwt      *JWTIssuer
	keys     *APIKeyVerifier
	members  membershipStore
	roles    roleCache
	writeErr func(w http.ResponseWriter, status int, code, msg string)
}

// AuthOption configures an Authenticator.
type AuthOption func(*Authenticator)

// WithErrorWriter overrides the 401/403 envelope writer (the default writes a small
// JSON body matching the dev/api envelope shape).
func WithErrorWriter(fn func(w http.ResponseWriter, status int, code, msg string)) AuthOption {
	return func(a *Authenticator) { a.writeErr = fn }
}

// NewAuthenticator builds the request authenticator. roles may be nil to skip the
// membership-role cache (every request then reads the role from Postgres).
func NewAuthenticator(j *JWTIssuer, keys *APIKeyVerifier, members membershipStore, roles roleCache, opts ...AuthOption) *Authenticator {
	a := &Authenticator{
		jwt:      j,
		keys:     keys,
		members:  members,
		roles:    roles,
		writeErr: defaultWriteErr,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Identify is step 1 of the chain (RFC-003 6.1): identify the principal from an API
// key or the access JWT, with no org/role yet. For an API key it resolves the full
// principal (org + role from the key). For a JWT it sets the user only; the org and
// role are resolved later by RequireOrg. On no or invalid credentials it writes 401.
func (a *Authenticator) Identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API key first: Authorization: Bearer pulse_sk_...
		if rawKey, ok := bearerAPIKey(r); ok {
			kp, err := a.keys.Verify(r.Context(), rawKey)
			if err != nil {
				a.writeErr(w, http.StatusUnauthorized, "unauthenticated", "invalid api key")
				return
			}
			p := Principal{
				Kind:  authz.ActorAPIKey,
				KeyID: kp.KeyID,
				OrgID: kp.OrgID,
				Role:  kp.Role,
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
			return
		}

		// JWT: access cookie or Authorization: Bearer <jwt>.
		raw, ok := accessJWT(r)
		if !ok {
			a.writeErr(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		vt, err := a.jwt.Verify(raw)
		if err != nil {
			a.writeErr(w, http.StatusUnauthorized, "unauthenticated", "invalid token")
			return
		}
		p := Principal{
			Kind:   authz.ActorHuman,
			UserID: vt.UserID,
			Email:  vt.Email,
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// RequireOrg is steps 2 and 3 of the chain (RFC-003 6.2/6.3): for a human it reads
// the active org from the URL path value orgId, checks it against membership, and
// loads the role (Redis-cached). For an API key the org and role are already fixed
// by the key, so it passes through unchanged. A non-member is 403 (not 404, so the
// org's existence is not leaked). It must run after Identify.
//
// The path value is read with r.PathValue("orgId"), so a route registered as
// /orgs/{orgId}/... on the stdlib mux supplies it.
func (a *Authenticator) RequireOrg(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok {
			a.writeErr(w, http.StatusUnauthorized, "unauthenticated", "no principal")
			return
		}
		// API key: org is fixed by the key, do not read any org parameter.
		if p.Kind == authz.ActorAPIKey {
			next.ServeHTTP(w, r)
			return
		}

		orgID, err := orgIDFromRequest(r)
		if err != nil {
			a.writeErr(w, http.StatusBadRequest, "invalid_org", "missing or invalid org id")
			return
		}
		role, err := a.resolveRole(r.Context(), p.UserID, orgID)
		if err != nil {
			if errors.Is(err, errNotMember) {
				a.writeErr(w, http.StatusForbidden, "forbidden", "not a member of this org")
				return
			}
			a.writeErr(w, http.StatusInternalServerError, "internal", "could not resolve membership")
			return
		}
		p.OrgID = orgID
		p.Role = role
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

var errNotMember = errors.New("not a member")

// resolveRole loads the (user, org) role, Redis-cached with invalidation on change
// (RFC-003 6.3). A cache miss reads Postgres and caches the result; a non-membership
// returns errNotMember.
func (a *Authenticator) resolveRole(ctx context.Context, userID, orgID int64) (domain.Role, error) {
	ck := memberRoleCacheKey(userID, orgID)
	if a.roles != nil {
		if v, ok, err := a.roles.GetCache(ctx, ck); err == nil && ok {
			if v == "" {
				return "", errNotMember // cached negative
			}
			return domain.Role(v), nil
		}
	}
	m, err := a.members.GetMembership(ctx, userID, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if a.roles != nil {
				_ = a.roles.SetCache(ctx, ck, "", memberRoleTTL) // negative cache
			}
			return "", errNotMember
		}
		return "", err
	}
	if a.roles != nil {
		_ = a.roles.SetCache(ctx, ck, string(m.Role), memberRoleTTL)
	}
	return m.Role, nil
}

// InvalidateMemberRole busts the cached role for (user, org) on a role change,
// removal, or new membership so the next request re-reads it (RFC-003 6.3). The
// handler that mutates a membership calls this.
func (a *Authenticator) InvalidateMemberRole(ctx context.Context, userID, orgID int64) error {
	if a.roles == nil {
		return nil
	}
	return a.roles.DelCache(ctx, memberRoleCacheKey(userID, orgID))
}

func memberRoleCacheKey(userID, orgID int64) string {
	return "member:" + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(orgID, 10)
}

// --- request parsing helpers ---

// bearerAPIKey returns the raw key if the Authorization header is a Bearer carrying
// a pulse_sk_ value, so an API key and a JWT bearer are told apart by the prefix.
func bearerAPIKey(r *http.Request) (string, bool) {
	tok, ok := bearerToken(r)
	if !ok {
		return "", false
	}
	if strings.HasPrefix(tok, APIKeyPrefix) {
		return tok, true
	}
	return "", false
}

// accessJWT returns the access token from the cookie or a non-key bearer header.
func accessJWT(r *http.Request) (string, bool) {
	if c, err := r.Cookie(AccessCookie); err == nil && c.Value != "" {
		return c.Value, true
	}
	if tok, ok := bearerToken(r); ok && !strings.HasPrefix(tok, APIKeyPrefix) {
		return tok, true
	}
	return "", false
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):]), true
	}
	return "", false
}

// orgIDFromRequest reads the active org from the URL path value orgId (the SPA
// /orgs/{orgId} mechanism, RFC-003 6.2) or the X-Pulse-Org header alternate.
func orgIDFromRequest(r *http.Request) (int64, error) {
	if v := r.PathValue("orgId"); v != "" {
		return strconv.ParseInt(v, 10, 64)
	}
	if v := r.Header.Get("X-Pulse-Org"); v != "" {
		return strconv.ParseInt(v, 10, 64)
	}
	return 0, errors.New("no org id in request")
}

func defaultWriteErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// minimal envelope matching the dev/api shape: {"error":{"code":..,"message":..}}
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"` + msg + `"}}`))
}

package authn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"pulse/internal/authz"
	"pulse/internal/domain"
)

type fakeMemberStore struct {
	// keyed by userID*1000+orgID for the test
	members map[int64]domain.Role
}

func (f *fakeMemberStore) GetMembership(_ context.Context, userID, orgID int64) (*domain.Membership, error) {
	role, ok := f.members[userID*1000+orgID]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return &domain.Membership{UserID: userID, OrgID: orgID, Role: role}, nil
}

// buildAuth wires an Authenticator with a real JWT issuer + api-key verifier and
// fake member/key stores, for middleware tests.
func buildAuth(t *testing.T) (*Authenticator, *JWTIssuer, *fakeAPIKeyStore) {
	t.Helper()
	iss := newTestIssuer(t)
	keyStore := &fakeAPIKeyStore{byHash: map[string]*domain.APIKey{}}
	keys := NewAPIKeyVerifier(keyStore, newFakeCache())
	members := &fakeMemberStore{members: map[int64]domain.Role{
		// user 42 is an admin of org 7
		42*1000 + 7: domain.RoleAdmin,
	}}
	a := NewAuthenticator(iss, keys, members, newFakeCache())
	return a, iss, keyStore
}

// terminal handler that records the principal it saw.
func captureHandler(seen *Principal) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := FromContext(r.Context())
		*seen = p
		w.WriteHeader(http.StatusOK)
	})
}

func TestMiddlewareNoCredentials401(t *testing.T) {
	a, _, _ := buildAuth(t)
	var seen Principal
	h := a.Identify(captureHandler(&seen))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/monitors", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddlewareInvalidJWT401(t *testing.T) {
	a, _, _ := buildAuth(t)
	h := a.Identify(captureHandler(&Principal{}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: AccessCookie, Value: "garbage.jwt.value"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddlewareValidJWTAndOrgMember(t *testing.T) {
	a, iss, _ := buildAuth(t)
	tok, _, _ := iss.Issue(42, "admin@example.com")

	var seen Principal
	// chain Identify -> RequireOrg -> terminal, with the org in the path value
	mux := http.NewServeMux()
	mux.Handle("GET /orgs/{orgId}/monitors", a.Identify(a.RequireOrg(captureHandler(&seen))))

	req := httptest.NewRequest(http.MethodGet, "/orgs/7/monitors", nil)
	req.AddCookie(&http.Cookie{Name: AccessCookie, Value: tok})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if seen.UserID != 42 || seen.OrgID != 7 || seen.Role != domain.RoleAdmin || seen.Kind != authz.ActorHuman {
		t.Fatalf("unexpected principal: %+v", seen)
	}
}

func TestMiddlewareOrgNotAMember403(t *testing.T) {
	a, iss, _ := buildAuth(t)
	tok, _, _ := iss.Issue(42, "admin@example.com")

	mux := http.NewServeMux()
	mux.Handle("GET /orgs/{orgId}/monitors", a.Identify(a.RequireOrg(captureHandler(&Principal{}))))

	// user 42 is not a member of org 999
	req := httptest.NewRequest(http.MethodGet, "/orgs/999/monitors", nil)
	req.AddCookie(&http.Cookie{Name: AccessCookie, Value: tok})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-member, got %d", rec.Code)
	}
}

func TestMiddlewareAPIKeyFixesOrg(t *testing.T) {
	a, _, keyStore := buildAuth(t)
	raw := APIKeyPrefix + "middlewarekey000000000"
	mkKey(keyStore, raw, 88, domain.RoleMember, false)

	var seen Principal
	mux := http.NewServeMux()
	// the api-key path skips org resolution, so RequireOrg passes through
	mux.Handle("GET /v1/monitors", a.Identify(a.RequireOrg(captureHandler(&seen))))

	req := httptest.NewRequest(http.MethodGet, "/v1/monitors", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for api key, got %d", rec.Code)
	}
	if seen.Kind != authz.ActorAPIKey || seen.OrgID != 88 || seen.Role != domain.RoleMember {
		t.Fatalf("api key principal wrong: %+v", seen)
	}
}

func TestMiddlewareInvalidAPIKey401(t *testing.T) {
	a, _, _ := buildAuth(t)
	h := a.Identify(captureHandler(&Principal{}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+APIKeyPrefix+"nopexxxxxxxxxxxxxxxxxx")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad api key, got %d", rec.Code)
	}
}

func TestMemberRoleCacheInvalidation(t *testing.T) {
	a, _, _ := buildAuth(t)
	ctx := context.Background()
	// first resolve caches the role
	role, err := a.resolveRole(ctx, 42, 7)
	if err != nil || role != domain.RoleAdmin {
		t.Fatalf("resolve: role=%s err=%v", role, err)
	}
	// invalidation removes the cache entry; the next resolve re-reads the store
	if err := a.InvalidateMemberRole(ctx, 42, 7); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := a.resolveRole(ctx, 42, 7); err != nil {
		t.Fatalf("re-resolve after bust: %v", err)
	}
}

package authn

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/crypto"
	"pulse/internal/domain"
)

type fakeAPIKeyStore struct {
	byHash  map[string]*domain.APIKey
	touched int
}

func (f *fakeAPIKeyStore) GetAPIKeyByHash(_ context.Context, h string) (*domain.APIKey, error) {
	k, ok := f.byHash[h]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *k
	return &cp, nil
}

func (f *fakeAPIKeyStore) TouchAPIKey(_ context.Context, _ int64) error {
	f.touched++
	return nil
}

// fakeCache is an in-memory keyCache that counts hits and can simulate an error.
type fakeCache struct {
	data map[string]string
	hits int
}

func newFakeCache() *fakeCache { return &fakeCache{data: map[string]string{}} }

func (c *fakeCache) GetCache(_ context.Context, key string) (string, bool, error) {
	v, ok := c.data[key]
	if ok {
		c.hits++
	}
	return v, ok, nil
}
func (c *fakeCache) SetCache(_ context.Context, key, value string, _ time.Duration) error {
	c.data[key] = value
	return nil
}
func (c *fakeCache) DelCache(_ context.Context, key string) error {
	delete(c.data, key)
	return nil
}

func mkKey(store *fakeAPIKeyStore, raw string, org int64, role domain.Role, revoked bool) {
	k := &domain.APIKey{ID: 1, OrgID: org, Role: role, TokenHash: crypto.HashToken(raw)}
	if revoked {
		now := time.Now()
		k.RevokedAt = &now
	}
	store.byHash[k.TokenHash] = k
}

func TestAPIKeyVerifyValid(t *testing.T) {
	ctx := context.Background()
	st := &fakeAPIKeyStore{byHash: map[string]*domain.APIKey{}}
	cache := newFakeCache()
	raw := APIKeyPrefix + "abc123def456ghi789jkl0"
	mkKey(st, raw, 55, domain.RoleAdmin, false)

	v := NewAPIKeyVerifier(st, cache)
	p, err := v.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.OrgID != 55 || p.Role != domain.RoleAdmin {
		t.Fatalf("unexpected principal: %+v", p)
	}

	// second verify should be a cache hit (no second store read needed)
	st.byHash = map[string]*domain.APIKey{} // remove from DB to prove cache is used
	p2, err := v.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("cached verify: %v", err)
	}
	if p2.OrgID != 55 {
		t.Fatalf("cache miss path returned wrong org: %+v", p2)
	}
	if cache.hits == 0 {
		t.Fatal("expected a cache hit on the second verify")
	}
}

func TestAPIKeyVerifyRevoked(t *testing.T) {
	ctx := context.Background()
	st := &fakeAPIKeyStore{byHash: map[string]*domain.APIKey{}}
	raw := APIKeyPrefix + "revoked000000000000000"
	mkKey(st, raw, 9, domain.RoleMember, true)

	v := NewAPIKeyVerifier(st, newFakeCache())
	if _, err := v.Verify(ctx, raw); err != ErrAPIKeyInvalid {
		t.Fatalf("revoked key should be invalid, got %v", err)
	}
}

func TestAPIKeyVerifyUnknownAndMalformed(t *testing.T) {
	ctx := context.Background()
	st := &fakeAPIKeyStore{byHash: map[string]*domain.APIKey{}}
	v := NewAPIKeyVerifier(st, newFakeCache())

	if _, err := v.Verify(ctx, APIKeyPrefix+"doesnotexist000000000"); err != ErrAPIKeyInvalid {
		t.Fatalf("unknown key should be invalid, got %v", err)
	}
	if _, err := v.Verify(ctx, "not-a-pulse-key"); err != ErrAPIKeyInvalid {
		t.Fatalf("malformed key should be invalid, got %v", err)
	}
}

func TestAPIKeyVerifyNoCacheFallsBackToDB(t *testing.T) {
	ctx := context.Background()
	st := &fakeAPIKeyStore{byHash: map[string]*domain.APIKey{}}
	raw := APIKeyPrefix + "nocache0000000000000000"
	mkKey(st, raw, 3, domain.RoleMember, false)

	// nil cache: verify must still work straight from the DB (fail closed, not open)
	v := NewAPIKeyVerifier(st, nil)
	p, err := v.Verify(ctx, raw)
	if err != nil || p.OrgID != 3 {
		t.Fatalf("nil-cache verify: p=%+v err=%v", p, err)
	}
}

func TestAPIKeyInvalidateBustsCache(t *testing.T) {
	ctx := context.Background()
	st := &fakeAPIKeyStore{byHash: map[string]*domain.APIKey{}}
	cache := newFakeCache()
	raw := APIKeyPrefix + "bustme00000000000000000"
	mkKey(st, raw, 1, domain.RoleAdmin, false)

	v := NewAPIKeyVerifier(st, cache)
	if _, err := v.Verify(ctx, raw); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// now revoke in DB and bust the cache; the next verify must see revoked
	now := time.Now()
	st.byHash[crypto.HashToken(raw)].RevokedAt = &now
	if err := v.InvalidateAPIKey(ctx, crypto.HashToken(raw)); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := v.Verify(ctx, raw); err != ErrAPIKeyInvalid {
		t.Fatalf("after bust the revoked key should fail, got %v", err)
	}
}

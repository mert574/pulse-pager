package authn

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/crypto"
	"pulse/internal/domain"
)

// fakeRefreshStore is an in-memory stand-in for the refresh-token store methods,
// matching the rotation/reuse semantics of the real Postgres methods.
type fakeRefreshStore struct {
	byHash map[string]*domain.RefreshToken
	nextID int64
}

func newFakeRefreshStore() *fakeRefreshStore {
	return &fakeRefreshStore{byHash: map[string]*domain.RefreshToken{}}
}

func (f *fakeRefreshStore) CreateRefreshToken(_ context.Context, rt *domain.RefreshToken) (int64, error) {
	f.nextID++
	rt.ID = f.nextID
	if rt.FamilyID == 0 {
		rt.FamilyID = rt.ID
	}
	f.byHash[rt.TokenHash] = rt
	return rt.ID, nil
}

func (f *fakeRefreshStore) GetRefreshTokenByHash(_ context.Context, h string) (*domain.RefreshToken, error) {
	rt, ok := f.byHash[h]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *rt
	return &cp, nil
}

func (f *fakeRefreshStore) RotateRefreshToken(_ context.Context, oldHash, newHash string) (int64, error) {
	old, ok := f.byHash[oldHash]
	if !ok || old.ReplacedBy != nil || old.RevokedAt != nil {
		return 0, pgx.ErrNoRows
	}
	f.nextID++
	newID := f.nextID
	f.byHash[newHash] = &domain.RefreshToken{
		ID: newID, UserID: old.UserID, FamilyID: old.FamilyID,
		TokenHash: newHash, ExpiresAt: old.ExpiresAt,
	}
	old.ReplacedBy = &newID
	return newID, nil
}

func (f *fakeRefreshStore) RevokeRefreshTokenFamily(_ context.Context, familyID int64) (int64, error) {
	var n int64
	now := time.Now()
	for _, rt := range f.byHash {
		if rt.FamilyID == familyID && rt.RevokedAt == nil {
			rt.RevokedAt = &now
			n++
		}
	}
	return n, nil
}

func (f *fakeRefreshStore) RevokeAllForUser(_ context.Context, userID int64) (int64, error) {
	var n int64
	now := time.Now()
	for _, rt := range f.byHash {
		if rt.UserID == userID && rt.RevokedAt == nil {
			rt.RevokedAt = &now
			n++
		}
	}
	return n, nil
}

func TestRefreshRotation(t *testing.T) {
	ctx := context.Background()
	fs := newFakeRefreshStore()
	svc := NewRefreshService(fs)

	issued, err := svc.Issue(ctx, 100)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	rot, err := svc.Rotate(ctx, issued.Raw)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rot.UserID != 100 {
		t.Fatalf("rotated token user mismatch: %d", rot.UserID)
	}
	if rot.FamilyID != issued.FamilyID {
		t.Fatalf("rotated token should stay in the same family: %d != %d", rot.FamilyID, issued.FamilyID)
	}
	// the new raw token verifies in the store, the old one is now rotated
	if _, err := fs.GetRefreshTokenByHash(ctx, crypto.HashToken(rot.Raw)); err != nil {
		t.Fatalf("new token should exist: %v", err)
	}
}

func TestRefreshReuseRevokesFamily(t *testing.T) {
	ctx := context.Background()
	fs := newFakeRefreshStore()
	svc := NewRefreshService(fs)

	issued, _ := svc.Issue(ctx, 200)
	// first rotation succeeds
	if _, err := svc.Rotate(ctx, issued.Raw); err != nil {
		t.Fatalf("first rotate: %v", err)
	}
	// presenting the already-rotated old token is reuse: revokes the whole family
	if _, err := svc.Rotate(ctx, issued.Raw); err != ErrReuseDetected {
		t.Fatalf("reused token should report ErrReuseDetected, got %v", err)
	}
	// the family is now revoked: the live (rotated) token is dead too
	for _, rt := range fs.byHash {
		if rt.FamilyID == issued.FamilyID && rt.RevokedAt == nil {
			t.Fatal("the whole family should be revoked after reuse")
		}
	}
}

func TestRefreshRevokeFamilyOnLogout(t *testing.T) {
	ctx := context.Background()
	fs := newFakeRefreshStore()
	svc := NewRefreshService(fs)

	issued, _ := svc.Issue(ctx, 300)
	if err := svc.RevokeFamily(ctx, issued.Raw); err != nil {
		t.Fatalf("revoke family: %v", err)
	}
	// rotating a revoked token is invalid
	if _, err := svc.Rotate(ctx, issued.Raw); err != ErrRefreshInvalid {
		t.Fatalf("rotating a revoked token should be invalid, got %v", err)
	}
	// unknown token logout is a no-op success
	if err := svc.RevokeFamily(ctx, "never-issued"); err != nil {
		t.Fatalf("logout of unknown token should be a no-op: %v", err)
	}
}

func TestRefreshRevokeAll(t *testing.T) {
	ctx := context.Background()
	fs := newFakeRefreshStore()
	svc := NewRefreshService(fs)

	a, _ := svc.Issue(ctx, 400)
	b, _ := svc.Issue(ctx, 400) // a second device, second family
	if err := svc.RevokeAll(ctx, 400); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if _, err := svc.Rotate(ctx, a.Raw); err != ErrRefreshInvalid {
		t.Fatalf("device a should be revoked, got %v", err)
	}
	if _, err := svc.Rotate(ctx, b.Raw); err != ErrRefreshInvalid {
		t.Fatalf("device b should be revoked, got %v", err)
	}
}

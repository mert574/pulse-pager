package authn

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/crypto"
	"pulse/internal/domain"
)

const refreshTTL = 30 * 24 * time.Hour // ~30 days (RFC-003 4.1)

// ErrReuseDetected is returned when an already-rotated refresh token is presented
// again (the classic theft signal). The caller revokes the whole family and forces
// re-login (RFC-003 4.1).
var ErrReuseDetected = errors.New("refresh token reuse detected; family revoked")

// ErrRefreshInvalid is returned when a refresh token is unknown, expired, or
// revoked. The caller clears the cookies and the user re-logs-in.
var ErrRefreshInvalid = errors.New("refresh token invalid")

// refreshStore is the subset of *store.Pool the refresh logic needs. Defining it as
// an interface keeps the service testable without a live DB for the rotation rules,
// though the integration test exercises the real pool.
type refreshStore interface {
	CreateRefreshToken(ctx context.Context, rt *domain.RefreshToken) (int64, error)
	GetRefreshTokenByHash(ctx context.Context, tokenHash string) (*domain.RefreshToken, error)
	RotateRefreshToken(ctx context.Context, oldHash, newHash string) (int64, error)
	RevokeRefreshTokenFamily(ctx context.Context, familyID int64) (int64, error)
	RevokeAllForUser(ctx context.Context, userID int64) (int64, error)
}

// RefreshService issues, rotates, reuse-detects, and revokes opaque refresh tokens
// (RFC-003 4). The client holds the raw token; the DB stores only its SHA-256 hash.
type RefreshService struct {
	store refreshStore
	now   func() time.Time
}

// NewRefreshService builds the service over a store.
func NewRefreshService(s refreshStore) *RefreshService {
	return &RefreshService{store: s, now: time.Now}
}

// IssuedRefresh is a freshly minted refresh token: the raw secret to hand the
// client and the family it belongs to.
type IssuedRefresh struct {
	Raw      string
	FamilyID int64
	Expires  time.Time
}

// Issue mints a new refresh token rooting a new family, used on a fresh login
// (RFC-003 4.1). The raw token goes in the cookie; only its hash is stored.
func (r *RefreshService) Issue(ctx context.Context, userID int64) (*IssuedRefresh, error) {
	raw, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}
	exp := r.now().Add(refreshTTL)
	rt := &domain.RefreshToken{
		UserID:    userID,
		TokenHash: crypto.HashToken(raw),
		ExpiresAt: exp,
	}
	if _, err := r.store.CreateRefreshToken(ctx, rt); err != nil {
		return nil, err
	}
	return &IssuedRefresh{Raw: raw, FamilyID: rt.FamilyID, Expires: exp}, nil
}

// Rotated is the result of a successful rotation: the new raw refresh token and the
// owning user, so the caller can mint a fresh access token alongside it.
type Rotated struct {
	Raw      string
	UserID   int64
	FamilyID int64
	Expires  time.Time
}

// Rotate consumes a presented refresh token and issues a new one in the same family
// (RFC-003 4.1). It detects reuse: if the presented token was already rotated (or is
// revoked/unknown), it does not issue, it reports the reuse so the caller revokes the
// whole family. The store's RotateRefreshToken only rotates a token with replaced_by
// IS NULL and revoked_at IS NULL, so a replayed already-rotated token rotates nothing.
func (r *RefreshService) Rotate(ctx context.Context, presentedRaw string) (*Rotated, error) {
	oldHash := crypto.HashToken(presentedRaw)

	existing, err := r.store.GetRefreshTokenByHash(ctx, oldHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRefreshInvalid // unknown token
		}
		return nil, err
	}

	// Already rotated -> reuse. Revoke the whole family and refuse.
	if existing.ReplacedBy != nil {
		_, _ = r.store.RevokeRefreshTokenFamily(ctx, existing.FamilyID)
		return nil, ErrReuseDetected
	}
	// Revoked or expired -> invalid, no new token.
	if existing.RevokedAt != nil || !existing.ExpiresAt.After(r.now()) {
		return nil, ErrRefreshInvalid
	}

	newRaw, err := newOpaqueToken()
	if err != nil {
		return nil, err
	}
	newHash := crypto.HashToken(newRaw)
	if _, err := r.store.RotateRefreshToken(ctx, oldHash, newHash); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A concurrent rotation already consumed it: treat as reuse.
			_, _ = r.store.RevokeRefreshTokenFamily(ctx, existing.FamilyID)
			return nil, ErrReuseDetected
		}
		return nil, err
	}
	return &Rotated{
		Raw:      newRaw,
		UserID:   existing.UserID,
		FamilyID: existing.FamilyID,
		Expires:  existing.ExpiresAt,
	}, nil
}

// RevokeFamily revokes the family a presented token belongs to, the sign-out
// (this device) path (RFC-003 4.3). Unknown token is a no-op success: logging out
// an already-dead session is fine.
func (r *RefreshService) RevokeFamily(ctx context.Context, presentedRaw string) error {
	existing, err := r.store.GetRefreshTokenByHash(ctx, crypto.HashToken(presentedRaw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	_, err = r.store.RevokeRefreshTokenFamily(ctx, existing.FamilyID)
	return err
}

// RevokeAll revokes every refresh family for a user, the "log out of all devices"
// path (RFC-003 4.3). It also runs on account-deletion request.
func (r *RefreshService) RevokeAll(ctx context.Context, userID int64) error {
	_, err := r.store.RevokeAllForUser(ctx, userID)
	return err
}

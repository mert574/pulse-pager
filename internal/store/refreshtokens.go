package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// refresh_tokens is a global table keyed by user_id (RFC-001 5.2), not org. It is
// reached only through the auth path, which scopes by the authenticated user, so
// these methods use the pool directly (no org RLS). Tokens are stored hashed; the
// caller passes the SHA-256 of the opaque token. Access tokens are stateless JWTs
// and are never stored.

const refreshColumns = `id, user_id, family_id, replaced_by, token_hash, created_at, expires_at, revoked_at`

func scanRefresh(row pgx.Row) (*domain.RefreshToken, error) {
	var rt domain.RefreshToken
	err := row.Scan(&rt.ID, &rt.UserID, &rt.FamilyID, &rt.ReplacedBy, &rt.TokenHash,
		&rt.CreatedAt, &rt.ExpiresAt, &rt.RevokedAt)
	if err != nil {
		return nil, err
	}
	return &rt, nil
}

// CreateRefreshToken inserts a token. If FamilyID is 0 this is the first token of
// a new login chain, so the family id is set to the new row's own id (one self-
// rooted family per login); pass a non-zero FamilyID to add to an existing family.
func (p *Pool) CreateRefreshToken(ctx context.Context, rt *domain.RefreshToken) (int64, error) {
	var id int64
	if rt.FamilyID == 0 {
		// Insert with a placeholder family, then set family_id = id so a fresh
		// login chain is rooted at its own first token.
		err := p.QueryRow(ctx, `
			INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
			VALUES ($1, 0, $2, $3)
			RETURNING id`,
			rt.UserID, rt.TokenHash, rt.ExpiresAt,
		).Scan(&id)
		if err != nil {
			return 0, err
		}
		if _, err := p.Exec(ctx, `UPDATE refresh_tokens SET family_id = $1 WHERE id = $1`, id); err != nil {
			return 0, err
		}
		rt.FamilyID = id
	} else {
		err := p.QueryRow(ctx, `
			INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
			VALUES ($1,$2,$3,$4)
			RETURNING id`,
			rt.UserID, rt.FamilyID, rt.TokenHash, rt.ExpiresAt,
		).Scan(&id)
		if err != nil {
			return 0, err
		}
	}
	rt.ID = id
	return id, nil
}

// GetRefreshTokenByHash loads a token by its hash, or pgx.ErrNoRows if unknown.
// The caller checks expires_at/revoked_at/replaced_by to decide validity and
// reuse (a non-null replaced_by means it was already rotated, RFC-003 4.1).
func (p *Pool) GetRefreshTokenByHash(ctx context.Context, tokenHash string) (*domain.RefreshToken, error) {
	return scanRefresh(p.QueryRow(ctx, `SELECT `+refreshColumns+` FROM refresh_tokens WHERE token_hash = $1`, tokenHash))
}

// RotateRefreshToken rotates a token on refresh: it sets the old token's
// replaced_by to a freshly inserted token in the same family, in one transaction
// (RFC-003 4.1). It only rotates a token that has not already been rotated
// (replaced_by IS NULL) and is not revoked, so a replayed already-rotated token
// rotates nothing and returns pgx.ErrNoRows, which the caller treats as the reuse
// signal and follows with RevokeRefreshTokenFamily. Returns the new token's id.
func (p *Pool) RotateRefreshToken(ctx context.Context, oldHash, newHash string) (int64, error) {
	var newID int64
	err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
		var userID, familyID int64
		err := tx.QueryRow(ctx, `
			SELECT user_id, family_id FROM refresh_tokens
			WHERE token_hash = $1 AND replaced_by IS NULL AND revoked_at IS NULL`,
			oldHash,
		).Scan(&userID, &familyID)
		if err != nil {
			return err // pgx.ErrNoRows => already rotated/revoked/unknown: the reuse path
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
			VALUES ($1, $2, $3, (SELECT expires_at FROM refresh_tokens WHERE token_hash = $4))
			RETURNING id`,
			userID, familyID, newHash, oldHash,
		).Scan(&newID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE refresh_tokens SET replaced_by = $1 WHERE token_hash = $2`,
			newID, oldHash)
		return err
	})
	if err != nil {
		return 0, err
	}
	return newID, nil
}

// RevokeRefreshTokenFamily revokes every non-revoked token in a family, the
// reuse-detection and logout response (RFC-003 4.1/4.3). Returns rows affected.
func (p *Pool) RevokeRefreshTokenFamily(ctx context.Context, familyID int64) (int64, error) {
	tag, err := p.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE family_id = $1 AND revoked_at IS NULL`, familyID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RevokeAllForUser revokes every non-revoked refresh token for a user across all
// families, the "log out of all devices" lever (PRD-001 3.4 AC13, RFC-003 4.3). It
// also runs on an account-deletion request so a deletion-pending account stops
// acting. Returns rows affected.
func (p *Pool) RevokeAllForUser(ctx context.Context, userID int64) (int64, error) {
	tag, err := p.Exec(ctx, `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredRefreshTokens hard-deletes tokens past their expiry, a maintenance
// sweep. Returns the count deleted.
func (p *Pool) DeleteExpiredRefreshTokens(ctx context.Context) (int64, error) {
	tag, err := p.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

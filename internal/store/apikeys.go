package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// API keys are org-scoped (RLS on org_id), except the by-hash verify lookup, which
// runs before any org is in context (the key is the credential and carries its own
// org, RFC-003 5.4). That lookup goes through the capability policy
// api_keys_hash_lookup (schema.sql). Keys are stored hashed: the caller passes the
// SHA-256 of the full pulse_sk_ key (crypto.HashToken). The secret is never stored.

const apiKeyColumns = `id, org_id, name, prefix, token_hash, role, created_by,
	created_at, last_used_at, revoked_at`

func scanAPIKey(row pgx.Row) (*domain.APIKey, error) {
	var k domain.APIKey
	var role string
	err := row.Scan(&k.ID, &k.OrgID, &k.Name, &k.Prefix, &k.TokenHash, &role,
		&k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt)
	if err != nil {
		return nil, err
	}
	k.Role = domain.Role(role)
	return &k, nil
}

// CreateAPIKey inserts a key. The caller passes the SHA-256 hash (never the raw
// secret) and the non-secret prefix. role must be member or admin (the CHECK
// constraint backs this). Returns the new id.
func (p *Pool) CreateAPIKey(ctx context.Context, k *domain.APIKey) (int64, error) {
	var id int64
	err := p.WithOrg(ctx, k.OrgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO api_keys (org_id, name, prefix, token_hash, role, created_by)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id`,
			k.OrgID, k.Name, k.Prefix, k.TokenHash, string(k.Role), k.CreatedBy,
		).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	k.ID = id
	return id, nil
}

// GetAPIKeyByHash is the verify lookup: it loads the key row by its hash without an
// org scope, the same shape as GetInvitationByToken. It sets app.api_key_hash for
// the transaction so the capability RLS policy lets the one matching row through.
// Returns pgx.ErrNoRows for an unknown hash. The caller checks revoked_at and uses
// org_id + role from the row to build the request principal (RFC-003 5.3/5.4).
func (p *Pool) GetAPIKeyByHash(ctx context.Context, tokenHash string) (*domain.APIKey, error) {
	var key *domain.APIKey
	err := p.withAPIKeyHash(ctx, tokenHash, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE token_hash = $1`, tokenHash)
		v, err := scanAPIKey(row)
		if err != nil {
			return err
		}
		key = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

// GetAPIKey loads one key by org + id (org-scoped). Returns pgx.ErrNoRows when the
// id is not in the org. The revoke handler reads it for the token_hash so it can
// bust the verify cache after revoking.
func (p *Pool) GetAPIKey(ctx context.Context, orgID, keyID int64) (*domain.APIKey, error) {
	var key *domain.APIKey
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id = $1 AND org_id = $2`, keyID, orgID)
		v, err := scanAPIKey(row)
		if err != nil {
			return err
		}
		key = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return key, nil
}

// ListAPIKeys returns every key in an org, newest first. Secrets are never stored,
// so the row carries only the non-secret prefix.
func (p *Pool) ListAPIKeys(ctx context.Context, orgID int64) ([]*domain.APIKey, error) {
	var out []*domain.APIKey
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE org_id = $1 ORDER BY id DESC`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			k, err := scanAPIKey(rows)
			if err != nil {
				return err
			}
			out = append(out, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RevokeAPIKey stamps revoked_at on a key. Returns rows affected so a no-op (wrong
// org/id or already revoked) is distinguishable. The auth layer also busts the
// Redis cache entry so the next request misses and sees the revoked row (RFC-003 5.3).
func (p *Pool) RevokeAPIKey(ctx context.Context, orgID, keyID int64) (int64, error) {
	var affected int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL`,
			keyID, orgID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	return affected, err
}

// TouchAPIKey stamps last_used_at = now() for a key. The auth layer calls this
// throttled (not on every request, RFC-003 5.3). It runs without an org scope via
// the same capability lookup, since the verify path has only the hash in hand.
func (p *Pool) TouchAPIKey(ctx context.Context, keyID int64) error {
	_, err := p.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)
	return err
}

// withAPIKeyHash runs fn in a transaction with app.api_key_hash set, so the
// capability RLS policy on api_keys lets the one matching row be read without an
// org scope. Mirrors withInviteToken but for the key-verify capability.
func (p *Pool) withAPIKeyHash(ctx context.Context, tokenHash string, fn func(tx pgx.Tx) error) (err error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Exec(ctx, "SELECT set_config('app.api_key_hash', $1, true)", tokenHash); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

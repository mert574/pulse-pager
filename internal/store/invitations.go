package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// Invitations are org-scoped (RLS on org_id), except the by-token lookup, which is
// the pre-login accept path and is reached through the capability policy
// invitations_token_lookup (see schema.sql). Tokens are stored hashed; the raw
// token lives only in the email link, and the caller passes its SHA-256 here.

const inviteColumns = `id, org_id, email, role, state, token_hash, locale,
	created_by, created_at, expires_at, accepted_at`

func scanInvite(row pgx.Row) (*domain.Invitation, error) {
	var inv domain.Invitation
	var role, state string
	// token_hash is nullable (RFC-019): a row exists briefly with no token between the
	// api INSERT and the notifier minting it. A NULL scans as the empty TokenHash.
	var tokenHash *string
	err := row.Scan(&inv.ID, &inv.OrgID, &inv.Email, &role, &state, &tokenHash,
		&inv.Locale, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt)
	if err != nil {
		return nil, err
	}
	if tokenHash != nil {
		inv.TokenHash = *tokenHash
	}
	inv.Role = domain.Role(role)
	inv.State = domain.InvitationState(state)
	return &inv, nil
}

// CreateInvitation inserts a pending invitation. The caller passes the SHA-256
// token hash (never the raw token) and the 7-day expiry. The unique partial index
// uniq_invite_pending makes a second pending invite for the same (org, email) a
// unique violation (I7).
func (p *Pool) CreateInvitation(ctx context.Context, inv *domain.Invitation) (int64, error) {
	if inv.Locale == "" {
		inv.Locale = "en"
	}
	if inv.ExpiresAt.IsZero() {
		inv.ExpiresAt = time.Now().Add(7 * 24 * time.Hour)
	}
	var id int64
	err := p.WithOrg(ctx, inv.OrgID, func(tx pgx.Tx) error {
		// NULLIF stores NULL when the caller passes no token (RFC-019: the api creates
		// the row token-less and the notifier mints later). A seed caller that does set
		// a hash keeps it.
		return tx.QueryRow(ctx, `
			INSERT INTO invitations (org_id, email, role, token_hash, locale, created_by, expires_at)
			VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7)
			RETURNING id`,
			inv.OrgID, inv.Email, string(inv.Role), inv.TokenHash, inv.Locale, inv.CreatedBy, inv.ExpiresAt,
		).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	inv.ID = id
	return id, nil
}

// GetInvitationByToken loads an invitation by its token hash, without an org
// scope (pre-login accept path). It sets app.invite_token for the transaction so
// the capability RLS policy lets the single matching row through.
func (p *Pool) GetInvitationByToken(ctx context.Context, tokenHash string) (*domain.Invitation, error) {
	var inv *domain.Invitation
	err := p.withInviteToken(ctx, tokenHash, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+inviteColumns+` FROM invitations WHERE token_hash = $1`, tokenHash)
		v, err := scanInvite(row)
		if err != nil {
			return err
		}
		inv = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// ListInvitations returns every invitation in an org, newest first.
func (p *Pool) ListInvitations(ctx context.Context, orgID int64) ([]*domain.Invitation, error) {
	var out []*domain.Invitation
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+inviteColumns+` FROM invitations WHERE org_id = $1 ORDER BY id DESC`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := scanInvite(rows)
			if err != nil {
				return err
			}
			out = append(out, inv)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CountPendingInvitations returns how many pending invitations an org has. Each
// reserves a seat (PRD-001 5.1), so this is the reserved-invite half of the seat
// meter that the service adds to the accepted-member count.
func (p *Pool) CountPendingInvitations(ctx context.Context, orgID int64) (int, error) {
	var n int
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM invitations WHERE org_id = $1 AND state = 'pending'`, orgID).Scan(&n)
	})
	return n, err
}

// GetInvitation loads one invitation by id within an org (the org-scoped read for
// revoke/resend, which already know the org from the path).
func (p *Pool) GetInvitation(ctx context.Context, orgID, inviteID int64) (*domain.Invitation, error) {
	var inv *domain.Invitation
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+inviteColumns+` FROM invitations WHERE id = $1 AND org_id = $2`, inviteID, orgID)
		v, err := scanInvite(row)
		if err != nil {
			return err
		}
		inv = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// ResendInvitation refreshes a pending invitation's 7-day expiry in place (PRD-001
// 6.2: same row, refreshed clock; the seat reservation is unchanged). The new token is
// minted by the notifier when it re-sends (RFC-019): it overwrites the row's token_hash
// via SetInvitationToken, so the old link keeps working until the new one is minted and
// then stops, with no window where the invite has no valid link. Returns rows affected
// so a no-op (already terminal) is distinguishable.
func (p *Pool) ResendInvitation(ctx context.Context, orgID, inviteID int64, expiresAt time.Time) (int64, error) {
	var affected int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE invitations SET expires_at = $3
			WHERE id = $1 AND org_id = $2 AND state = 'pending'`,
			inviteID, orgID, expiresAt)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	return affected, err
}

// SetInvitationToken writes a freshly minted invite token hash to a still-pending
// invitation (RFC-019 section 5.2). The notifier mints the token at send time and
// calls this under the org scope, then emails the link. It overwrites any existing
// hash on purpose: on an at-least-once redelivery the notifier re-mints and the link
// it emails always matches the stored hash, so a send that failed before is retried
// rather than lost (a rare redelivery after a successful send just supersedes the
// first link). The "state = pending" guard means a revoke/accept that landed first
// wins: affected == 0, and the notifier skips the send. A resend follows the same
// path (bump expiry, publish, re-mint here). Returns rows affected.
func (p *Pool) SetInvitationToken(ctx context.Context, orgID, inviteID int64, tokenHash string) (int64, error) {
	var affected int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE invitations SET token_hash = $3
			WHERE id = $1 AND org_id = $2 AND state = 'pending'`,
			inviteID, orgID, tokenHash)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	return affected, err
}

// AcceptInvitation flips a pending invitation to accepted and creates the
// membership, in one transaction (RFC-003 2.6). The email-match guard and seat
// flip are service-layer; this owns the atomic state change + membership insert.
// It only acts on a still-pending invite, so a double-accept is a no-op that
// returns pgx.ErrNoRows. Returns the new membership id.
func (p *Pool) AcceptInvitation(ctx context.Context, orgID, inviteID, userID int64) (int64, error) {
	var membershipID int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		var role string
		err := tx.QueryRow(ctx, `
			UPDATE invitations SET state = 'accepted', accepted_at = now()
			WHERE id = $1 AND org_id = $2 AND state = 'pending'
			RETURNING role`,
			inviteID, orgID,
		).Scan(&role)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO memberships (org_id, user_id, role)
			VALUES ($1,$2,$3)
			RETURNING id`,
			orgID, userID, role,
		).Scan(&membershipID)
	})
	if err != nil {
		return 0, err
	}
	return membershipID, nil
}

// RevokeInvitation flips a pending invitation to revoked. Returns rows affected
// so a no-op (already accepted/revoked/expired, or wrong org) is distinguishable.
func (p *Pool) RevokeInvitation(ctx context.Context, orgID, inviteID int64) (int64, error) {
	var affected int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE invitations SET state = 'revoked'
			WHERE id = $1 AND org_id = $2 AND state = 'pending'`,
			inviteID, orgID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	return affected, err
}

// ExpireInvitations flips every pending invitation past its expiry to expired,
// across all orgs. This is a control-plane sweep (a scheduled job) that spans every
// org, so it cannot set one app.current_org and cannot run under the org RLS policy.
// It must run on a privileged connection (a maintenance role allowed to bypass
// RLS, RFC-001 5.2), the same as the scheduler's cross-org reads, not on the
// per-request app pool. Returns the count expired.
func (p *Pool) ExpireInvitations(ctx context.Context) (int64, error) {
	tag, err := p.Exec(ctx, `
		UPDATE invitations SET state = 'expired'
		WHERE state = 'pending' AND expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// withInviteToken runs fn in a transaction with app.invite_token set, so the
// capability RLS policy on invitations lets the one matching row be read without
// an org scope. Mirrors WithOrg but for the token capability.
func (p *Pool) withInviteToken(ctx context.Context, tokenHash string, fn func(tx pgx.Tx) error) (err error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Exec(ctx, "SELECT set_config('app.invite_token', $1, true)", tokenHash); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

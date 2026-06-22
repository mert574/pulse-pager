package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// Memberships are org-scoped, so every method runs through WithOrg, which sets
// app.current_org so RLS keys off the right org. The caller supplies the org id;
// it is never read from a request body (RFC-001 5.1).

// CreateMembership inserts a membership and returns its new id.
func (p *Pool) CreateMembership(ctx context.Context, m *domain.Membership) (int64, error) {
	var id int64
	err := p.WithOrg(ctx, m.OrgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO memberships (org_id, user_id, role)
			VALUES ($1,$2,$3)
			RETURNING id`,
			m.OrgID, m.UserID, string(m.Role),
		).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	m.ID = id
	return id, nil
}

// GetMembership returns the (user, org) membership, or pgx.ErrNoRows if the user
// is not a member of that org.
func (p *Pool) GetMembership(ctx context.Context, userID, orgID int64) (*domain.Membership, error) {
	var m domain.Membership
	var role string
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id, org_id, user_id, role, created_at
			FROM memberships WHERE user_id = $1 AND org_id = $2`,
			userID, orgID,
		).Scan(&m.ID, &m.OrgID, &m.UserID, &role, &m.CreatedAt)
	})
	if err != nil {
		return nil, err
	}
	m.Role = domain.Role(role)
	return &m, nil
}

// ListMembers returns every membership in an org, oldest first.
func (p *Pool) ListMembers(ctx context.Context, orgID int64) ([]*domain.Membership, error) {
	var out []*domain.Membership
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, org_id, user_id, role, created_at
			FROM memberships WHERE org_id = $1 ORDER BY id`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m domain.Membership
			var role string
			if err := rows.Scan(&m.ID, &m.OrgID, &m.UserID, &role, &m.CreatedAt); err != nil {
				return err
			}
			m.Role = domain.Role(role)
			out = append(out, &m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateMemberRole changes a member's role. The at-least-one-owner trigger blocks
// demoting the last owner (PRD-001 I1); the service layer should refuse it first
// with a friendly message, this is the DB backstop. Returns the rows affected so
// the caller can tell a no-op (wrong user/org) from a real update.
func (p *Pool) UpdateMemberRole(ctx context.Context, orgID, userID int64, role domain.Role) (int64, error) {
	var affected int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE memberships SET role = $3 WHERE org_id = $1 AND user_id = $2`,
			orgID, userID, string(role))
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	return affected, err
}

// RemoveMember deletes a membership. The at-least-one-owner trigger blocks
// removing the last owner. Returns the rows affected.
func (p *Pool) RemoveMember(ctx context.Context, orgID, userID int64) (int64, error) {
	var affected int64
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM memberships WHERE org_id = $1 AND user_id = $2`, orgID, userID)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	return affected, err
}

// CountMembers returns how many memberships an org has. The seat meter counts
// accepted members plus reserved pending invites (PRD-001 5.1); this is the
// accepted-member half.
func (p *Pool) CountMembers(ctx context.Context, orgID int64) (int, error) {
	var n int
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM memberships WHERE org_id = $1`, orgID).Scan(&n)
	})
	return n, err
}

// CountOwners returns how many owners an org has. The service layer reads this in
// the same transaction as a demote/remove to enforce the last-owner rule (the DB
// trigger is the backstop, RFC-003 7.5).
func (p *Pool) CountOwners(ctx context.Context, orgID int64) (int, error) {
	var n int
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM memberships WHERE org_id = $1 AND role = 'owner'`, orgID).Scan(&n)
	})
	return n, err
}

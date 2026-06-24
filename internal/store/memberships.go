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

// ActiveMemberEmails returns the email addresses of the given user ids that are
// active members of the org. It is the send-time half of the Team email channel's
// org scoping (RFC-007a): the channel config holds only member ids, and this join
// turns them into addresses, dropping any id that is not an active member of THIS
// org. So a tampered config can't email outside the org, and a member removed after
// the channel was saved is silently dropped on the next send. memberships is
// RLS-scoped by WithOrg (app.current_org), so the join only sees this org's rows;
// users is the global account table joined on it for the address. An active member
// is one whose user row status is 'active' (not deletion-pending or deleted). An
// empty userIDs returns no rows.
func (p *Pool) ActiveMemberEmails(ctx context.Context, orgID int64, userIDs []int64) ([]string, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	var out []string
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.email
			FROM memberships m
			JOIN users u ON u.id = m.user_id
			WHERE m.org_id = $1 AND m.user_id = ANY($2) AND u.status = 'active'
			ORDER BY u.email`, orgID, userIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var email string
			if err := rows.Scan(&email); err != nil {
				return err
			}
			out = append(out, email)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AreActiveMembers reports whether every id in userIDs is an active member of the
// org. It is the save-time half of the Team email channel's org scoping (RFC-007a):
// channel create/update calls it to reject a member id that does not belong to the
// org before the channel is stored. The check runs through WithOrg so memberships
// RLS confines the count to this org. An empty userIDs is false (an email channel
// with no recipients is not a valid config; the provider's Validate also rejects it).
func (p *Pool) AreActiveMembers(ctx context.Context, orgID int64, userIDs []int64) (bool, error) {
	if len(userIDs) == 0 {
		return false, nil
	}
	var matched int
	err := p.WithOrg(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(DISTINCT m.user_id)
			FROM memberships m
			JOIN users u ON u.id = m.user_id
			WHERE m.org_id = $1 AND m.user_id = ANY($2) AND u.status = 'active'`,
			orgID, userIDs).Scan(&matched)
	})
	if err != nil {
		return false, err
	}
	return matched == len(dedupeIDs(userIDs)), nil
}

// dedupeIDs returns userIDs with duplicates removed, preserving first-seen order. It
// lets AreActiveMembers compare the matched count against the number of distinct ids
// asked about, so a config that repeats an id is not counted twice.
func dedupeIDs(userIDs []int64) []int64 {
	seen := make(map[int64]struct{}, len(userIDs))
	out := make([]int64, 0, len(userIDs))
	for _, id := range userIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
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

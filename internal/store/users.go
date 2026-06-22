package store

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// Users and user_identities are global tables (RFC-001 5.2): a user spans orgs, so
// they carry no org_id and are not under org RLS. Access is mediated by the auth
// path, which scopes by the authenticated user, not by org. These methods use the
// pool directly (not WithOrg) the same way the scheduler's cross-org reads do.

// CreateUser inserts a user and returns its new id. Locale/timezone default to
// en/UTC at the DB level if left empty.
func (p *Pool) CreateUser(ctx context.Context, u *domain.User) (int64, error) {
	if u.Locale == "" {
		u.Locale = "en"
	}
	if u.Timezone == "" {
		u.Timezone = "UTC"
	}
	var id int64
	err := p.QueryRow(ctx, `
		INSERT INTO users (email, email_verified, name, avatar_url, locale, timezone)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id`,
		u.Email, u.EmailVerified, u.Name, u.AvatarURL, u.Locale, u.Timezone,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	u.ID = id
	return id, nil
}

const userColumns = `id, email, email_verified, name, avatar_url, locale, timezone,
	status, deletion_pending_at, created_at, last_login_at`

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Email, &u.EmailVerified, &u.Name, &u.AvatarURL,
		&u.Locale, &u.Timezone, &u.Status, &u.DeletionPendingAt, &u.CreatedAt, &u.LastLoginAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUser looks a user up by id.
func (p *Pool) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	return scanUser(p.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// GetUserByEmail looks a user up by email, case-insensitively, skipping
// hard-deleted rows. This is the account-linking lookup (PRD-001 3.3).
func (p *Pool) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	return scanUser(p.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE lower(email) = lower($1) AND status <> 'deleted'`, email))
}

// UpdateUser writes the editable profile and i18n fields.
func (p *Pool) UpdateUser(ctx context.Context, u *domain.User) error {
	_, err := p.Exec(ctx, `
		UPDATE users SET name = $2, avatar_url = $3, locale = $4, timezone = $5
		WHERE id = $1`,
		u.ID, u.Name, u.AvatarURL, u.Locale, u.Timezone,
	)
	return err
}

// SetLastLogin stamps last_login_at = now() for a user.
func (p *Pool) SetLastLogin(ctx context.Context, userID int64) error {
	_, err := p.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, userID)
	return err
}

// LinkIdentity attaches a social identity to a user (RFC-003 2.4). The
// unique(provider, provider_user_id) index makes a provider account map to one
// user (I5); a duplicate returns the underlying unique-violation error.
func (p *Pool) LinkIdentity(ctx context.Context, idn *domain.UserIdentity) (int64, error) {
	var id int64
	err := p.QueryRow(ctx, `
		INSERT INTO user_identities (user_id, provider, provider_user_id)
		VALUES ($1,$2,$3)
		RETURNING id`,
		idn.UserID, string(idn.Provider), idn.ProviderUserID,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	idn.ID = id
	return id, nil
}

// UnlinkIdentity removes one provider identity from a user (RFC-003 2.4 unlink).
// It is keyed by (user, provider) so a user only ever removes their own identity.
// Returns the rows affected so the caller can tell a no-op (not linked) from a real
// delete. The handler refuses to remove the last identity first, so the user is
// never left with no way to sign in; this is the plain delete.
func (p *Pool) UnlinkIdentity(ctx context.Context, userID int64, provider domain.IdentityProvider) (int64, error) {
	tag, err := p.Exec(ctx,
		`DELETE FROM user_identities WHERE user_id = $1 AND provider = $2`,
		userID, string(provider))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// FirstSignIn is the result of the atomic first-sign-in flow: the new user, its
// personal org, and the owner membership, all created in one transaction.
type FirstSignIn struct {
	UserID       int64
	IdentityID   int64
	OrgID        int64
	MembershipID int64
}

// CreateUserWithPersonalOrg runs the first-sign-in transaction (PRD-001 3.2,
// RFC-003 2.3): in one Postgres transaction it creates the user from the verified
// provider profile, links the identity, creates a personal Organization on the Free
// plan, and makes the user its owner. If any step fails the whole thing rolls back,
// so a user is never left with no org (PRD-001 I2). The membership insert sets
// app.current_org in the same transaction so the org RLS policy lets it through.
// orgName/orgSlug are derived by the caller from the user's name or email.
func (p *Pool) CreateUserWithPersonalOrg(ctx context.Context, u *domain.User, idn *domain.UserIdentity, orgName, orgSlug string) (*FirstSignIn, error) {
	if u.Locale == "" {
		u.Locale = "en"
	}
	if u.Timezone == "" {
		u.Timezone = "UTC"
	}
	var out FirstSignIn
	err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (email, email_verified, name, avatar_url, locale, timezone)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
			u.Email, u.EmailVerified, u.Name, u.AvatarURL, u.Locale, u.Timezone,
		).Scan(&out.UserID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO user_identities (user_id, provider, provider_user_id)
			VALUES ($1,$2,$3) RETURNING id`,
			out.UserID, string(idn.Provider), idn.ProviderUserID,
		).Scan(&out.IdentityID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO organizations (name, slug) VALUES ($1,$2) RETURNING id`,
			orgName, orgSlug,
		).Scan(&out.OrgID); err != nil {
			return err
		}
		// The membership insert is org-scoped, so set app.current_org in this same
		// transaction for the RLS policy, mirroring WithOrg.
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_org', $1, true)",
			strconv.FormatInt(out.OrgID, 10)); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO memberships (org_id, user_id, role) VALUES ($1,$2,'owner') RETURNING id`,
			out.OrgID, out.UserID,
		).Scan(&out.MembershipID)
	})
	if err != nil {
		return nil, err
	}
	u.ID = out.UserID
	idn.ID = out.IdentityID
	idn.UserID = out.UserID
	return &out, nil
}

// GetIdentity finds the identity for a (provider, providerUserID) pair, which is
// the sign-in lookup that resolves a returning user.
func (p *Pool) GetIdentity(ctx context.Context, provider domain.IdentityProvider, providerUserID string) (*domain.UserIdentity, error) {
	var idn domain.UserIdentity
	var prov string
	err := p.QueryRow(ctx, `
		SELECT id, user_id, provider, provider_user_id, created_at
		FROM user_identities WHERE provider = $1 AND provider_user_id = $2`,
		string(provider), providerUserID,
	).Scan(&idn.ID, &idn.UserID, &prov, &idn.ProviderUserID, &idn.CreatedAt)
	if err != nil {
		return nil, err
	}
	idn.Provider = domain.IdentityProvider(prov)
	return &idn, nil
}

// ListIdentitiesForUser returns every identity linked to a user.
func (p *Pool) ListIdentitiesForUser(ctx context.Context, userID int64) ([]*domain.UserIdentity, error) {
	rows, err := p.Query(ctx, `
		SELECT id, user_id, provider, provider_user_id, created_at
		FROM user_identities WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.UserIdentity
	for rows.Next() {
		var idn domain.UserIdentity
		var prov string
		if err := rows.Scan(&idn.ID, &idn.UserID, &prov, &idn.ProviderUserID, &idn.CreatedAt); err != nil {
			return nil, err
		}
		idn.Provider = domain.IdentityProvider(prov)
		out = append(out, &idn)
	}
	return out, rows.Err()
}

package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"pulse/internal/domain"
)

// ErrSlugTaken is returned when an org slug clashes with an existing active org.
// The caller maps it to a 422 validation error rather than a 500.
var ErrSlugTaken = errors.New("org slug already taken")

// The organizations table is the tenant root. It is not under org RLS in this
// schema: the auth-path lookups here (by slug, by user) run before an org is in
// context or span many orgs, so they cannot be org-scoped. Org isolation lives on
// the org-owned tables (monitors, memberships, invitations, ...) via WithOrg +
// RLS. RFC-001 5.2 keys org RLS off the org's own id; we keep organizations open
// to the auth path and rely on the service layer + membership checks to decide
// which orgs a caller may see. Decision noted in the work-package summary.

const orgColumns = `id, name, slug, plan_id, plan, default_locale, default_timezone, created_at, deleted_at`

func scanOrg(row pgx.Row) (*domain.Organization, error) {
	var o domain.Organization
	err := row.Scan(&o.ID, &o.Name, &o.Slug, &o.PlanID, &o.Plan, &o.DefaultLocale,
		&o.DefaultTimezone, &o.CreatedAt, &o.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// CreateOrganization inserts an org and returns its new id. Locale/timezone
// default to en/UTC at the DB level if left empty.
func (p *Pool) CreateOrganization(ctx context.Context, o *domain.Organization) (int64, error) {
	if o.DefaultLocale == "" {
		o.DefaultLocale = "en"
	}
	if o.DefaultTimezone == "" {
		o.DefaultTimezone = "UTC"
	}
	var id int64
	err := p.QueryRow(ctx, `
		INSERT INTO organizations (name, slug, plan_id, default_locale, default_timezone)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id`,
		o.Name, o.Slug, o.PlanID, o.DefaultLocale, o.DefaultTimezone,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	o.ID = id
	return id, nil
}

// CreateOrgWithOwner creates a new org and makes the caller its owner in one
// transaction (PRD-001 7.2: any member may create an org; it is per-user). It
// mirrors CreateUserWithPersonalOrg's membership-insert pattern: it sets
// app.current_org in the same transaction so the org RLS policy lets the membership
// row through. If slug is empty it is derived from the name with a short random
// suffix. A slug clash with an explicit slug returns ErrSlugTaken; a derived-slug
// clash is retried with a fresh suffix. Returns the new org and the caller's role.
func (p *Pool) CreateOrgWithOwner(ctx context.Context, name, slug string, ownerID int64) (*domain.Organization, domain.Role, error) {
	explicitSlug := slug != ""
	if slug == "" {
		slug = deriveSlug(name)
	}
	const maxTries = 5
	for attempt := 0; attempt < maxTries; attempt++ {
		org := &domain.Organization{Name: name, Slug: slug, Plan: "free", DefaultLocale: "en", DefaultTimezone: "UTC"}
		err := pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `
				INSERT INTO organizations (name, slug, default_locale, default_timezone)
				VALUES ($1,$2,$3,$4) RETURNING id, created_at`,
				org.Name, org.Slug, org.DefaultLocale, org.DefaultTimezone,
			).Scan(&org.ID, &org.CreatedAt); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_org', $1, true)",
				strconv.FormatInt(org.ID, 10)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO memberships (org_id, user_id, role) VALUES ($1,$2,'owner')`,
				org.ID, ownerID)
			return err
		})
		if err == nil {
			return org, domain.RoleOwner, nil
		}
		if isUniqueViolation(err) {
			if explicitSlug {
				return nil, "", ErrSlugTaken
			}
			slug = deriveSlug(name) // fresh suffix, retry
			continue
		}
		return nil, "", err
	}
	return nil, "", ErrSlugTaken
}

// isUniqueViolation reports whether err is a Postgres unique-constraint violation
// (SQLSTATE 23505), used to turn a slug clash into ErrSlugTaken.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// deriveSlug builds a URL-safe slug from a name plus a short random suffix so two
// orgs with the same name still get distinct slugs.
func deriveSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "workspace"
	}
	raw := make([]byte, 4)
	_, _ = rand.Read(raw)
	return base + "-" + strings.ToLower(base64.RawURLEncoding.EncodeToString(raw))
}

// GetOrganization looks an org up by id.
func (p *Pool) GetOrganization(ctx context.Context, id int64) (*domain.Organization, error) {
	return scanOrg(p.QueryRow(ctx, `SELECT `+orgColumns+` FROM organizations WHERE id = $1`, id))
}

// GetOrganizationBySlug looks an org up by slug, case-insensitively, skipping
// soft-deleted orgs (the slug is free to reuse after a deletion grace ends).
func (p *Pool) GetOrganizationBySlug(ctx context.Context, slug string) (*domain.Organization, error) {
	return scanOrg(p.QueryRow(ctx,
		`SELECT `+orgColumns+` FROM organizations WHERE lower(slug) = lower($1) AND deleted_at IS NULL`, slug))
}

// ListOrganizationsForUser returns the active (not soft-deleted) orgs a user is a
// member of. This is the "orgs I belong to" list (PRD-001 7.3), user-scoped not
// org-scoped, so it cannot set one app.current_org. It runs in a transaction with
// app.current_user set so the memberships_self_read capability policy lets the
// user's own membership rows through the join under RLS.
func (p *Pool) ListOrganizationsForUser(ctx context.Context, userID int64) ([]*domain.Organization, error) {
	var out []*domain.Organization
	err := p.withUser(ctx, userID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT o.id, o.name, o.slug, o.plan_id, o.plan, o.default_locale, o.default_timezone, o.created_at, o.deleted_at
			FROM organizations o
			JOIN memberships m ON m.org_id = o.id
			WHERE m.user_id = $1 AND o.deleted_at IS NULL
			ORDER BY o.id`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanOrg(rows)
			if err != nil {
				return err
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// withUser runs fn in a transaction with app.current_user set, so the
// memberships_self_read capability policy lets a user read its own membership rows
// across orgs without an org scope. Mirrors WithOrg but for the user capability.
func (p *Pool) withUser(ctx context.Context, userID int64, fn func(tx pgx.Tx) error) (err error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Exec(ctx, "SELECT set_config('app.current_user', $1, true)", strconv.FormatInt(userID, 10)); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateOrganization writes the editable org fields (name, slug, i18n defaults).
func (p *Pool) UpdateOrganization(ctx context.Context, o *domain.Organization) error {
	_, err := p.Exec(ctx, `
		UPDATE organizations SET name = $2, slug = $3, default_locale = $4, default_timezone = $5
		WHERE id = $1`,
		o.ID, o.Name, o.Slug, o.DefaultLocale, o.DefaultTimezone,
	)
	return err
}

// SoftDeleteOrganization stamps deleted_at = now(), starting the 14-day grace
// (RFC-015). The hard-delete cascade at grace end is a separate later work
// package; this just hides the org and starts the clock. Idempotent: re-deleting
// keeps the original deleted_at.
func (p *Pool) SoftDeleteOrganization(ctx context.Context, orgID int64) error {
	_, err := p.Exec(ctx,
		`UPDATE organizations SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, orgID)
	return err
}

//go:build integration

// Identity data-layer integration test. Same testcontainers pattern as
// foundation_test.go: start Postgres, apply the schema, then exercise the store
// methods for users, identities, orgs, memberships, invitations, and refresh
// tokens against the app pool (so RLS is in force on the org-scoped tables).
package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/crypto"
	"pulse/internal/domain"
	"pulse/internal/store"
)

func TestIdentity(t *testing.T) {
	ctx := context.Background()

	pgC, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("pulse"),
		postgres.WithUsername("pulse"),
		postgres.WithPassword("pulse"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	defer func() { _ = pgC.Terminate(ctx) }()

	host, err := pgC.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}
	adminDSN := fmt.Sprintf("postgres://pulse:pulse@%s:%s/pulse?sslmode=disable", host, port.Port())
	appDSN := fmt.Sprintf("postgres://pulse_app:pulse_app@%s:%s/pulse?sslmode=disable", host, port.Port())

	admin, err := store.Open(ctx, adminDSN)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()
	if err := store.ApplySchema(ctx, admin); err != nil {
		t.Fatalf("apply schema: %v", err)
	}

	// The app pool is the one that runs through RLS (non-superuser role). All the
	// store methods are exercised against it so the org-scoped tables really go
	// through WithOrg + RLS, like production.
	app, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	defer app.Close()

	t.Run("user_create_get_and_identity_link", func(t *testing.T) {
		u := &domain.User{Email: "Alice@Example.com", EmailVerified: true, Name: "Alice"}
		id, err := app.CreateUser(ctx, u)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		if u.Locale != "en" || u.Timezone != "UTC" {
			t.Fatalf("locale/timezone defaults not applied: %q %q", u.Locale, u.Timezone)
		}

		got, err := app.GetUser(ctx, id)
		if err != nil {
			t.Fatalf("get user: %v", err)
		}
		if got.Locale != "en" || got.Timezone != "UTC" || got.Status != "active" {
			t.Fatalf("unexpected stored user: %+v", got)
		}

		// case-insensitive lookup (the unique index is on lower(email))
		byEmail, err := app.GetUserByEmail(ctx, "alice@example.com")
		if err != nil {
			t.Fatalf("get by email: %v", err)
		}
		if byEmail.ID != id {
			t.Fatalf("get-by-email returned wrong user: %d != %d", byEmail.ID, id)
		}

		// profile + locale/timezone update
		got.Name = "Alice B"
		got.Locale = "de"
		got.Timezone = "Europe/Berlin"
		if err := app.UpdateUser(ctx, got); err != nil {
			t.Fatalf("update user: %v", err)
		}
		reread, _ := app.GetUser(ctx, id)
		if reread.Name != "Alice B" || reread.Locale != "de" || reread.Timezone != "Europe/Berlin" {
			t.Fatalf("update not persisted: %+v", reread)
		}

		// last login
		if reread.LastLoginAt != nil {
			t.Fatal("last_login_at should start nil")
		}
		if err := app.SetLastLogin(ctx, id); err != nil {
			t.Fatalf("set last login: %v", err)
		}
		reread, _ = app.GetUser(ctx, id)
		if reread.LastLoginAt == nil {
			t.Fatal("last_login_at should be set")
		}

		// Two providers with the same verified email map to ONE user. This is the
		// building block of the service-intended auto-link path: the service finds
		// the existing user by verified email (GetUserByEmail), then links the new
		// provider to that same user id rather than creating a new user.
		if _, err := app.LinkIdentity(ctx, &domain.UserIdentity{
			UserID: id, Provider: domain.ProviderGoogle, ProviderUserID: "g-123",
		}); err != nil {
			t.Fatalf("link google: %v", err)
		}
		existing, err := app.GetUserByEmail(ctx, "alice@example.com")
		if err != nil {
			t.Fatalf("re-find by email for linking: %v", err)
		}
		if _, err := app.LinkIdentity(ctx, &domain.UserIdentity{
			UserID: existing.ID, Provider: domain.ProviderGitHub, ProviderUserID: "gh-456",
		}); err != nil {
			t.Fatalf("link github: %v", err)
		}
		idents, err := app.ListIdentitiesForUser(ctx, id)
		if err != nil {
			t.Fatalf("list identities: %v", err)
		}
		if len(idents) != 2 {
			t.Fatalf("expected 2 identities mapped to one user, got %d", len(idents))
		}

		gi, err := app.GetIdentity(ctx, domain.ProviderGoogle, "g-123")
		if err != nil || gi.UserID != id {
			t.Fatalf("get identity: id=%v err=%v", gi, err)
		}

		// I5: the same provider account cannot map to a second user.
		other := &domain.User{Email: "other@example.com", EmailVerified: true}
		oid, _ := app.CreateUser(ctx, other)
		if _, err := app.LinkIdentity(ctx, &domain.UserIdentity{
			UserID: oid, Provider: domain.ProviderGoogle, ProviderUserID: "g-123",
		}); err == nil {
			t.Fatal("linking a provider account to a second user must fail (I5)")
		}
	})

	t.Run("org_create_slug_unique_list_softdelete", func(t *testing.T) {
		owner := &domain.User{Email: "owner@acme.test", EmailVerified: true}
		ownerID, _ := app.CreateUser(ctx, owner)

		o := &domain.Organization{Name: "Acme", Slug: "acme"}
		orgID, err := app.CreateOrganization(ctx, o)
		if err != nil {
			t.Fatalf("create org: %v", err)
		}
		if o.DefaultLocale != "en" || o.DefaultTimezone != "UTC" {
			t.Fatalf("org i18n defaults not applied: %+v", o)
		}

		// slug uniqueness
		if _, err := app.CreateOrganization(ctx, &domain.Organization{Name: "Acme 2", Slug: "Acme"}); err == nil {
			t.Fatal("duplicate slug (case-insensitive) must be rejected")
		}

		bySlug, err := app.GetOrganizationBySlug(ctx, "ACME")
		if err != nil || bySlug.ID != orgID {
			t.Fatalf("get by slug: %v err=%v", bySlug, err)
		}

		// list-for-user needs a membership
		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgID, UserID: ownerID, Role: domain.RoleOwner}); err != nil {
			t.Fatalf("create owner membership: %v", err)
		}
		orgs, err := app.ListOrganizationsForUser(ctx, ownerID)
		if err != nil {
			t.Fatalf("list orgs for user: %v", err)
		}
		if len(orgs) != 1 || orgs[0].ID != orgID {
			t.Fatalf("expected one org for user, got %v", orgs)
		}

		// update
		o.ID = orgID
		o.Name = "Acme Inc"
		o.DefaultLocale = "fr"
		o.DefaultTimezone = "Europe/Paris"
		if err := app.UpdateOrganization(ctx, o); err != nil {
			t.Fatalf("update org: %v", err)
		}
		reread, _ := app.GetOrganization(ctx, orgID)
		if reread.Name != "Acme Inc" || reread.DefaultLocale != "fr" {
			t.Fatalf("org update not persisted: %+v", reread)
		}

		// soft delete hides it from slug + list lookups and frees the slug
		if err := app.SoftDeleteOrganization(ctx, orgID); err != nil {
			t.Fatalf("soft delete: %v", err)
		}
		if _, err := app.GetOrganizationBySlug(ctx, "acme"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("soft-deleted org should not resolve by slug, got %v", err)
		}
		orgs, _ = app.ListOrganizationsForUser(ctx, ownerID)
		if len(orgs) != 0 {
			t.Fatalf("soft-deleted org should not be listed, got %v", orgs)
		}
		gone, _ := app.GetOrganization(ctx, orgID)
		if gone.DeletedAt == nil {
			t.Fatal("deleted_at should be set after soft delete")
		}
		// the freed slug can be reused by a new org
		if _, err := app.CreateOrganization(ctx, &domain.Organization{Name: "Acme New", Slug: "acme"}); err != nil {
			t.Fatalf("slug should be reusable after soft delete: %v", err)
		}
	})

	t.Run("membership_roles_and_last_owner", func(t *testing.T) {
		orgID := mkOrg(ctx, t, app, "team-org")
		u1 := mkUser(ctx, t, app, "m1@team.test")
		u2 := mkUser(ctx, t, app, "m2@team.test")

		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgID, UserID: u1, Role: domain.RoleOwner}); err != nil {
			t.Fatalf("create owner: %v", err)
		}
		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgID, UserID: u2, Role: domain.RoleMember}); err != nil {
			t.Fatalf("create member: %v", err)
		}

		m, err := app.GetMembership(ctx, u1, orgID)
		if err != nil || m.Role != domain.RoleOwner {
			t.Fatalf("get membership: %v err=%v", m, err)
		}
		members, _ := app.ListMembers(ctx, orgID)
		if len(members) != 2 {
			t.Fatalf("expected 2 members, got %d", len(members))
		}

		n, err := app.CountOwners(ctx, orgID)
		if err != nil || n != 1 {
			t.Fatalf("count owners: %d err=%v", n, err)
		}

		// promote member to owner, then role update works
		if aff, err := app.UpdateMemberRole(ctx, orgID, u2, domain.RoleAdmin); err != nil || aff != 1 {
			t.Fatalf("update role: aff=%d err=%v", aff, err)
		}

		// last-owner rule: demoting the only owner must be blocked by the DB trigger
		if _, err := app.UpdateMemberRole(ctx, orgID, u1, domain.RoleAdmin); err == nil {
			t.Fatal("demoting the last owner must be blocked")
		}
		// and removing the only owner must be blocked too
		if _, err := app.RemoveMember(ctx, orgID, u1); err == nil {
			t.Fatal("removing the last owner must be blocked")
		}

		// once there are two owners, removing one is fine
		if _, err := app.UpdateMemberRole(ctx, orgID, u2, domain.RoleOwner); err != nil {
			t.Fatalf("promote to owner: %v", err)
		}
		if n, _ := app.CountOwners(ctx, orgID); n != 2 {
			t.Fatalf("expected 2 owners, got %d", n)
		}
		if aff, err := app.RemoveMember(ctx, orgID, u1); err != nil || aff != 1 {
			t.Fatalf("remove non-last owner: aff=%d err=%v", aff, err)
		}
		if n, _ := app.CountOwners(ctx, orgID); n != 1 {
			t.Fatalf("expected 1 owner after removal, got %d", n)
		}
	})

	t.Run("invitation_lifecycle_and_hashed_token", func(t *testing.T) {
		orgID := mkOrg(ctx, t, app, "invite-org")
		creator := mkUser(ctx, t, app, "creator@inv.test")
		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgID, UserID: creator, Role: domain.RoleOwner}); err != nil {
			t.Fatalf("seed owner: %v", err)
		}

		rawToken := "raw-invite-token-abc123"
		tokenHash := crypto.HashToken(rawToken)
		inv := &domain.Invitation{
			OrgID: orgID, Email: "newbie@inv.test", Role: domain.RoleMember,
			TokenHash: tokenHash, Locale: "de", CreatedBy: &creator,
		}
		invID, err := app.CreateInvitation(ctx, inv)
		if err != nil {
			t.Fatalf("create invitation: %v", err)
		}
		if inv.ExpiresAt.Before(time.Now().Add(6 * 24 * time.Hour)) {
			t.Fatalf("expiry should default to ~7 days, got %v", inv.ExpiresAt)
		}

		// hashed at rest: the raw column must not equal the plaintext token
		var stored string
		if err := admin.QueryRow(ctx, "SELECT token_hash FROM invitations WHERE id = $1", invID).Scan(&stored); err != nil {
			t.Fatalf("read raw token column: %v", err)
		}
		if stored == rawToken {
			t.Fatal("invitation token stored in plaintext")
		}
		if stored != tokenHash {
			t.Fatalf("stored hash mismatch: %q != %q", stored, tokenHash)
		}

		// by-token lookup works without an org scope (pre-login accept path)
		byToken, err := app.GetInvitationByToken(ctx, tokenHash)
		if err != nil || byToken.ID != invID {
			t.Fatalf("get by token: %v err=%v", byToken, err)
		}
		if byToken.Locale != "de" {
			t.Fatalf("invite locale not stored: %q", byToken.Locale)
		}

		list, _ := app.ListInvitations(ctx, orgID)
		if len(list) != 1 {
			t.Fatalf("expected 1 invitation, got %d", len(list))
		}

		// accept -> membership exists, invite flips to accepted
		invitee := mkUser(ctx, t, app, "newbie@inv.test")
		mID, err := app.AcceptInvitation(ctx, orgID, invID, invitee)
		if err != nil || mID == 0 {
			t.Fatalf("accept invitation: mID=%d err=%v", mID, err)
		}
		gotM, err := app.GetMembership(ctx, invitee, orgID)
		if err != nil || gotM.Role != domain.RoleMember {
			t.Fatalf("membership after accept: %v err=%v", gotM, err)
		}
		// double-accept is a no-op (no longer pending)
		if _, err := app.AcceptInvitation(ctx, orgID, invID, invitee); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("double accept should be a no-op, got %v", err)
		}

		// revoke: a fresh pending invite can be revoked, then revoke again is a no-op
		raw2 := "raw-invite-token-two"
		inv2 := &domain.Invitation{OrgID: orgID, Email: "second@inv.test", Role: domain.RoleViewer, TokenHash: crypto.HashToken(raw2)}
		inv2ID, _ := app.CreateInvitation(ctx, inv2)
		if aff, err := app.RevokeInvitation(ctx, orgID, inv2ID); err != nil || aff != 1 {
			t.Fatalf("revoke: aff=%d err=%v", aff, err)
		}
		if aff, _ := app.RevokeInvitation(ctx, orgID, inv2ID); aff != 0 {
			t.Fatalf("re-revoke should affect 0 rows, got %d", aff)
		}

		// expire: a past-due pending invite is flipped by the sweep
		raw3 := "raw-invite-token-three"
		inv3 := &domain.Invitation{
			OrgID: orgID, Email: "third@inv.test", Role: domain.RoleMember,
			TokenHash: crypto.HashToken(raw3), ExpiresAt: time.Now().Add(-time.Hour),
		}
		inv3ID, _ := app.CreateInvitation(ctx, inv3)
		// ExpireInvitations is a cross-org maintenance sweep, so it runs on the
		// privileged pool (it cannot scope to one org under RLS), like the
		// scheduler's cross-org reads.
		n, err := admin.ExpireInvitations(ctx)
		if err != nil || n < 1 {
			t.Fatalf("expire invitations: n=%d err=%v", n, err)
		}
		expired, _ := app.GetInvitationByToken(ctx, crypto.HashToken(raw3))
		if expired.State != domain.InviteExpired {
			t.Fatalf("invite %d should be expired, got %s", inv3ID, expired.State)
		}
	})

	t.Run("refresh_token_rotation_reuse_and_revoke", func(t *testing.T) {
		userID := mkUser(ctx, t, app, "session@rt.test")

		raw1 := "rt-raw-1"
		hash1 := crypto.HashToken(raw1)
		rt := &domain.RefreshToken{UserID: userID, TokenHash: hash1, ExpiresAt: time.Now().Add(30 * 24 * time.Hour)}
		first, err := app.CreateRefreshToken(ctx, rt)
		if err != nil {
			t.Fatalf("create refresh: %v", err)
		}
		if rt.FamilyID != first {
			t.Fatalf("a fresh login should root the family at its own id: family=%d id=%d", rt.FamilyID, first)
		}

		// hashed at rest
		var storedHash string
		if err := admin.QueryRow(ctx, "SELECT token_hash FROM refresh_tokens WHERE id = $1", first).Scan(&storedHash); err != nil {
			t.Fatalf("read raw refresh column: %v", err)
		}
		if storedHash == raw1 {
			t.Fatal("refresh token stored in plaintext")
		}
		if storedHash != hash1 {
			t.Fatalf("stored refresh hash mismatch")
		}

		// rotate: old gets replaced_by, new token is in the same family
		raw2 := "rt-raw-2"
		hash2 := crypto.HashToken(raw2)
		newID, err := app.RotateRefreshToken(ctx, hash1, hash2)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		old, _ := app.GetRefreshTokenByHash(ctx, hash1)
		if old.ReplacedBy == nil || *old.ReplacedBy != newID {
			t.Fatalf("old token replaced_by not set to new id: %+v", old.ReplacedBy)
		}
		newTok, _ := app.GetRefreshTokenByHash(ctx, hash2)
		if newTok.FamilyID != rt.FamilyID {
			t.Fatalf("rotated token must stay in the family: %d != %d", newTok.FamilyID, rt.FamilyID)
		}

		// reuse detection: presenting the already-rotated old token rotates nothing
		if _, err := app.RotateRefreshToken(ctx, hash1, crypto.HashToken("rt-raw-x")); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("re-using a rotated token should rotate nothing, got %v", err)
		}
		// the reuse response is to revoke the whole family
		aff, err := app.RevokeRefreshTokenFamily(ctx, rt.FamilyID)
		if err != nil || aff < 1 {
			t.Fatalf("revoke family: aff=%d err=%v", aff, err)
		}
		after, _ := app.GetRefreshTokenByHash(ctx, hash2)
		if after.RevokedAt == nil {
			t.Fatal("family revoke should revoke the live token too")
		}

		// expired-token sweep deletes a past-due token
		rawExp := "rt-expired"
		expTok := &domain.RefreshToken{UserID: userID, TokenHash: crypto.HashToken(rawExp), ExpiresAt: time.Now().Add(-time.Hour)}
		if _, err := app.CreateRefreshToken(ctx, expTok); err != nil {
			t.Fatalf("create expired token: %v", err)
		}
		n, err := app.DeleteExpiredRefreshTokens(ctx)
		if err != nil || n < 1 {
			t.Fatalf("delete expired: n=%d err=%v", n, err)
		}
		if _, err := app.GetRefreshTokenByHash(ctx, crypto.HashToken(rawExp)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("expired token should be gone, got %v", err)
		}
	})
}

// mkUser and mkOrg are small seed helpers for the sub-tests.
func mkUser(ctx context.Context, t *testing.T, app *store.Pool, email string) int64 {
	t.Helper()
	id, err := app.CreateUser(ctx, &domain.User{Email: email, EmailVerified: true})
	if err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return id
}

func mkOrg(ctx context.Context, t *testing.T, app *store.Pool, slug string) int64 {
	t.Helper()
	id, err := app.CreateOrganization(ctx, &domain.Organization{Name: slug, Slug: slug})
	if err != nil {
		t.Fatalf("seed org %s: %v", slug, err)
	}
	return id
}

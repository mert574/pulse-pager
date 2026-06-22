//go:build integration

// AuthN data-layer integration test. Same testcontainers pattern as
// identity_test.go: start Postgres, apply the schema, then exercise the
// authn-relevant store methods and the authn services that touch the DB against the
// app pool (so RLS is in force). Covers the first-sign-in atomic create, the
// refresh-token rotation/reuse via the RefreshService, and the API-key verify via
// the APIKeyVerifier hitting real rows.
package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"pulse/internal/authn"
	"pulse/internal/crypto"
	"pulse/internal/domain"
	"pulse/internal/store"
)

func TestAuthN(t *testing.T) {
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

	app, err := store.Open(ctx, appDSN)
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	defer app.Close()

	t.Run("first_sign_in_creates_user_org_owner", func(t *testing.T) {
		u := &domain.User{Email: "first@signin.test", EmailVerified: true, Name: "First"}
		idn := &domain.UserIdentity{Provider: domain.ProviderGitHub, ProviderUserID: "gh-first-1"}
		res, err := app.CreateUserWithPersonalOrg(ctx, u, idn, "First's workspace", "first-abc123")
		if err != nil {
			t.Fatalf("first sign-in: %v", err)
		}
		if res.UserID == 0 || res.OrgID == 0 || res.MembershipID == 0 {
			t.Fatalf("first sign-in returned zero ids: %+v", res)
		}
		// the owner membership exists in the new org
		m, err := app.GetMembership(ctx, res.UserID, res.OrgID)
		if err != nil || m.Role != domain.RoleOwner {
			t.Fatalf("owner membership missing: %v err=%v", m, err)
		}
		// the identity resolves a returning login to the same user
		gotIdn, err := app.GetIdentity(ctx, domain.ProviderGitHub, "gh-first-1")
		if err != nil || gotIdn.UserID != res.UserID {
			t.Fatalf("identity not linked: %v err=%v", gotIdn, err)
		}
		// the org is listed for the user
		orgs, _ := app.ListOrganizationsForUser(ctx, res.UserID)
		if len(orgs) != 1 || orgs[0].ID != res.OrgID {
			t.Fatalf("expected the personal org listed, got %v", orgs)
		}
	})

	t.Run("refresh_service_rotation_and_reuse", func(t *testing.T) {
		// seed a user
		uid, err := app.CreateUser(ctx, &domain.User{Email: "rt@authn.test", EmailVerified: true})
		if err != nil {
			t.Fatalf("seed user: %v", err)
		}
		svc := authn.NewRefreshService(app)

		issued, err := svc.Issue(ctx, uid)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		rot, err := svc.Rotate(ctx, issued.Raw)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		if rot.FamilyID != issued.FamilyID || rot.UserID != uid {
			t.Fatalf("rotated token mismatch: %+v", rot)
		}
		// reuse: presenting the original (now rotated) token revokes the family
		if _, err := svc.Rotate(ctx, issued.Raw); !errors.Is(err, authn.ErrReuseDetected) {
			t.Fatalf("reuse should be detected, got %v", err)
		}
		// the live (rotated) token is now revoked, so rotating it is invalid
		if _, err := svc.Rotate(ctx, rot.Raw); !errors.Is(err, authn.ErrRefreshInvalid) {
			t.Fatalf("after family revoke the live token should be invalid, got %v", err)
		}
	})

	t.Run("api_key_verify_valid_revoked_cache", func(t *testing.T) {
		orgID := mkOrg(ctx, t, app, "apikey-org")
		owner := mkUser(ctx, t, app, "apikey-owner@authn.test")
		if _, err := app.CreateMembership(ctx, &domain.Membership{OrgID: orgID, UserID: owner, Role: domain.RoleOwner}); err != nil {
			t.Fatalf("seed owner: %v", err)
		}

		raw := authn.APIKeyPrefix + "integration0000000000abc"
		key := &domain.APIKey{
			OrgID: orgID, Name: "ci key", Prefix: "integ", Role: domain.RoleMember,
			TokenHash: crypto.HashToken(raw), CreatedBy: &owner,
		}
		keyID, err := app.CreateAPIKey(ctx, key)
		if err != nil {
			t.Fatalf("create api key: %v", err)
		}

		// hashed at rest: the stored column is the hash, not the raw secret
		var stored string
		if err := admin.QueryRow(ctx, "SELECT token_hash FROM api_keys WHERE id = $1", keyID).Scan(&stored); err != nil {
			t.Fatalf("read token_hash: %v", err)
		}
		if stored == raw {
			t.Fatal("api key stored in plaintext")
		}

		// verify with no cache (DB-only path): resolves to (org, role)
		v := authn.NewAPIKeyVerifier(app, nil)
		p, err := v.Verify(ctx, raw)
		if err != nil || p.OrgID != orgID || p.Role != domain.RoleMember {
			t.Fatalf("verify valid key: p=%+v err=%v", p, err)
		}

		// revoke then verify fails on the next call
		aff, err := app.RevokeAPIKey(ctx, orgID, keyID)
		if err != nil || aff != 1 {
			t.Fatalf("revoke: aff=%d err=%v", aff, err)
		}
		if _, err := v.Verify(ctx, raw); !errors.Is(err, authn.ErrAPIKeyInvalid) {
			t.Fatalf("revoked key should fail verify, got %v", err)
		}

		// unknown key is invalid (pgx.ErrNoRows under the hood)
		if _, err := v.Verify(ctx, authn.APIKeyPrefix+"unknownkey0000000000000"); !errors.Is(err, authn.ErrAPIKeyInvalid) {
			t.Fatalf("unknown key should fail, got %v", err)
		}
	})
}

package authn

import (
	"context"
	"strings"
	"testing"
	"time"

	"pulse/internal/crypto"
	"pulse/internal/domain"
)

// fakeMagicFlows is an in-memory stand-in for the Redis magic-link store, with the
// single-use GETDEL semantics the real *kv.Client gives: GetDelCache returns the
// value and removes it, so a second read of the same key finds nothing.
type fakeMagicFlows struct {
	data map[string]string
}

func newFakeMagicFlows() *fakeMagicFlows {
	return &fakeMagicFlows{data: map[string]string{}}
}

func (f *fakeMagicFlows) SetCache(_ context.Context, key, value string, _ time.Duration) error {
	f.data[key] = value
	return nil
}

func (f *fakeMagicFlows) GetDelCache(_ context.Context, key string) (string, bool, error) {
	v, ok := f.data[key]
	if !ok {
		return "", false, nil
	}
	delete(f.data, key)
	return v, true, nil
}

func TestMagicLinkStartStoresHashNotRawToken(t *testing.T) {
	ctx := context.Background()
	flows := newFakeMagicFlows()
	svc := NewMagicLinkService(flows, newFakeUserStore())

	raw, err := svc.Start(ctx, "Person@Example.com")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// The raw token must never be the key, and must not appear anywhere in the stored
	// value: only its hash is stored (the key), and the value carries the email.
	if _, ok := flows.data["magiclink:"+raw]; ok {
		t.Fatal("raw token was used as the key; only its hash should be")
	}
	hashKey := "magiclink:" + crypto.HashToken(raw)
	stored, ok := flows.data[hashKey]
	if !ok {
		t.Fatal("the token hash should key the stored record")
	}
	if strings.Contains(stored, raw) {
		t.Fatal("the raw token must not be stored in the record value")
	}
	// The email is normalized (lowercased + trimmed) in the record.
	if !strings.Contains(stored, "person@example.com") {
		t.Fatalf("stored record should carry the normalized email, got %q", stored)
	}
}

func TestMagicLinkVerifyIsSingleUse(t *testing.T) {
	ctx := context.Background()
	flows := newFakeMagicFlows()
	users := newFakeUserStore()
	svc := NewMagicLinkService(flows, users)

	raw, err := svc.Start(ctx, "single@example.com")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// First verify creates the account and succeeds.
	uid, email, isNew, err := svc.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if uid == 0 || email != "single@example.com" || !isNew {
		t.Fatalf("first verify should create a new user, got uid=%d email=%q isNew=%v", uid, email, isNew)
	}
	// Second verify of the same token finds nothing (the record was consumed).
	if _, _, _, err := svc.Verify(ctx, raw); err != ErrMagicLinkInvalid {
		t.Fatalf("replayed token should be invalid, got %v", err)
	}
}

func TestMagicLinkVerifyMissingTokenFails(t *testing.T) {
	ctx := context.Background()
	svc := NewMagicLinkService(newFakeMagicFlows(), newFakeUserStore())

	if _, _, _, err := svc.Verify(ctx, ""); err != ErrMagicLinkInvalid {
		t.Fatalf("empty token should be invalid, got %v", err)
	}
	if _, _, _, err := svc.Verify(ctx, "never-issued"); err != ErrMagicLinkInvalid {
		t.Fatalf("unknown token should be invalid, got %v", err)
	}
}

func TestMagicLinkStartNeutralForAnyEmail(t *testing.T) {
	ctx := context.Background()
	users := newFakeUserStore()
	svc := NewMagicLinkService(newFakeMagicFlows(), users)

	// An unknown email still gets a token (the start handler is enumeration-safe: it
	// behaves the same whether or not the email exists). Start never touches the user
	// store, so there is no signal either way.
	if _, err := svc.Start(ctx, "stranger@example.com"); err != nil {
		t.Fatalf("start for unknown email should succeed: %v", err)
	}
}

func TestMagicLinkVerifyNewEmailCreatesUserAndOrg(t *testing.T) {
	ctx := context.Background()
	flows := newFakeMagicFlows()
	users := newFakeUserStore()
	svc := NewMagicLinkService(flows, users)

	raw, _ := svc.Start(ctx, "new@example.com")
	uid, _, isNew, err := svc.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !isNew {
		t.Fatal("a brand-new email should sign up (isNew true)")
	}
	if _, ok := users.orgs[uid]; !ok {
		t.Fatal("a new user should get a personal org")
	}
	// A passwordless sign-up has no social identity row: the verified email is the
	// sign-in handle, so the user is found by email on the next link.
	if len(users.identities) != 0 {
		t.Fatalf("a magic-link sign-up should not create a social identity, got %v", users.identities)
	}
	if _, err := users.GetUserByEmail(ctx, "new@example.com"); err != nil {
		t.Fatalf("the new user should be findable by email: %v", err)
	}
}

func TestMagicLinkVerifyKnownEmailSignsIntoSameAccount(t *testing.T) {
	ctx := context.Background()
	flows := newFakeMagicFlows()
	users := newFakeUserStore()
	svc := NewMagicLinkService(flows, users)

	// Seed an existing account as if it were created via OAuth (a google identity).
	u := &domain.User{Email: "known@example.com", EmailVerified: true, Name: "Known"}
	idn := &domain.UserIdentity{Provider: domain.ProviderGoogle, ProviderUserID: "g-123"}
	res, err := users.CreateUserWithPersonalOrg(ctx, u, idn, "Known's workspace", "known-abc123")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	raw, _ := svc.Start(ctx, "Known@Example.com") // mixed case normalizes to the same email
	uid, email, isNew, err := svc.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if isNew {
		t.Fatal("a known email should sign into the existing account, not create one")
	}
	if uid != res.UserID {
		t.Fatalf("should sign into the same user: got %d want %d", uid, res.UserID)
	}
	if email != "known@example.com" {
		t.Fatalf("email should be normalized, got %q", email)
	}
	if !users.lastLogin[uid] {
		t.Fatal("the existing-account path should stamp last_login")
	}
}

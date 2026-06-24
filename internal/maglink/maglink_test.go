package maglink

import (
	"context"
	"strings"
	"testing"
	"time"

	"pulse/internal/crypto"
)

// memStore is an in-memory stand-in for the Redis record store with the single-use
// GETDEL semantics the real *kv.Client gives.
type memStore struct{ m map[string]string }

func newMemStore() *memStore { return &memStore{m: map[string]string{}} }

func (s *memStore) SetCache(_ context.Context, k, v string, _ time.Duration) error {
	s.m[k] = v
	return nil
}

func (s *memStore) GetDelCache(_ context.Context, k string) (string, bool, error) {
	v, ok := s.m[k]
	delete(s.m, k)
	return v, ok, nil
}

func TestMintConsumeRoundtrip(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()

	raw, err := Mint(ctx, s, "person@example.com")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// The raw token is never the key, and never stored in the value: only its hash is
	// the key, and the value carries the email.
	if _, ok := s.m["magiclink:"+raw]; ok {
		t.Fatal("raw token was used as the key; only its hash should be")
	}
	stored, ok := s.m["magiclink:"+crypto.HashToken(raw)]
	if !ok {
		t.Fatal("the token hash should key the stored record")
	}
	if strings.Contains(stored, raw) {
		t.Fatal("the raw token must not be stored in the record value")
	}
	if !strings.Contains(stored, "person@example.com") {
		t.Fatalf("stored record should carry the email, got %q", stored)
	}

	email, err := Consume(ctx, s, raw)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if email != "person@example.com" {
		t.Fatalf("consume returned %q", email)
	}
}

func TestConsumeIsSingleUse(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()
	raw, _ := Mint(ctx, s, "a@b.test")

	if _, err := Consume(ctx, s, raw); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := Consume(ctx, s, raw); err != ErrInvalid {
		t.Fatalf("a replayed token should be ErrInvalid, got %v", err)
	}
}

func TestConsumeInvalidTokens(t *testing.T) {
	ctx := context.Background()
	s := newMemStore()

	if _, err := Consume(ctx, s, ""); err != ErrInvalid {
		t.Fatalf("empty token should be ErrInvalid, got %v", err)
	}
	if _, err := Consume(ctx, s, "never-minted"); err != ErrInvalid {
		t.Fatalf("unknown token should be ErrInvalid, got %v", err)
	}
	// A record that does not parse is treated as invalid, not a hard error.
	s.m["magiclink:"+crypto.HashToken("garbage")] = "{not json"
	if _, err := Consume(ctx, s, "garbage"); err != ErrInvalid {
		t.Fatalf("unparseable record should be ErrInvalid, got %v", err)
	}
}

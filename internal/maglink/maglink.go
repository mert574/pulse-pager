// Package maglink owns the short-lived magic-link Redis record: the key shape, the
// stored fields, the TTL, and the mint/consume pair. It is the one contract shared by
// the notifier (which mints the token and writes the record when it sends the email,
// RFC-019 section 5.1) and the api (which consumes it on verify). Keeping it here means
// neither side hard-codes the other's format, so the two cannot drift. It imports only
// internal/crypto, so both the api and the notifier can use it without pulling in authn.
package maglink

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"pulse/internal/crypto"
)

// TTL is how long an emailed link stays valid (RFC-003). Short on purpose: the link is
// single-use, but the TTL caps the window if the mail sits unread.
const TTL = 15 * time.Minute

// key is the Redis key for a token: the magic-link namespace plus the token hash, so
// the raw token is never the key (only its SHA-256 is).
func key(tokenHash string) string { return "magiclink:" + tokenHash }

// record is the per-attempt value stored under key. It carries only the target email
// and when it was made; the raw token is never stored (its hash is the key), matching
// how refresh tokens and invitations store only the hash (RFC-003 5.2).
type record struct {
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// Store is the Redis seam for the short-lived record. *kv.Client satisfies it. The
// record is single-use: Consume reads-and-deletes it so a replay finds nothing.
type Store interface {
	SetCache(ctx context.Context, key, value string, ttl time.Duration) error
	GetDelCache(ctx context.Context, key string) (string, bool, error)
}

// ErrInvalid is returned by Consume when a presented token is unknown, already used,
// expired, or its record does not parse. The caller aborts with no session.
var ErrInvalid = errors.New("magic link is invalid or expired")

// Mint creates a one-time token, stores only its hash with the target email and the
// 15-minute TTL, and returns the raw token for the caller to put in the verify link.
// The email is expected already normalized (trimmed, lowercased) by the caller so the
// record key and the later GetUserByEmail lookup line up.
func Mint(ctx context.Context, s Store, email string) (rawToken string, err error) {
	raw, err := crypto.NewOpaqueToken()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(record{Email: email, CreatedAt: time.Now().UTC()})
	if err != nil {
		return "", err
	}
	if err := s.SetCache(ctx, key(crypto.HashToken(raw)), string(b), TTL); err != nil {
		return "", err
	}
	return raw, nil
}

// Consume hashes the presented token, atomically reads-and-deletes its record (single
// use, so a concurrent verify of the same token cannot also read it), and returns the
// target email. An unknown/used/expired token or an unparseable record is ErrInvalid.
func Consume(ctx context.Context, s Store, rawToken string) (email string, err error) {
	if rawToken == "" {
		return "", ErrInvalid
	}
	raw, ok, err := s.GetDelCache(ctx, key(crypto.HashToken(rawToken)))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrInvalid
	}
	var rec record
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return "", ErrInvalid
	}
	return rec.Email, nil
}

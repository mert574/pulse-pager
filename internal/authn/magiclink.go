package authn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/crypto"
	"pulse/internal/domain"
)

// Magic-link (passwordless) email login (RFC-003). It mirrors the OAuth flow's
// short-lived Redis record, but the proof of ownership is clicking a link sent to
// the email instead of an IdP redirect. Start mints a one-time token, stores only
// its hash in Redis with a short TTL, and hands the raw token back so the caller
// can email the link. Verify hashes the presented token, atomically consumes the
// Redis record (so a replay finds nothing), then resolves to a Pulse user: a
// brand-new email creates a user + personal org, a known email signs into the same
// account. Linking is by verified email, since the click proves ownership.

// magicLinkTTL is how long an emailed link stays valid (RFC-003). Short on purpose:
// the link is single-use, but the TTL caps the window if the mail sits unread.
const magicLinkTTL = 15 * time.Minute

// magicLinkRecord is the per-attempt record stored in Redis, keyed by the token
// hash. It only carries the target email and when it was made; the raw token is
// never stored (only its hash, which is the key), matching how refresh tokens and
// invitations store only crypto.HashToken (RFC-003 5.2).
type magicLinkRecord struct {
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// magicLinkFlowStore is the Redis seam for the short-lived record. *kv.Client
// satisfies it. The record is single-use: Verify consumes it with GetDelCache so a
// second verify finds nothing.
type magicLinkFlowStore interface {
	SetCache(ctx context.Context, key, value string, ttl time.Duration) error
	GetDelCache(ctx context.Context, key string) (string, bool, error)
}

// MagicLinkService orchestrates the passwordless email login. It reuses the same
// userStore seam the OAuth LoginService and dev-login use, so a magic-link sign-in
// finds-or-creates the exact same account.
type MagicLinkService struct {
	flows magicLinkFlowStore
	users userStore
	now   func() time.Time
}

// NewMagicLinkService builds the service from the Redis flow store and the user
// store.
func NewMagicLinkService(flows magicLinkFlowStore, users userStore) *MagicLinkService {
	return &MagicLinkService{flows: flows, users: users, now: time.Now}
}

func magicLinkKey(tokenHash string) string { return "magiclink:" + tokenHash }

// Start mints a fresh one-time token, stores only its hash in Redis with the short
// TTL, and returns the raw token. The caller builds the verify link from it and
// emails it. The email is normalized (trimmed, lowercased) so the same address
// always resolves to the same record and the same account.
func (s *MagicLinkService) Start(ctx context.Context, email string) (rawToken string, err error) {
	email = normalizeEmail(email)
	raw, err := newOpaqueToken()
	if err != nil {
		return "", err
	}
	rec := magicLinkRecord{Email: email, CreatedAt: s.now().UTC()}
	b, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	if err := s.flows.SetCache(ctx, magicLinkKey(crypto.HashToken(raw)), string(b), magicLinkTTL); err != nil {
		return "", err
	}
	return raw, nil
}

// ErrMagicLinkInvalid is returned when a presented token is unknown, already used,
// or expired. The caller aborts the flow with no session (the same dead end the
// OAuth state mismatch produces).
var ErrMagicLinkInvalid = errors.New("magic link is invalid or expired")

// Verify hashes the presented token, atomically consumes the Redis record (single
// use), and resolves to a Pulse user. A brand-new email creates a user + personal
// org (isNew true); a known email, including one created via OAuth, signs into the
// same account (isNew false). It stamps last_login on the existing-account path.
func (s *MagicLinkService) Verify(ctx context.Context, rawToken string) (userID int64, email string, isNew bool, err error) {
	if rawToken == "" {
		return 0, "", false, ErrMagicLinkInvalid
	}
	// Single use: consume the record now. GETDEL is atomic, so a concurrent verify
	// of the same token can't also read it.
	raw, ok, err := s.flows.GetDelCache(ctx, magicLinkKey(crypto.HashToken(rawToken)))
	if err != nil {
		return 0, "", false, err
	}
	if !ok {
		return 0, "", false, ErrMagicLinkInvalid
	}
	var rec magicLinkRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return 0, "", false, ErrMagicLinkInvalid
	}

	// Known email (including an OAuth-created account): sign into the same user.
	existing, err := s.users.GetUserByEmail(ctx, rec.Email)
	if err == nil {
		_ = s.users.SetLastLogin(ctx, existing.ID)
		return existing.ID, rec.Email, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, err
	}

	// New email: create the user + personal org + owner membership atomically. The
	// click proved the email, so the user is verified. No social identity is linked:
	// the verified email is the sign-in handle (a later link for the same email finds
	// this account via GetUserByEmail), so CreateUserWithPersonalOrg is called with a
	// nil identity and skips the user_identities insert.
	u := &domain.User{
		Email:         rec.Email,
		EmailVerified: true,
		Name:          nameFromEmail(rec.Email),
	}
	name, slug := magicLinkOrgNaming(rec.Email)
	res, err := s.users.CreateUserWithPersonalOrg(ctx, u, nil, name, slug)
	if err != nil {
		return 0, "", false, err
	}
	return res.UserID, rec.Email, true, nil
}

// normalizeEmail trims surrounding space and lowercases, so the Redis record key
// and the GetUserByEmail lookup (which is already case-insensitive) line up.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// nameFromEmail derives a display name from the local part when none is known
// (the magic-link sign-up has no profile), matching the dev-login default.
func nameFromEmail(email string) string {
	return strings.Split(email, "@")[0]
}

// magicLinkOrgNaming derives the personal org name and a unique-ish slug, mirroring
// the OAuth first-sign-in naming ("Dev's workspace") with a short random suffix.
func magicLinkOrgNaming(email string) (name, slug string) {
	base := nameFromEmail(email)
	name = base + "'s workspace"
	suffix, _ := newOpaqueToken()
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	return name, slugify(base) + "-" + strings.ToLower(suffix)
}

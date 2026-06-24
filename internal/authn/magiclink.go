package authn

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
	"pulse/internal/maglink"
)

// Magic-link (passwordless) email login (RFC-003). It mirrors the OAuth flow's
// short-lived Redis record, but the proof of ownership is clicking a link sent to
// the email instead of an IdP redirect. The Redis record contract (key shape, fields,
// TTL, mint/consume) lives in internal/maglink, shared with the notifier, which mints
// the token at send time (RFC-019 section 5.1); this service only consumes on verify.
// Verify hashes the presented token, atomically consumes the Redis record (so a replay
// finds nothing), then resolves to a Pulse user: a brand-new email creates a user +
// personal org, a known email signs into the same account. Linking is by verified
// email, since the click proves ownership.

// MagicLinkService orchestrates the passwordless email login. It reuses the same
// userStore seam the OAuth LoginService and dev-login use, so a magic-link sign-in
// finds-or-creates the exact same account.
type MagicLinkService struct {
	flows maglink.Store
	users userStore
}

// NewMagicLinkService builds the service from the Redis flow store and the user
// store.
func NewMagicLinkService(flows maglink.Store, users userStore) *MagicLinkService {
	return &MagicLinkService{flows: flows, users: users}
}

// Start mints a fresh one-time token, stores only its hash in Redis with the short
// TTL, and returns the raw token. The caller builds the verify link from it and emails
// it. The email is normalized (trimmed, lowercased) so the same address always
// resolves to the same record and the same account. RFC-019 moves real sending to the
// notifier; this stays for the dev/test path and any caller that still mints inline.
func (s *MagicLinkService) Start(ctx context.Context, email string) (rawToken string, err error) {
	return maglink.Mint(ctx, s.flows, normalizeEmail(email))
}

// ErrMagicLinkInvalid is returned when a presented token is unknown, already used,
// or expired. The caller aborts the flow with no session (the same dead end the
// OAuth state mismatch produces).
var ErrMagicLinkInvalid = errors.New("magic link is invalid or expired")

// Verify consumes the one-time Redis record for the presented token (single use) and
// resolves to a Pulse user. A brand-new email creates a user + personal org (isNew
// true); a known email, including one created via OAuth, signs into the same account
// (isNew false). It stamps last_login on the existing-account path. An unknown/used/
// expired token is ErrMagicLinkInvalid; a store failure is surfaced as-is.
func (s *MagicLinkService) Verify(ctx context.Context, rawToken string) (userID int64, email string, isNew bool, err error) {
	email, err = maglink.Consume(ctx, s.flows, rawToken)
	if err != nil {
		if errors.Is(err, maglink.ErrInvalid) {
			return 0, "", false, ErrMagicLinkInvalid
		}
		return 0, "", false, err
	}

	// Known email (including an OAuth-created account): sign into the same user.
	existing, err := s.users.GetUserByEmail(ctx, email)
	if err == nil {
		_ = s.users.SetLastLogin(ctx, existing.ID)
		return existing.ID, email, false, nil
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
		Email:         email,
		EmailVerified: true,
		Name:          nameFromEmail(email),
	}
	name, slug := magicLinkOrgNaming(email)
	res, err := s.users.CreateUserWithPersonalOrg(ctx, u, nil, name, slug)
	if err != nil {
		return 0, "", false, err
	}
	return res.UserID, email, true, nil
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

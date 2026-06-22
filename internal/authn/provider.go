package authn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"

	"pulse/internal/domain"
)

// Provider is the small seam every social/SSO login plugs into (RFC-003 2.1, 2.7).
// Google (OIDC) and GitHub (OAuth2) implement it; enterprise SSO (RFC-016) adds
// more later without touching the login/callback orchestration. A provider builds
// the authorize redirect and, given a callback code, returns the verified profile.
type Provider interface {
	// Name is the provider key used in the URL (/auth/{name}/login) and stored as
	// the identity provider.
	Name() domain.IdentityProvider

	// AuthCodeURL builds the authorize redirect for an attempt. state is the CSRF
	// token, nonce is the OIDC replay nonce (ignored by providers without an ID
	// token), and challenge is the PKCE code challenge.
	AuthCodeURL(state, nonce, challenge string) string

	// Exchange handles the callback: it swaps the code (with the PKCE verifier) for
	// tokens and returns the verified profile. For OIDC it verifies the ID token
	// signature and that the nonce matches. It refuses an unverified email
	// (RFC-003 2.1): a profile is returned only when the email is verified.
	Exchange(ctx context.Context, code, verifier, nonce string) (*Profile, error)
}

// Profile is the verified provider profile used to resolve a Pulse user (RFC-003
// 2.3). Email is always verified by the time it is here; the orchestration never
// sees an unverified email.
type Profile struct {
	Provider       domain.IdentityProvider
	ProviderUserID string // stable subject id (Google sub, GitHub numeric id)
	Email          string // verified
	Name           string
	AvatarURL      string
}

// pkceChallenge returns base64url(sha256(verifier)), the S256 PKCE challenge
// (RFC-003 2.2). The verifier is a high-entropy opaque token.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

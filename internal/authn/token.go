package authn

import (
	"crypto/rand"
	"encoding/base64"
)

// newOpaqueToken returns 32 random bytes, base64url-encoded. This is the v1
// newToken() generator carried forward (RFC-003 1.4): it makes the refresh-token
// secret, the API-key secret body, the OAuth state/nonce, and the JWT jti. The
// caller stores only the SHA-256 hash of secrets (crypto.HashToken); the raw value
// is handed to the client and never persisted in clear.
func newOpaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewCSRFToken mints a fresh CSRF token for the double-submit cookie (RFC-003 4.5).
// It is exported so the HTTP edge can set the pulse_csrf cookie on login/refresh.
func NewCSRFToken() (string, error) {
	return newOpaqueToken()
}

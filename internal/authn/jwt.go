package authn

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// JWT (RS256) issue and verify, plus the JWKS endpoint payload (RFC-003 section 3).
//
// Library choice: github.com/golang-jwt/jwt/v5. It is the most widely used Go JWT
// library, signs/verifies RS256 against the stdlib crypto/rsa keys we already load
// from PEM, and its RegisteredClaims cover iss/aud/sub/iat/exp/jti directly. We do
// not need the broader jwx feature set (JWE, full JWK marshaling), and building the
// small JWKS document by hand from the rsa.PublicKey keeps the dependency surface
// minimal. So golang-jwt over lestrrat-go/jwx.

const (
	tokenUseAccess = "access"
	accessTTL      = 15 * time.Minute
)

// accessClaims are the access-token claims (RFC-003 3.1). There is deliberately no
// org, no role, and no scope claim: the token answers only "who is this person".
// The org comes from the request and the role from a fresh membership lookup on
// every request (RFC-003 3.2).
type accessClaims struct {
	Email    string `json:"email"`
	TokenUse string `json:"token_use"`
	jwt.RegisteredClaims
}

// SigningKey holds the RS256 keypair api signs with, plus its kid. The private key
// lives only in api (RFC-003 3.4); the public half is published in JWKS by kid so a
// verifier picks the right key and rotation works.
type SigningKey struct {
	kid  string
	priv *rsa.PrivateKey
}

// NewSigningKey loads an RSA private key from a PEM string (PKCS#1 or PKCS#8) and
// pairs it with a kid. main loads the PEM from config/env (a KMS-backed secret in
// production, RFC-003 3.4). The kid lets JWKS publish the right key for rotation.
func NewSigningKey(kid, pemKey string) (*SigningKey, error) {
	if kid == "" {
		return nil, errors.New("signing key id (kid) is empty")
	}
	priv, err := parseRSAPrivateKey([]byte(pemKey))
	if err != nil {
		return nil, err
	}
	return &SigningKey{kid: kid, priv: priv}, nil
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("signing key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("signing key is not a PKCS#1 or PKCS#8 RSA key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("signing key is not RSA")
	}
	return rsaKey, nil
}

// GenerateSigningKey makes a fresh RSA-2048 keypair with the given kid. Useful for
// dev and tests; production loads a KMS-backed key via NewSigningKey.
func GenerateSigningKey(kid string) (*SigningKey, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return &SigningKey{kid: kid, priv: priv}, nil
}

// JWTIssuer signs access tokens and verifies them against the public key set. It
// holds the current signing key and any retired-but-still-valid public keys so a
// rotation overlap verifies tokens signed by either (RFC-003 3.4).
type JWTIssuer struct {
	issuer   string
	audience string
	signing  *SigningKey
	// verifyKeys maps kid -> public key, including the current signing key's. During
	// a rotation overlap an outgoing public key stays here until its last token expires.
	verifyKeys map[string]*rsa.PublicKey
	now        func() time.Time // injectable clock for tests
}

// NewJWTIssuer builds an issuer that signs with signing and verifies against it.
// Extra retired public keys can be registered for a rotation overlap via AddVerifyKey.
func NewJWTIssuer(issuer, audience string, signing *SigningKey) *JWTIssuer {
	return &JWTIssuer{
		issuer:     issuer,
		audience:   audience,
		signing:    signing,
		verifyKeys: map[string]*rsa.PublicKey{signing.kid: &signing.priv.PublicKey},
		now:        time.Now,
	}
}

// AddVerifyKey registers an extra public key (by kid) accepted on verify. Used to
// publish/accept a retired key during a rotation overlap (RFC-003 3.4).
func (j *JWTIssuer) AddVerifyKey(kid string, pub *rsa.PublicKey) {
	j.verifyKeys[kid] = pub
}

// Issue signs an access token for a user (RFC-003 3.1). It returns the compact JWT
// and the expiry so the caller can set the cookie MaxAge.
func (j *JWTIssuer) Issue(userID int64, email string) (string, time.Time, error) {
	now := j.now()
	exp := now.Add(accessTTL)
	jti, err := newOpaqueToken()
	if err != nil {
		return "", time.Time{}, err
	}
	claims := accessClaims{
		Email:    email,
		TokenUse: tokenUseAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    j.issuer,
			Audience:  jwt.ClaimStrings{j.audience},
			Subject:   fmt.Sprintf("%d", userID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        jti,
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = j.signing.kid
	signed, err := tok.SignedString(j.signing.priv)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, exp, nil
}

// VerifiedToken is what Verify returns: the resolved user id and email from a valid
// access token. The org and role are NOT here; they come from the request and a
// fresh membership lookup (RFC-003 3.2).
type VerifiedToken struct {
	UserID int64
	Email  string
}

// Verify checks an access token: RS256 signature against the JWKS key named by the
// header kid, plus iss, aud, exp, and token_use=access (RFC-003 3.1, 6.1). Any
// failure (bad signature, wrong kid, expired, wrong issuer/audience, a refresh-use
// token replayed as access) returns an error and no principal.
func (j *JWTIssuer) Verify(raw string) (*VerifiedToken, error) {
	var claims accessClaims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %q", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		pub, ok := j.verifyKeys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown signing key id %q", kid)
		}
		return pub, nil
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(j.issuer),
		jwt.WithAudience(j.audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(j.now),
	)
	if err != nil {
		return nil, err
	}
	if claims.TokenUse != tokenUseAccess {
		return nil, fmt.Errorf("token_use is %q, want %q", claims.TokenUse, tokenUseAccess)
	}
	var userID int64
	if _, err := fmt.Sscanf(claims.Subject, "%d", &userID); err != nil || userID == 0 {
		return nil, errors.New("token subject is not a valid user id")
	}
	return &VerifiedToken{UserID: userID, Email: claims.Email}, nil
}

// JWKS returns the public key set as the JSON bytes served at
// /.well-known/jwks.json (RFC-003 3.5). It publishes every verify key by kid so a
// rotated key is picked up; each entry is an RSA key with use=sig, alg=RS256.
func (j *JWTIssuer) JWKS() ([]byte, error) {
	type jwk struct {
		Kty string `json:"kty"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	var keys []jwk
	for kid, pub := range j.verifyKeys {
		keys = append(keys, jwk{
			Kty: "RSA",
			Use: "sig",
			Alg: "RS256",
			Kid: kid,
			N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		})
	}
	return json.Marshal(map[string]any{"keys": keys})
}

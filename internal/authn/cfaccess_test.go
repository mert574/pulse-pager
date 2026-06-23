package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const cfTestKID = "test-kid"

// cfTestServer serves a JWKS with one RSA key and returns a verifier wired to it.
func cfTestServer(t *testing.T, key *rsa.PrivateKey, now time.Time) (*CFAccessVerifier, func()) {
	t.Helper()
	jwks := map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"kid": cfTestKID,
			"alg": "RS256",
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	v := NewCFAccessVerifier("team.example.cloudflareaccess.com", "test-aud")
	v.certsURL = srv.URL + "/cdn-cgi/access/certs"
	v.now = func() time.Time { return now }
	return v, srv.Close
}

func mintCFToken(t *testing.T, key *rsa.PrivateKey, claims cfClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = cfTestKID
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func TestCFAccessVerify(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	v, closeFn := cfTestServer(t, key, now)
	defer closeFn()
	ctx := context.Background()

	good := cfClaims{
		Email: "Operator@Example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    v.issuer,
			Audience:  jwt.ClaimStrings{"test-aud"},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		},
	}

	t.Run("valid token returns the lowercased email", func(t *testing.T) {
		email, err := v.Verify(ctx, mintCFToken(t, key, good))
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if email != "operator@example.com" {
			t.Errorf("email = %q, want operator@example.com", email)
		}
	})

	t.Run("wrong audience is rejected", func(t *testing.T) {
		bad := good
		bad.Audience = jwt.ClaimStrings{"some-other-app"}
		if _, err := v.Verify(ctx, mintCFToken(t, key, bad)); err == nil {
			t.Error("expected an error for the wrong audience")
		}
	})

	t.Run("wrong issuer is rejected", func(t *testing.T) {
		bad := good
		bad.Issuer = "https://evil.example"
		if _, err := v.Verify(ctx, mintCFToken(t, key, bad)); err == nil {
			t.Error("expected an error for the wrong issuer")
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		bad := good
		bad.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Hour))
		if _, err := v.Verify(ctx, mintCFToken(t, key, bad)); err == nil {
			t.Error("expected an error for the expired token")
		}
	})

	t.Run("token signed by a different key is rejected", func(t *testing.T) {
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(ctx, mintCFToken(t, other, good)); err == nil {
			t.Error("expected an error for a token signed by an untrusted key")
		}
	})

	t.Run("empty token is rejected", func(t *testing.T) {
		if _, err := v.Verify(ctx, ""); err == nil {
			t.Error("expected an error for an empty token")
		}
	})
}

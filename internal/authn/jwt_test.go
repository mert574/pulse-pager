package authn

import (
	"encoding/json"
	"testing"
	"time"
)

func newTestIssuer(t *testing.T) *JWTIssuer {
	t.Helper()
	sk, err := GenerateSigningKey("kid-1")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return NewJWTIssuer("https://api.pulsepager.com", "pulse-api", sk)
}

func TestJWTRoundTrip(t *testing.T) {
	iss := newTestIssuer(t)
	tok, exp, err := iss.Issue(42, "alice@example.com")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !exp.After(time.Now()) {
		t.Fatalf("exp should be in the future: %v", exp)
	}
	vt, err := iss.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if vt.UserID != 42 || vt.Email != "alice@example.com" {
		t.Fatalf("unexpected verified token: %+v", vt)
	}
}

func TestJWTExpiredRejected(t *testing.T) {
	iss := newTestIssuer(t)
	// issue with a clock far in the past so the token is already expired
	iss.now = func() time.Time { return time.Now().Add(-1 * time.Hour) }
	tok, _, err := iss.Issue(7, "old@example.com")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// verify with the real clock
	iss.now = time.Now
	if _, err := iss.Verify(tok); err == nil {
		t.Fatal("expired token should be rejected")
	}
}

func TestJWTTamperedRejected(t *testing.T) {
	iss := newTestIssuer(t)
	tok, _, _ := iss.Issue(1, "x@example.com")
	// flip a character in the signature segment
	bad := tok[:len(tok)-3] + "AAA"
	if _, err := iss.Verify(bad); err == nil {
		t.Fatal("tampered token should be rejected")
	}
}

func TestJWTWrongIssuerKeyRejected(t *testing.T) {
	a := newTestIssuer(t)
	other := newTestIssuer(t) // a different keypair, same kid
	tok, _, _ := other.Issue(5, "y@example.com")
	if _, err := a.Verify(tok); err == nil {
		t.Fatal("token signed by an unknown key must be rejected")
	}
}

func TestJWKSExposesKid(t *testing.T) {
	iss := newTestIssuer(t)
	raw, err := iss.JWKS()
	if err != nil {
		t.Fatalf("jwks: %v", err)
	}
	var doc struct {
		Keys []struct {
			Kty, Use, Alg, Kid, N, E string
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal jwks: %v", err)
	}
	if len(doc.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(doc.Keys))
	}
	k := doc.Keys[0]
	if k.Kid != "kid-1" || k.Kty != "RSA" || k.Alg != "RS256" || k.Use != "sig" {
		t.Fatalf("unexpected jwk: %+v", k)
	}
	if k.N == "" || k.E == "" {
		t.Fatal("jwk modulus/exponent must be present")
	}
}

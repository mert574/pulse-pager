package authn

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// CFAccessHeader is the request header Cloudflare Access sets to the signed identity
// JWT once a request has passed the Access policy.
const CFAccessHeader = "Cf-Access-Jwt-Assertion"

// CFAccessVerifier verifies the Cloudflare Access application token. It is how the
// operator admin origin (admin.pulsepager.com) authenticates: CF Access logs the
// user in at the edge and forwards a signed JWT, which we verify against the team's
// published keys and read the email claim from. The email is then checked against
// the platform admin allowlist by the caller. We always verify the signature: the
// header on its own is spoofable, the signed token is not.
type CFAccessVerifier struct {
	issuer   string // https://<team>.cloudflareaccess.com
	aud      string // the Access application AUD tag
	certsURL string
	httpc    *http.Client
	now      func() time.Time
	ttl      time.Duration

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// NewCFAccessVerifier builds a verifier for a team domain and application audience.
// teamDomain may be given with or without scheme; it is normalized to an https
// issuer (Cloudflare Access tokens are issued by https://<team>.cloudflareaccess.com).
func NewCFAccessVerifier(teamDomain, aud string) *CFAccessVerifier {
	host := strings.TrimSuffix(teamDomain, "/")
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	issuer := "https://" + host
	return &CFAccessVerifier{
		issuer:   issuer,
		aud:      aud,
		certsURL: issuer + "/cdn-cgi/access/certs",
		httpc:    &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
		ttl:      time.Hour,
	}
}

// cfClaims is the Access token: the email claim plus the registered claims.
type cfClaims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

// Verify validates the Access JWT and returns the authenticated email, lowercased.
// It checks the RS256 signature against the team JWKS (selected by kid), the issuer,
// the audience, and expiry.
func (v *CFAccessVerifier) Verify(ctx context.Context, raw string) (string, error) {
	if raw == "" {
		return "", errors.New("no cloudflare access token")
	}
	var claims cfClaims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %q", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("access token missing kid")
		}
		return v.keyByKID(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.aud),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return "", err
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" {
		return "", errors.New("access token has no email claim")
	}
	return email, nil
}

// keyByKID returns the RSA public key for a kid, refreshing the JWKS cache when the
// kid is unknown or the cache is stale. A transient refresh failure still serves a
// cached key if we already trust that kid.
func (v *CFAccessVerifier) keyByKID(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	key, ok := v.keys[kid]
	fresh := !v.fetchedAt.IsZero() && v.now().Sub(v.fetchedAt) < v.ttl
	v.mu.RUnlock()
	if ok && fresh {
		return key, nil
	}
	if err := v.refresh(ctx); err != nil {
		v.mu.RLock()
		key, ok = v.keys[kid]
		v.mu.RUnlock()
		if ok {
			return key, nil
		}
		return nil, err
	}
	v.mu.RLock()
	key, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no cloudflare access key for kid %q", kid)
	}
	return key, nil
}

func (v *CFAccessVerifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certsURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cloudflare access certs: status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := jwkToRSA(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("cloudflare access certs: no usable RSA keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = v.now()
	v.mu.Unlock()
	return nil
}

// jwkToRSA builds an rsa.PublicKey from the base64url modulus and exponent of a JWK.
func jwkToRSA(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, errors.New("invalid jwk exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}

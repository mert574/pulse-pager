package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
	"pulse/internal/store"
)

// --- fake user store implementing userStore ---

type fakeUserStore struct {
	users      map[int64]*domain.User
	byEmail    map[string]int64
	identities map[string]int64 // "provider|subject" -> userID
	orgs       map[int64]int64  // userID -> personal orgID
	nextUser   int64
	nextOrg    int64
	lastLogin  map[int64]bool
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		users:      map[int64]*domain.User{},
		byEmail:    map[string]int64{},
		identities: map[string]int64{},
		orgs:       map[int64]int64{},
		lastLogin:  map[int64]bool{},
	}
}

func identityKey(p domain.IdentityProvider, subject string) string {
	return string(p) + "|" + subject
}

func (f *fakeUserStore) GetIdentity(_ context.Context, p domain.IdentityProvider, subject string) (*domain.UserIdentity, error) {
	uid, ok := f.identities[identityKey(p, subject)]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return &domain.UserIdentity{UserID: uid, Provider: p, ProviderUserID: subject}, nil
}

func (f *fakeUserStore) GetUserByEmail(_ context.Context, email string) (*domain.User, error) {
	uid, ok := f.byEmail[email]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return f.users[uid], nil
}

func (f *fakeUserStore) LinkIdentity(_ context.Context, idn *domain.UserIdentity) (int64, error) {
	key := identityKey(idn.Provider, idn.ProviderUserID)
	if existing, ok := f.identities[key]; ok && existing != idn.UserID {
		return 0, errors.New("identity already linked to another user (I5)")
	}
	f.identities[key] = idn.UserID
	return 1, nil
}

func (f *fakeUserStore) CreateUserWithPersonalOrg(_ context.Context, u *domain.User, idn *domain.UserIdentity, _, _ string) (*store.FirstSignIn, error) {
	f.nextUser++
	uid := f.nextUser
	f.nextOrg++
	orgID := f.nextOrg
	u.ID = uid
	f.users[uid] = u
	f.byEmail[u.Email] = uid
	if idn != nil {
		f.identities[identityKey(idn.Provider, idn.ProviderUserID)] = uid
	}
	f.orgs[uid] = orgID
	return &store.FirstSignIn{UserID: uid, OrgID: orgID, MembershipID: 1, IdentityID: 1}, nil
}

func (f *fakeUserStore) SetLastLogin(_ context.Context, userID int64) error {
	f.lastLogin[userID] = true
	return nil
}

// --- fake Google OIDC IdP ---

type fakeOIDC struct {
	server *httptest.Server
	priv   *rsa.PrivateKey
	kid    string
	// configured callback profile to mint into the id token
	sub           string
	email         string
	emailVerified bool
	name          string
	// recorded authorize params
	lastNonce string
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDC{priv: priv, kid: "fake-oidc-1", emailVerified: true}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		base := f.server.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                base,
			"authorization_endpoint":                base + "/authorize",
			"token_endpoint":                        base + "/token",
			"jwks_uri":                              base + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		pub := f.priv.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": f.kid,
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		// record the nonce so the minted id token echoes it
		f.lastNonce = r.URL.Query().Get("nonce")
		// in a real flow the user consents and the IdP redirects with a code; the
		// test calls the token endpoint directly, so just echo back.
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		idToken := f.mintIDToken(t, "fake-client", f.lastNonce)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOIDC) mintIDToken(t *testing.T, aud, nonce string) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"iss":            f.server.URL,
		"aud":            aud,
		"sub":            f.sub,
		"email":          f.email,
		"email_verified": f.emailVerified,
		"name":           f.name,
		"nonce":          nonce,
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = f.kid
	signed, err := tok.SignedString(f.priv)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// buildGoogle wires the real googleProvider against the fake IdP.
func buildGoogle(t *testing.T, idp *fakeOIDC) Provider {
	t.Helper()
	p, err := NewGoogleProvider(context.Background(), OIDCConfig{
		IssuerURL:    idp.server.URL,
		ClientID:     "fake-client",
		ClientSecret: "fake-secret",
		RedirectURL:  "https://api.pulse.app/auth/google/callback",
	})
	if err != nil {
		t.Fatalf("build google provider: %v", err)
	}
	return p
}

// runFlow drives StartLogin then HandleCallback, simulating the browser carrying
// the state cookie back. It posts to the IdP token endpoint via the provider's
// Exchange. code is irrelevant to the fake token endpoint.
func runFlow(t *testing.T, svc *LoginService, provider domain.IdentityProvider, idp *fakeOIDC) (*CallbackResult, error) {
	t.Helper()
	ctx := context.Background()
	state, redirect, err := svc.StartLogin(ctx, provider, FlowLogin, "/onboarding", 0)
	if err != nil {
		t.Fatalf("start login: %v", err)
	}
	// hit the IdP authorize so the fake records the nonce it must echo
	u, _ := url.Parse(redirect)
	resp, err := http.Get(idp.server.URL + "/authorize?" + u.RawQuery)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	// the browser returns to the callback with the same state in cookie and query
	return svc.HandleCallback(ctx, state, state, "any-code")
}

func TestGoogleFirstLoginCreatesUserAndOrg(t *testing.T) {
	idp := newFakeOIDC(t)
	idp.sub = "google-sub-1"
	idp.email = "newuser@example.com"
	idp.name = "New User"

	users := newFakeUserStore()
	svc := NewLoginService([]Provider{buildGoogle(t, idp)}, newFakeCache(), users)

	res, err := runFlow(t, svc, domain.ProviderGoogle, idp)
	if err != nil {
		t.Fatalf("flow: %v", err)
	}
	if !res.IsNew {
		t.Fatal("first login should create a new user")
	}
	if res.OrgID == 0 {
		t.Fatal("first login should create a personal org")
	}
	if _, ok := users.users[res.UserID]; !ok {
		t.Fatal("user not stored")
	}
	if users.orgs[res.UserID] == 0 {
		t.Fatal("personal org not recorded for the user")
	}
}

func TestGoogleSecondLoginSameUser(t *testing.T) {
	idp := newFakeOIDC(t)
	idp.sub = "google-sub-2"
	idp.email = "returning@example.com"
	idp.name = "Returning"

	users := newFakeUserStore()
	svc := NewLoginService([]Provider{buildGoogle(t, idp)}, newFakeCache(), users)

	first, err := runFlow(t, svc, domain.ProviderGoogle, idp)
	if err != nil {
		t.Fatalf("first flow: %v", err)
	}
	second, err := runFlow(t, svc, domain.ProviderGoogle, idp)
	if err != nil {
		t.Fatalf("second flow: %v", err)
	}
	if second.IsNew {
		t.Fatal("second login must not create a new user")
	}
	if second.UserID != first.UserID {
		t.Fatalf("second login returned a different user: %d != %d", second.UserID, first.UserID)
	}
}

func TestSecondProviderSameEmailLinks(t *testing.T) {
	// user signs in with Google first
	gIDP := newFakeOIDC(t)
	gIDP.sub = "g-link"
	gIDP.email = "shared@example.com"
	gIDP.name = "Shared"
	users := newFakeUserStore()
	gsvc := NewLoginService([]Provider{buildGoogle(t, gIDP)}, newFakeCache(), users)
	first, err := runFlow(t, gsvc, domain.ProviderGoogle, gIDP)
	if err != nil {
		t.Fatalf("google flow: %v", err)
	}

	// then signs in with GitHub returning the SAME verified email -> auto-link
	gh := newFakeGitHub(t)
	gh.id = 9988
	gh.login = "sharedhub"
	gh.emails = []ghEmail{{Email: "shared@example.com", Primary: true, Verified: true}}
	ghProvider := buildGitHub(t, gh)
	ghsvc := NewLoginService([]Provider{ghProvider}, newFakeCache(), users)

	res, err := runGitHubFlow(t, ghsvc, gh)
	if err != nil {
		t.Fatalf("github flow: %v", err)
	}
	if res.IsNew {
		t.Fatal("matching verified email should link, not create a new user")
	}
	if res.UserID != first.UserID {
		t.Fatalf("github login should resolve to the same user: %d != %d", res.UserID, first.UserID)
	}
	// both identities now map to the one user
	if users.identities[identityKey(domain.ProviderGoogle, "g-link")] != first.UserID ||
		users.identities[identityKey(domain.ProviderGitHub, "9988")] != first.UserID {
		t.Fatal("both provider identities should map to the one user")
	}
}

func TestStateMismatchRejected(t *testing.T) {
	idp := newFakeOIDC(t)
	idp.sub = "sub-x"
	idp.email = "x@example.com"
	users := newFakeUserStore()
	svc := NewLoginService([]Provider{buildGoogle(t, idp)}, newFakeCache(), users)

	ctx := context.Background()
	state, _, _ := svc.StartLogin(ctx, domain.ProviderGoogle, FlowLogin, "/", 0)
	// cookie state != query state
	if _, err := svc.HandleCallback(ctx, state, "different-state", "code"); err != ErrStateMismatch {
		t.Fatalf("mismatched state should be rejected, got %v", err)
	}
	// unknown state (never started / expired)
	if _, err := svc.HandleCallback(ctx, "ghost", "ghost", "code"); err != ErrStateMismatch {
		t.Fatalf("unknown state should be rejected, got %v", err)
	}
}

func TestNonceMismatchRejected(t *testing.T) {
	idp := newFakeOIDC(t)
	idp.sub = "sub-n"
	idp.email = "n@example.com"
	users := newFakeUserStore()
	svc := NewLoginService([]Provider{buildGoogle(t, idp)}, newFakeCache(), users)

	ctx := context.Background()
	state, redirect, _ := svc.StartLogin(ctx, domain.ProviderGoogle, FlowLogin, "/", 0)
	u, _ := url.Parse(redirect)
	// hit authorize so the IdP records the real nonce, then corrupt it so the minted
	// id token echoes a wrong nonce
	resp, _ := http.Get(idp.server.URL + "/authorize?" + u.RawQuery)
	_ = resp.Body.Close()
	idp.lastNonce = "tampered-nonce"

	if _, err := svc.HandleCallback(ctx, state, state, "code"); err == nil {
		t.Fatal("nonce mismatch should be rejected")
	}
}

func TestUnverifiedEmailRefused(t *testing.T) {
	idp := newFakeOIDC(t)
	idp.sub = "sub-u"
	idp.email = "unverified@example.com"
	idp.emailVerified = false // the provider says the email is not verified
	users := newFakeUserStore()
	svc := NewLoginService([]Provider{buildGoogle(t, idp)}, newFakeCache(), users)

	if _, err := runFlow(t, svc, domain.ProviderGoogle, idp); !errors.Is(err, ErrEmailNotVerified) {
		t.Fatalf("unverified email should refuse sign-in, got %v", err)
	}
	if len(users.users) != 0 {
		t.Fatal("no user should be created for an unverified email")
	}
}

// --- fake GitHub ---

type ghEmail struct {
	Email    string
	Primary  bool
	Verified bool
}

type fakeGitHub struct {
	server *httptest.Server
	id     int64
	login  string
	name   string
	emails []ghEmail
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "gho_fake", "token_type": "bearer", "scope": "read:user,user:email",
		})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": g.id, "login": g.login, "name": g.name, "avatar_url": "https://avatars/x",
		})
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, _ *http.Request) {
		out := make([]map[string]any, 0, len(g.emails))
		for _, e := range g.emails {
			out = append(out, map[string]any{"email": e.Email, "primary": e.Primary, "verified": e.Verified})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

func buildGitHub(t *testing.T, g *fakeGitHub) Provider {
	t.Helper()
	return NewGitHubProvider(OAuth2Config{
		ClientID:     "gh-client",
		ClientSecret: "gh-secret",
		RedirectURL:  "https://api.pulse.app/auth/github/callback",
		APIBaseURL:   g.server.URL,
		AuthURL:      g.server.URL + "/login/oauth/authorize",
		TokenURL:     g.server.URL + "/login/oauth/access_token",
	})
}

func runGitHubFlow(t *testing.T, svc *LoginService, _ *fakeGitHub) (*CallbackResult, error) {
	t.Helper()
	ctx := context.Background()
	state, _, err := svc.StartLogin(ctx, domain.ProviderGitHub, FlowLogin, "/", 0)
	if err != nil {
		t.Fatalf("start github login: %v", err)
	}
	return svc.HandleCallback(ctx, state, state, "gh-code")
}

func TestGitHubFirstLoginCreatesUser(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.id = 12345
	gh.login = "octocat"
	gh.name = "Octo Cat"
	gh.emails = []ghEmail{
		{Email: "secondary@example.com", Primary: false, Verified: false},
		{Email: "octo@example.com", Primary: true, Verified: true},
	}
	users := newFakeUserStore()
	svc := NewLoginService([]Provider{buildGitHub(t, gh)}, newFakeCache(), users)

	res, err := runGitHubFlow(t, svc, gh)
	if err != nil {
		t.Fatalf("github flow: %v", err)
	}
	if !res.IsNew {
		t.Fatal("first github login should create a user")
	}
	if users.users[res.UserID].Email != "octo@example.com" {
		t.Fatalf("should pick the primary+verified email, got %q", users.users[res.UserID].Email)
	}
}

func TestGitHubNoVerifiedEmailRefused(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.id = 999
	gh.login = "noverify"
	gh.emails = []ghEmail{{Email: "x@example.com", Primary: true, Verified: false}}
	users := newFakeUserStore()
	svc := NewLoginService([]Provider{buildGitHub(t, gh)}, newFakeCache(), users)

	if _, err := runGitHubFlow(t, svc, gh); !errors.Is(err, ErrEmailNotVerified) {
		t.Fatalf("no verified email should refuse, got %v", err)
	}
}

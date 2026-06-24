package authn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"

	"pulse/internal/domain"
)

// GitHubProvider is the GitHub OAuth2 provider (RFC-003 2.1). GitHub has no OIDC ID
// token, so the verified email is not in the token: after the code exchange we call
// GET /user (profile + stable numeric id) and GET /user/emails and pick the entry
// with primary=true AND verified=true. If none is verified, sign-in is refused.

// OAuth2Config is the GitHub provider config. APIBaseURL and the endpoint URLs are
// overridable so tests point them at a fake GitHub; empty values use the real
// github.com/api.github.com defaults.
type OAuth2Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// APIBaseURL is the base for the user/emails API (default https://api.github.com).
	APIBaseURL string
	// AuthURL and TokenURL override the OAuth endpoints (default github.com). Used by tests.
	AuthURL  string
	TokenURL string
}

type githubProvider struct {
	oauth      *oauth2.Config
	apiBaseURL string
	httpClient *http.Client
}

const defaultGitHubAPIBase = "https://api.github.com"

// NewGitHubProvider builds the GitHub provider from config (RFC-003 8.1).
func NewGitHubProvider(cfg OAuth2Config) Provider {
	endpoint := githuboauth.Endpoint
	if cfg.AuthURL != "" {
		endpoint.AuthURL = cfg.AuthURL
	}
	if cfg.TokenURL != "" {
		endpoint.TokenURL = cfg.TokenURL
	}
	apiBase := cfg.APIBaseURL
	if apiBase == "" {
		apiBase = defaultGitHubAPIBase
	}
	return &githubProvider{
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     endpoint,
			Scopes:       []string{"read:user", "user:email"},
		},
		apiBaseURL: apiBase,
		httpClient: http.DefaultClient,
	}
}

func (g *githubProvider) Name() domain.IdentityProvider { return domain.ProviderGitHub }

func (g *githubProvider) AuthCodeURL(state, _, challenge string) string {
	// GitHub has no OIDC nonce; PKCE still adds code-interception protection.
	return g.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

func (g *githubProvider) Exchange(ctx context.Context, code, verifier, _ string) (*Profile, error) {
	tok, err := g.oauth.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", verifier),
	)
	if err != nil {
		return nil, fmt.Errorf("github token exchange: %w", err)
	}

	client := g.oauth.Client(context.WithValue(ctx, oauth2.HTTPClient, g.httpClient), tok)

	var user struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := getJSON(ctx, client, g.apiBaseURL+"/user", &user); err != nil {
		return nil, fmt.Errorf("github /user: %w", err)
	}

	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := getJSON(ctx, client, g.apiBaseURL+"/user/emails", &emails); err != nil {
		return nil, fmt.Errorf("github /user/emails: %w", err)
	}

	// Pick the primary AND verified email (RFC-003 2.1). Fall back to any verified
	// email if no primary one is, so a verified-but-not-primary still signs in.
	var picked string
	for _, e := range emails {
		if e.Verified && e.Primary {
			picked = e.Email
			break
		}
	}
	if picked == "" {
		for _, e := range emails {
			if e.Verified {
				picked = e.Email
				break
			}
		}
	}
	if picked == "" {
		return nil, ErrEmailNotVerified
	}

	name := user.Name
	if name == "" {
		name = user.Login
	}
	return &Profile{
		Provider:       domain.ProviderGitHub,
		ProviderUserID: strconv.FormatInt(user.ID, 10), // stable numeric id, not the renamable login
		Email:          picked,
		Name:           name,
		AvatarURL:      user.AvatarURL,
	}, nil
}

func getJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

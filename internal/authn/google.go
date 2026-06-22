package authn

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"pulse/internal/domain"
)

// GoogleProvider is the Google OIDC provider (RFC-003 2.1). It uses OIDC discovery
// to find the endpoints and JWKS, runs authorization-code + PKCE, and reads the
// verified email from the ID token (email + email_verified claims), verifying the
// ID token signature against Google's JWKS and checking the nonce.

// OIDCConfig is the provider config sourced from config/env (RFC-003 8.1).
type OIDCConfig struct {
	IssuerURL    string // discovery base, e.g. https://accounts.google.com
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

type googleProvider struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewGoogleProvider builds the Google provider. It does OIDC discovery against
// cfg.IssuerURL at construction (so a bad config fails at boot, not per request).
// In tests cfg.IssuerURL points at the fake IdP's discovery document.
func NewGoogleProvider(ctx context.Context, cfg OIDCConfig) (Provider, error) {
	prov, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("google oidc discovery: %w", err)
	}
	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     prov.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	return &googleProvider{
		oauth:    oauthCfg,
		verifier: prov.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

func (g *googleProvider) Name() domain.IdentityProvider { return domain.ProviderGoogle }

func (g *googleProvider) AuthCodeURL(state, nonce, challenge string) string {
	return g.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

func (g *googleProvider) Exchange(ctx context.Context, code, verifier, nonce string) (*Profile, error) {
	tok, err := g.oauth.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", verifier),
	)
	if err != nil {
		return nil, fmt.Errorf("google token exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return nil, errors.New("google response had no id_token")
	}
	idTok, err := g.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, fmt.Errorf("google id token verify: %w", err)
	}
	if idTok.Nonce != nonce {
		return nil, errors.New("google id token nonce mismatch")
	}

	var claims struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("google id token claims: %w", err)
	}
	// The hard rule: never act on an unverified provider email (RFC-003 2.1, PRD-001 E1).
	if claims.Email == "" || !claims.EmailVerified {
		return nil, ErrEmailNotVerified
	}
	return &Profile{
		Provider:       domain.ProviderGoogle,
		ProviderUserID: claims.Sub,
		Email:          claims.Email,
		Name:           claims.Name,
		AvatarURL:      claims.Picture,
	}, nil
}

// ErrEmailNotVerified is returned when a provider gives back no verified email, so
// sign-in is refused and no user is created (RFC-003 2.1, PRD-001 E1).
var ErrEmailNotVerified = errors.New("provider email is not verified")

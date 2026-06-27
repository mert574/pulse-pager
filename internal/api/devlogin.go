package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"pulse/internal/domain"
)

// Dev-login: a guarded, local-only sign-in that skips OAuth entirely (RFC-016
// anticipates widening the identity provider set). With PULSE_DEV_LOGIN=true the
// router registers POST /auth/dev/login; with it off the route does not exist and
// any request gets the mux's 404. This is for local/dev so a developer can get a
// real Postgres-backed account (real user + personal org + owner membership + a
// real JWT session) without setting up Google/GitHub creds. Never enable it in
// production: the route is simply absent there.

// devLoginRequest is the JSON body: an email (required) and an optional display name.
type devLoginRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// handleDevLogin resolves the posted email to a real user (creating one with a
// personal org on first sign-in), stamps last_login, and mints the same session
// cookies the OAuth callback sets, so the SPA's "Dev sign in" button plus the
// GET /api/v1/me bootstrap work unchanged. Responds 204 with the cookies set.
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	var req devLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEnvelope(w, http.StatusUnprocessableEntity, "validation_failed", "invalid JSON body")
		return
	}
	email := strings.TrimSpace(req.Email)
	if !looksLikeEmail(email) {
		writeEnvelope(w, http.StatusUnprocessableEntity, "validation_failed", "a valid email is required")
		return
	}

	userID, err := s.resolveDevUser(r, email, strings.TrimSpace(req.Name))
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, "internal", "could not sign in")
		return
	}

	if err := s.issueSession(w, r, userID, email); err != nil {
		writeEnvelope(w, http.StatusInternalServerError, "internal", "could not issue session")
		return
	}
	s.log.InfoContext(r.Context(), fmt.Sprintf("dev login: %s (user %d)", email, userID), "user", userID, "email", email, "method", "dev")
	w.WriteHeader(http.StatusNoContent)
}

// resolveDevUser finds the user by email or creates one with a personal org + owner
// membership (so a fresh email signs in just like a first OAuth sign-in), linking a
// 'dev' provider identity. It stamps last_login on the way out. A second dev-login
// with the same email returns the same user, so it never makes a duplicate.
func (s *Server) resolveDevUser(r *http.Request, email, name string) (int64, error) {
	ctx := r.Context()

	existing, err := s.store.GetUserByEmail(ctx, email)
	if err == nil {
		_ = s.store.SetLastLogin(ctx, existing.ID)
		return existing.ID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	if name == "" {
		name = strings.Split(email, "@")[0]
	}
	u := &domain.User{Email: email, EmailVerified: true, Name: name}
	// The 'dev' provider identity uses the email as its stable subject id; the unique
	// (provider, provider_user_id) index keeps one dev identity per email.
	idn := &domain.UserIdentity{Provider: domain.ProviderDev, ProviderUserID: email}
	orgName, orgSlug := devOrgNaming(name, email)
	res, err := s.store.CreateUserWithPersonalOrg(ctx, u, idn, orgName, orgSlug)
	if err != nil {
		return 0, err
	}
	return res.UserID, nil
}

// looksLikeEmail is a light presence + shape check: non-empty, one '@' with text on
// both sides and a dot in the domain. Dev-login is local, so this stays simple.
func looksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	local, domainPart := s[:at], s[at+1:]
	if local == "" || strings.ContainsRune(domainPart, '@') {
		return false
	}
	dot := strings.IndexByte(domainPart, '.')
	return dot > 0 && dot < len(domainPart)-1
}

// devOrgNaming derives the personal org name and a unique-ish slug, mirroring the
// OAuth first-sign-in naming ("Dev's workspace") with a short random suffix.
func devOrgNaming(name, email string) (orgName, slug string) {
	base := name
	if base == "" {
		base = strings.Split(email, "@")[0]
	}
	orgName = base + "'s workspace"
	suffix, _ := newCSRF()
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	return orgName, devSlugify(base) + "-" + strings.ToLower(suffix)
}

// devSlugify lowercases and keeps a-z0-9, turning spaces/punctuation into dashes.
func devSlugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "workspace"
	}
	return out
}

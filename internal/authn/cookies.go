package authn

import (
	"net/http"
	"time"
)

// Cookie names and the cookie discipline carried forward from v1 auth (RFC-003
// 1.4, 4.4): the access JWT and the refresh token are both httpOnly+Secure+
// SameSite=Lax; the refresh cookie is path-scoped to /auth so the long-lived
// credential only travels to the refresh and logout endpoints; the CSRF cookie is
// readable by JS (not httpOnly) so the SPA can echo it in a header.
const (
	AccessCookie  = "pulse_at"
	RefreshCookie = "pulse_rt"
	CSRFCookie    = "pulse_csrf"
	// OAuthStateCookie is the short-lived cookie that cross-checks the callback
	// state against the query param (RFC-003 2.2).
	OAuthStateCookie = "pulse_oauth_state"

	refreshPath = "/auth"
)

// CookieConfig controls cookie attributes. Secure is configurable so dev over
// http works; production sets it true (RFC-003 4.4).
type CookieConfig struct {
	Secure bool
}

// SetSession writes the access, refresh, and CSRF cookies after a successful login
// or refresh (RFC-003 4.4). accessExp and refreshExp bound the cookie MaxAge.
func (c CookieConfig) SetSession(w http.ResponseWriter, accessJWT, refreshToken, csrf string, accessExp, refreshExp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     AccessCookie,
		Value:    accessJWT,
		Path:     "/",
		Expires:  accessExp,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookie,
		Value:    refreshToken,
		Path:     refreshPath, // only sent to /auth/* (refresh + logout)
		Expires:  refreshExp,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookie,
		Value:    csrf,
		Path:     "/",
		Expires:  accessExp,
		HttpOnly: false, // JS must read it to echo X-CSRF-Token (RFC-003 4.5)
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSession expires the session cookies on logout (RFC-003 4.3). The refresh
// cookie must be cleared on its own path or the browser keeps it.
func (c CookieConfig) ClearSession(w http.ResponseWriter) {
	for _, ck := range []struct {
		name, path string
	}{
		{AccessCookie, "/"},
		{RefreshCookie, refreshPath},
		{CSRFCookie, "/"},
	} {
		http.SetCookie(w, &http.Cookie{
			Name:     ck.name,
			Value:    "",
			Path:     ck.path,
			MaxAge:   -1,
			HttpOnly: ck.name != CSRFCookie,
			Secure:   c.Secure,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// SetOAuthState writes the short-lived state cookie used to cross-check the OAuth
// callback (RFC-003 2.2). It is httpOnly and expires with the flow TTL.
func (c CookieConfig) SetOAuthState(w http.ResponseWriter, state string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     OAuthStateCookie,
		Value:    state,
		Path:     "/auth",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearOAuthState expires the state cookie once the callback has consumed it.
func (c CookieConfig) ClearOAuthState(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     OAuthStateCookie,
		Value:    "",
		Path:     "/auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

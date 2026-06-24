package api

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"pulse/internal/events"
)

// Magic-link (passwordless) email login (RFC-003), alongside OAuth. Two hand-wired
// routes, like the other /auth/* surface, because they are a POST that returns a
// neutral JSON body and a GET that redirects, not part of the JSON resource
// contract:
//
//   - POST /auth/email/start: takes an email, rate-limits, and publishes a
//     MagicLinkRequested intent; the notifier mints the one-time token, stores its hash
//     in Redis, and sends the link (RFC-019). It ALWAYS returns the same neutral success
//     so it never reveals whether the email has an account (enumeration-safe).
//   - GET /auth/email/verify: consumes the token (single-use via Redis GETDEL),
//     finds-or-creates the user, mints the session cookies, and redirects into the
//     SPA (new users to /onboarding, the same as the OAuth callback).

// Rate-limit windows for the start handler (RFC-003). Both layers run; either one
// tripping returns the same neutral response so the limit does not leak whether the
// email exists. Per-email is the tight loop (stop spamming one inbox); per-IP is the
// broad loop (stop one client probing many addresses).
const (
	magicLinkPerEmailLimit  = 3
	magicLinkPerEmailWindow = 15 * time.Minute
	magicLinkPerIPLimit     = 10
	magicLinkPerIPWindow    = time.Hour
)

// handleEmailStart begins a passwordless login: parse + normalize the email,
// rate-limit by email and by IP, mint a token, store its hash, and email the verify
// link. The response is always the same neutral success (or a 429 with the same
// neutral body when a limit is hit), so it never reveals whether the email exists or
// whether the send worked.
func (s *Server) handleEmailStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEnvelope(w, http.StatusUnprocessableEntity, "validation_failed", "invalid JSON body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !looksLikeEmail(email) {
		writeEnvelope(w, http.StatusUnprocessableEntity, "validation_failed", "a valid email is required")
		return
	}

	// Rate-limit both layers before doing any work. On exceed we return 429 with the
	// SAME neutral body, so a caller can't tell a limited request from a fresh one for
	// a known vs unknown email.
	if s.magicLimited(r, email) {
		writeEnvelope(w, http.StatusTooManyRequests, "rate_limited", magicLinkNeutralMessage)
		return
	}

	// Publish the intent; the notifier mints the token and sends the link (RFC-019). A
	// publish error is swallowed into the neutral response, so the edge never leaks an
	// internal failure. With no publisher wired (dev/test without a bus), we still answer
	// neutrally. The key is the email (no org context for a sign-in link).
	if s.email != nil {
		_ = s.email.PublishEmail(r.Context(), email, events.EmailIntent{
			Type:      events.EmailMagicLink,
			Locale:    magicLinkLocale(r),
			MagicLink: &events.MagicLinkRequested{Email: email},
		})
	}

	writeEnvelope(w, http.StatusOK, "ok", magicLinkNeutralMessage)
}

// handleEmailVerify completes a passwordless login: consume the one-time token,
// find-or-create the user, mint the session cookies, and redirect into the SPA. A
// bad/used/expired token, or a session failure, sends the browser to the same
// login-failed page the OAuth callback uses.
func (s *Server) handleEmailVerify(w http.ResponseWriter, r *http.Request) {
	if s.magic == nil {
		s.redirectToApp(w, r, "/login?error=auth_failed")
		return
	}
	token := r.URL.Query().Get("token")
	userID, email, isNew, err := s.magic.Verify(r.Context(), token)
	if err != nil {
		// Invalid/used/expired token (or a store error) aborts with no session.
		s.redirectToApp(w, r, "/login?error=auth_failed")
		return
	}
	if err := s.issueSession(w, r, userID, email); err != nil {
		s.redirectToApp(w, r, "/login?error=session_failed")
		return
	}
	dest := "/"
	if isNew {
		dest = "/onboarding"
	}
	s.redirectToApp(w, r, dest)
}

// magicLinkNeutralMessage is the single enumeration-safe message: it never says
// whether the email has an account or whether the mail was sent.
const magicLinkNeutralMessage = "if that email has an account, we sent a link"

// magicLimited reports whether this start request is over either rate-limit window.
// It increments the per-email and per-IP counters (fixed-window via Incr + Expire on
// the first hit). With no Redis wired (dev/test) it never limits. Either layer over
// its limit returns true; the handler then answers with the neutral response.
func (s *Server) magicLimited(r *http.Request, email string) bool {
	if s.redis == nil {
		return false
	}
	emailOver := s.overLimit(r, "ratelimit:magiclink:email:"+email, magicLinkPerEmailLimit, magicLinkPerEmailWindow)
	ipOver := s.overLimit(r, "ratelimit:magiclink:ip:"+clientIP(r), magicLinkPerIPLimit, magicLinkPerIPWindow)
	return emailOver || ipOver
}

// overLimit increments a fixed-window counter and reports whether it now exceeds the
// limit. It sets the window TTL on the first increment (count == 1), matching the
// check-now sustained-rate counter. A Redis error fails open (does not limit) so a
// Redis blip can't lock everyone out of login.
func (s *Server) overLimit(r *http.Request, key string, limit int, window time.Duration) bool {
	n, err := s.redis.Incr(r.Context(), key)
	if err != nil {
		return false
	}
	if n == 1 {
		_ = s.redis.Expire(r.Context(), key, window)
	}
	return n > int64(limit)
}

// magicLinkLocale picks the email locale from the Accept-Language header, falling
// back to English. We only need the leading two letters to match the en/de/es copy.
func magicLinkLocale(r *http.Request) string {
	al := strings.ToLower(strings.TrimSpace(r.Header.Get("Accept-Language")))
	if len(al) >= 2 {
		return al[:2]
	}
	return "en"
}

// clientIP extracts the client IP for the per-IP rate-limit key. It trusts the first
// hop of X-Forwarded-For when present (the platform runs behind a proxy that sets
// it), else falls back to the connection's RemoteAddr without the port.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

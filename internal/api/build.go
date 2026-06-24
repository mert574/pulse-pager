package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"pulse/internal/authn"
	"pulse/internal/billing"
	"pulse/internal/billing/paddle"
	"pulse/internal/bus"
	"pulse/internal/config"
	"pulse/internal/crypto"
	"pulse/internal/entitlements"
	"pulse/internal/events"
	"pulse/internal/kv"
	"pulse/internal/notify"
	"pulse/internal/obs"
	"pulse/internal/store"
)

// Deps are the connected runtime dependencies the api edge needs to build its
// handlers (RFC-003). The caller (cmd/api) connects Postgres + Redis on the shared
// runtime and passes them here.
type Deps struct {
	Cfg   *config.Config
	Store *store.Pool
	Redis *kv.Client
	Log   *slog.Logger
	Reg   *prometheus.Registry
	// Producer publishes monitor.changed so the live schedule tracks a monitor
	// create/update/delete (PRD-006 5). Optional: nil skips the publish (the scheduler
	// still rebuilds from Postgres on its scan).
	Producer *bus.Producer
}

// Build assembles the real identity api from the config and connected deps
// (RFC-003): it loads the JWT signing key, registers the OAuth providers that are
// configured, builds the login/refresh services and the request Authenticator, and
// returns the Server plus the fully wired HTTP handler (middleware chain + routes).
func Build(ctx context.Context, d Deps) (*Server, http.Handler, error) {
	ic := d.Cfg.Identity

	signing, err := authn.NewSigningKey(ic.JWTKeyID, ic.JWTPrivateKeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("load jwt signing key: %w", err)
	}
	jwt := authn.NewJWTIssuer(ic.JWTIssuer, ic.JWTAudience, signing)

	providers, err := buildProviders(ctx, ic)
	if err != nil {
		return nil, nil, err
	}
	// With dev-login on, the api may run with no OAuth provider (local dev signs in
	// via POST /auth/dev/login). Without it, at least one provider is required.
	if len(providers) == 0 && !ic.DevLogin {
		return nil, nil, fmt.Errorf("no OAuth providers configured")
	}

	loginSvc := authn.NewLoginService(providers, d.Redis, d.Store)
	refreshSvc := authn.NewRefreshService(d.Store)
	keyVerifier := authn.NewAPIKeyVerifier(d.Store, d.Redis)

	// Wire the secret-field cipher so secret monitor headers are encrypted at rest
	// (PRD-002 2.2, master 13). The key is validated at config load; LoadKey here
	// returns the AEAD. A bad key fails the build (fail closed, never plaintext).
	cipher, err := crypto.LoadKey(d.Cfg.SecretKey)
	if err != nil {
		return nil, nil, fmt.Errorf("load secret key: %w", err)
	}
	d.Store.SetCipher(cipher)

	// Publish monitor.changed when a monitor is created/updated/deleted so the
	// scheduler picks up the change (PRD-006 5). Skipped when no producer is wired.
	var changed MonitorPublisher
	if d.Producer != nil {
		changed = &busMonitorPublisher{prod: d.Producer}
	}

	// Cloudflare Access verifier for the admin origin: built only when both the team
	// domain and the application AUD are set. Nil otherwise, so the admin endpoint
	// uses the normal session + allowlist (local/dev).
	var cfAccess *authn.CFAccessVerifier
	if ic.CFAccessTeamDomain != "" && ic.CFAccessAUD != "" {
		cfAccess = authn.NewCFAccessVerifier(ic.CFAccessTeamDomain, ic.CFAccessAUD)
	}

	// Billing provider for the operator/self-serve billing endpoints (RFC-018). The api
	// only makes operator/self-serve calls (not webhook verify), so the stub needs no
	// secret. Defaults to the stub so the api runs without a provider account.
	var billingProvider billing.Provider
	switch d.Cfg.Billing.Provider {
	case "paddle":
		billingProvider = paddle.New(d.Cfg.Billing.PaddleAPIKey, "")
	default:
		billingProvider = billing.NewStub("")
	}

	// Audit publisher for operator billing actions (RFC-018 8). Emits to audit.events;
	// nil when no producer is wired (dev/test), in which case the action still happens.
	var auditPub AuditPublisher
	if d.Producer != nil {
		auditPub = &busAuditPublisher{prod: d.Producer, log: d.Log}
	}

	// The Authenticator writes 401/403 using the localizable envelope so an auth
	// failure reads the same as a handler-level failure.
	auth := authn.NewAuthenticator(jwt, keyVerifier, d.Store, d.Redis,
		authn.WithErrorWriter(func(w http.ResponseWriter, status int, code, msg string) {
			writeEnvelope(w, status, code, msg)
		}),
	)

	srv := New(Config{
		Store:      d.Store,
		Login:      loginSvc,
		JWT:        jwt,
		Refresh:    refreshSvc,
		Cookies:    authn.CookieConfig{Secure: ic.CookieSecure},
		Auth:       auth,
		Keys:       keyVerifier,
		AppBaseURL: ic.AppBaseURL,
		// DevLogin exposes POST /auth/dev/login for local sign-in without OAuth. Off in
		// production (PULSE_DEV_LOGIN unset/false), so the route is simply not registered.
		DevLogin: ic.DevLogin,
		// Seats: the per-plan cap resolver (PRD-001 5.2); PRD-006 will swap in the
		// real per-org resolver behind this seam.
		Seats: entitlements.DefaultSeats{},
		// Monitors: the per-plan monitor cap / interval floor / region resolver
		// (PRD-006); the real per-org resolver drops in behind this seam later.
		Monitors: entitlements.DefaultMonitors{},
		// Changed: publishes monitor.changed for the scheduler (nil when no producer).
		Changed: changed,
		// Jobs: enqueues check-now jobs onto the same pipeline scheduled checks use, so a
		// manual check fans out per region through the worker.
		Jobs: &busCheckJobPublisher{prod: d.Producer},
		// State: the live per-(monitor,region) check-state store (Redis), read by the
		// region-states endpoint and written by check-now/scheduler/worker.
		State: d.Redis,
		// CheckNow: the Redis-backed manual-check rate limit (PRD-006). The api always
		// connects Redis, so this is non-nil in production; it survives api restarts
		// because it lives in Redis, not in process memory.
		CheckNow: d.Redis,
		// Mailer: real SMTP when configured, else a dev mailer that logs the accept
		// link so the invite flow still completes (PRD-001 6.1).
		Mailer: buildMailer(ic, d.Log),
		// PlatformAdmins: the email allowlist for the operator admin panel
		// (PULSE_PLATFORM_ADMINS). Empty means the panel is closed.
		PlatformAdmins: ic.PlatformAdmins,
		// CFAccess: verifies the Cloudflare Access token on the admin endpoint when
		// both the team domain and AUD are configured; nil otherwise (local/dev).
		CFAccess: cfAccess,
		// Billing: the payment provider for operator/self-serve billing (RFC-018).
		Billing: billingProvider,
		// Audit: emits audit.events for operator billing actions (nil when no producer).
		Audit: auditPub,
	})

	handler := chain(srv.Router(), d.Log, newHTTPMetrics(d.Reg))
	return srv, handler, nil
}

// buildProviders registers the OAuth providers that have both a client id and
// secret configured (RFC-003 2.1). Google uses OIDC discovery (so a bad issuer
// fails at boot); GitHub uses plain OAuth2. It may return an empty list; the caller
// decides whether zero providers is allowed (it is only when dev-login is on).
func buildProviders(ctx context.Context, ic config.IdentityConfig) ([]authn.Provider, error) {
	var providers []authn.Provider
	if ic.GoogleClientID != "" && ic.GoogleClientSecret != "" {
		g, err := authn.NewGoogleProvider(ctx, authn.OIDCConfig{
			IssuerURL:    ic.GoogleIssuerURL,
			ClientID:     ic.GoogleClientID,
			ClientSecret: ic.GoogleClientSecret,
			RedirectURL:  ic.GoogleRedirectURL,
		})
		if err != nil {
			return nil, fmt.Errorf("build google provider: %w", err)
		}
		providers = append(providers, g)
	}
	if ic.GitHubClientID != "" && ic.GitHubClientSecret != "" {
		providers = append(providers, authn.NewGitHubProvider(authn.OAuth2Config{
			ClientID:     ic.GitHubClientID,
			ClientSecret: ic.GitHubClientSecret,
			RedirectURL:  ic.GitHubRedirectURL,
		}))
	}
	return providers, nil
}

// buildMailer picks the transactional mailer for invitation email. With an SMTP
// host configured it sends for real; otherwise it logs the accept link (so a
// self-host without SMTP still completes the flow and the operator sees the link).
func buildMailer(ic config.IdentityConfig, log *slog.Logger) notify.Mailer {
	if ic.SMTPHost == "" {
		log.Warn("no SMTP configured: invitation emails will be logged, not sent")
		return notify.LogMailer{Log: log}
	}
	return notify.NewSMTPMailer(notify.SMTPMailerConfig{
		Host:     ic.SMTPHost,
		Port:     ic.SMTPPort,
		Username: ic.SMTPUsername,
		Password: ic.SMTPPassword,
		From:     ic.SMTPFrom,
		TLSMode:  ic.SMTPTLSMode,
	})
}

// busMonitorPublisher publishes monitor.changed onto the bus, keyed by org_id so a
// consumer sees an org's changes in order (RFC-002 5.1). It implements MonitorPublisher.
type busMonitorPublisher struct {
	prod *bus.Producer
}

// MonitorChanged emits the monitor.changed event for the scheduler.
func (b *busMonitorPublisher) MonitorChanged(ctx context.Context, orgID, monitorID int64) error {
	payload, err := json.Marshal(events.MonitorChangedEvent{
		OrgID:     orgID,
		MonitorID: monitorID,
		ChangedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return b.prod.Produce(ctx, bus.TopicMonitorChanged, strconv.FormatInt(orgID, 10), payload)
}

// busAuditPublisher emits audit.events onto the bus, keyed by org_id so a consumer
// sees an org's actions in order (RFC-018 8). It implements AuditPublisher. There is no
// consumer yet; the emit is best-effort so the trail exists for the future audit log.
type busAuditPublisher struct {
	prod *bus.Producer
	log  *slog.Logger
}

// Audit emits one audit event; a failure is logged, not surfaced (the action happened).
func (b *busAuditPublisher) Audit(ctx context.Context, ev events.AuditEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := b.prod.Produce(ctx, bus.TopicAuditEvents, strconv.FormatInt(ev.OrgID, 10), payload); err != nil {
		b.log.Warn("audit emit failed", "action", ev.Action, "org_id", ev.OrgID, "err", err)
		return err
	}
	return nil
}

// busCheckJobPublisher enqueues a check job onto check.jobs.<region>, keyed by monitor
// id (matching the scheduler), so check-now fans out through the worker. It implements
// CheckJobPublisher.
type busCheckJobPublisher struct {
	prod *bus.Producer
}

// PublishCheckJob produces one check job for its region.
func (b *busCheckJobPublisher) PublishCheckJob(ctx context.Context, job events.CheckJob) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return b.prod.Produce(ctx, bus.CheckJobsTopic(job.Region), strconv.FormatInt(job.Monitor.ID, 10), payload)
}

// --- middleware chain ---

// chain wraps the router with recover, a request id, and request metrics, outermost
// first so a panic deep in a handler is still recovered and counted.
func chain(h http.Handler, log *slog.Logger, m *httpMetrics) http.Handler {
	return recoverMW(log)(requestIDMW(m.instrument(h)))
}

// recoverMW turns a handler panic into a 500 envelope instead of dropping the
// connection, and logs it.
func recoverMW(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic in handler", "err", rec, "path", r.URL.Path)
					writeEnvelope(w, http.StatusInternalServerError, "internal", "internal error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// requestIDMW puts a correlation id on the request context and echoes it in the
// response so a client can quote it. It reuses an inbound X-Request-Id when present.
func requestIDMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id, _ = authn.NewCSRFToken() // a random opaque id; reuse the token generator
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(obs.WithCorrelationID(r.Context(), id)))
	})
}

// httpMetrics holds the request counter and latency histogram registered on the
// shared registry (RFC-010 2.5: per-service SLI metrics live on the same registry).
type httpMetrics struct {
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
}

func newHTTPMetrics(reg *prometheus.Registry) *httpMetrics {
	m := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "pulse_api_http_requests_total",
			Help: "Count of HTTP requests by method and status.",
		}, []string{"method", "status"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "pulse_api_http_request_duration_seconds",
			Help:    "HTTP request duration by method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method"}),
	}
	if reg != nil {
		reg.MustRegister(m.requests, m.latency)
	}
	return m
}

// instrument records the count, status, and latency of each request.
func (m *httpMetrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		m.requests.WithLabelValues(r.Method, http.StatusText(sw.status)).Inc()
		m.latency.WithLabelValues(r.Method).Observe(time.Since(start).Seconds())
	})
}

// statusWriter captures the response status for metrics.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

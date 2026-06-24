// Package config reads and validates per-service env vars at startup. A service
// only requires the dependencies it actually uses, so the worker does not need a
// Postgres DSN to boot. Required-but-missing vars fail closed: Load returns an
// error so main exits non-zero before the service does any work. This replaces
// the v1 single-process config; the per-service shape matches RFC-000 section 2.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"pulse/internal/crypto"
	"pulse/internal/region"
)

// Service names the binary asking for config. It decides which deps are required.
type Service string

const (
	ServiceAPI       Service = "api"
	ServiceScheduler Service = "scheduler"
	ServiceWorker    Service = "worker"
	ServiceAlerting  Service = "alerting"
	ServiceNotifier  Service = "notifier"
	ServiceBilling   Service = "billing"
)

// needs describes which dependencies a service must have configured to boot.
type needs struct {
	postgres bool
	redis    bool
	kafka    bool
	secret   bool // needs the AES key (touches encrypted channel/header secrets)
}

var serviceNeeds = map[Service]needs{
	ServiceAPI:       {postgres: true, redis: true, kafka: true, secret: true},
	ServiceScheduler: {postgres: true, redis: true, kafka: true},
	ServiceWorker:    {postgres: true, redis: true, kafka: true},
	ServiceAlerting:  {postgres: true, redis: true, kafka: true},
	ServiceNotifier:  {postgres: true, redis: true, kafka: true, secret: true},
	// billing only writes subscription state from the webhook sync path (RFC-018 1).
	// Nothing consumes billing.events yet, so no bus; the webhook secret is a plain
	// env var, so no AES key.
	ServiceBilling: {postgres: true},
}

// Config is the resolved settings for one service.
type Config struct {
	Service        Service
	LogLevel       string
	HealthAddr     string
	Region         string
	TracingEnabled bool

	PostgresDSN  string
	RedisAddr    string
	KafkaBrokers []string
	// BusBackend selects the event transport: "kafka" (default, the distributed
	// deployment) or "redis" (Redis Streams, a single-node lightweight mode that
	// reuses Redis instead of a separate broker). See internal/bus.
	BusBackend string
	SecretKey  string // base64 of 32 bytes, validated by crypto.LoadKey

	// BlockPrivateNetworks turns on the checker's SSRF guard. Default false here
	// (v1 self-host default); the hosted product sets it true (PRD-013). Worker reads it.
	BlockPrivateNetworks bool

	// Identity is the auth/identity config the api edge needs (RFC-003 8.1): the JWT
	// signing key, the OAuth provider creds, the cookie/secure and app-base settings.
	// Only the api service loads it; missing required values fail closed at boot.
	Identity IdentityConfig

	// Billing is the billing-service config (RFC-018). Only the billing service loads it.
	Billing BillingConfig
}

// BillingConfig is the billing service's settings (RFC-018 Phase 1). The provider is
// pluggable behind the billing.Provider seam; "stub" is the dev/test adapter that
// needs no provider account. WebhookSecret verifies the inbound webhook signature for
// whichever provider is selected.
type BillingConfig struct {
	// Provider selects the adapter: "stub" (default) or "paddle".
	Provider string
	// WebhookSecret is the shared secret the webhook signature is verified against
	// (billing service only; the api does not verify webhooks).
	WebhookSecret string
	// Addr is the webhook HTTP listen address, separate from HealthAddr.
	Addr string
	// PaddleAPIKey is the Paddle Billing API key, used by the api/billing services to
	// make provider calls when Provider is "paddle". Empty for the stub.
	PaddleAPIKey string
	// APIBase overrides the Paddle API base URL (PULSE_PADDLE_API_BASE). Empty means
	// production, except a sandbox key (contains "sdbx") auto-selects the sandbox host.
	APIBase string
}

// IdentityConfig is the api edge's auth settings (RFC-003 8.1). The JWT signing key
// is required in production and falls back to a generated dev key only when
// PULSE_DEV_AUTH is set. The OAuth providers are optional: a provider is registered
// only when both its client id and secret are present, so a deployment can run with
// just Google, just GitHub, or both.
type IdentityConfig struct {
	// AppBaseURL is the SPA origin the OAuth callback redirects back into, e.g.
	// https://app.pulse.dev. Empty means redirect to a relative path.
	AppBaseURL string
	// CookieSecure marks the session cookies Secure. True in production; dev over
	// http sets it false. Defaults to true and fails safe.
	CookieSecure bool

	// DevLogin turns on the guarded dev-login route (POST /auth/dev/login) so a
	// developer can sign in locally without OAuth creds and get a real Postgres-backed
	// account. Off by default and meant for local/dev only; never set it in production.
	// When on, the api may boot with no OAuth provider configured.
	DevLogin bool

	// JWTIssuer and JWTAudience are the iss/aud claims (RFC-003 3.1).
	JWTIssuer   string
	JWTAudience string
	// JWTKeyID is the kid published in JWKS so a verifier picks the right key.
	JWTKeyID string
	// JWTPrivateKeyPEM is the RS256 private key as a PEM string. If empty,
	// JWTPrivateKeyPath is read from disk. Required in production.
	JWTPrivateKeyPEM string
	// JWTPrivateKeyPath is an alternate to the inline PEM: a path to the key file.
	JWTPrivateKeyPath string

	// GoogleIssuerURL is the OIDC discovery base (default https://accounts.google.com).
	GoogleIssuerURL    string
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string

	GitHubClientID     string
	GitHubClientSecret string
	GitHubRedirectURL  string

	// SMTP is the transactional-email connection used for invitation emails
	// (PRD-001 6.1). When SMTPHost is empty the api falls back to a dev mailer that
	// logs the accept link instead of sending, so the flow still completes.
	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPTLSMode  string

	// PlatformAdmins is the lowercased email allowlist for the operator admin panel
	// (GET /admin/metrics), read from PULSE_PLATFORM_ADMINS (comma-separated). Empty
	// means no one is an admin, so the panel is closed by default (fails safe).
	PlatformAdmins []string

	// CFAccessTeamDomain and CFAccessAUD configure Cloudflare Access verification for
	// the admin origin (admin.pulsepager.com). When both are set, the admin endpoint
	// authorizes off the verified CF Access identity instead of an app session, so a
	// customer app-session cookie can't reach it. Empty = not behind CF Access (the
	// admin endpoint falls back to the normal session + allowlist, for local/dev).
	CFAccessTeamDomain string // e.g. yourteam.cloudflareaccess.com
	CFAccessAUD        string // the Access application AUD tag
}

// Load reads env vars for the given service and validates the deps it requires.
func Load(service Service) (*Config, error) {
	n, ok := serviceNeeds[service]
	if !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}

	cfg := &Config{
		Service:    service,
		LogLevel:   withDefault("PULSE_LOG_LEVEL", "info"),
		HealthAddr: withDefault("PULSE_HEALTH_ADDR", ":8080"),
		Region:     withDefault("PULSE_REGION", region.Default),
	}
	// Fail closed on an unknown region: a worker set to a region no plan or topic
	// knows would consume check.jobs.<that> and silently process nothing, while
	// monitors pile up on the real region's topic. This is the "home vs eu-central"
	// trap, caught at boot instead of as stuck-pending monitors later.
	if !region.Known(cfg.Region) {
		return nil, fmt.Errorf("PULSE_REGION %q is not a known region %v", cfg.Region, region.All)
	}

	var err error
	if cfg.TracingEnabled, err = parseBool("PULSE_TRACING_ENABLED", false); err != nil {
		return nil, err
	}
	if cfg.BlockPrivateNetworks, err = parseBool("PULSE_BLOCK_PRIVATE_NETWORKS", false); err != nil {
		return nil, err
	}

	if n.postgres {
		if cfg.PostgresDSN, err = required("PULSE_POSTGRES_DSN"); err != nil {
			return nil, err
		}
	}
	if n.redis {
		if cfg.RedisAddr, err = required("PULSE_REDIS_ADDR"); err != nil {
			return nil, err
		}
	}
	// n.kafka means "this service uses the event bus". Which transport it needs depends
	// on PULSE_BUS: kafka needs the brokers; redis reuses PULSE_REDIS_ADDR.
	cfg.BusBackend = withDefault("PULSE_BUS", "kafka")
	if cfg.BusBackend != "kafka" && cfg.BusBackend != "redis" {
		return nil, fmt.Errorf("PULSE_BUS must be \"kafka\" or \"redis\", got %q", cfg.BusBackend)
	}
	if n.kafka {
		switch cfg.BusBackend {
		case "kafka":
			brokers, err := required("PULSE_KAFKA_BROKERS")
			if err != nil {
				return nil, err
			}
			cfg.KafkaBrokers = splitList(brokers)
		case "redis":
			if cfg.RedisAddr == "" {
				if cfg.RedisAddr, err = required("PULSE_REDIS_ADDR"); err != nil {
					return nil, err
				}
			}
		}
	}
	if n.secret {
		if cfg.SecretKey, err = required("PULSE_SECRET_KEY"); err != nil {
			return nil, err
		}
		// Validate the key contents the same way the runtime will use them.
		if _, err := crypto.LoadKey(cfg.SecretKey); err != nil {
			return nil, fmt.Errorf("PULSE_SECRET_KEY invalid: %w", err)
		}
	}

	// Only the api edge needs the identity/auth config.
	if service == ServiceAPI {
		if cfg.Identity, err = loadIdentity(); err != nil {
			return nil, err
		}
		// The api makes operator/self-serve provider calls (RFC-018 5-6) but does not
		// verify webhooks, so it needs the provider selection (and the Paddle key when
		// paddle) but not the webhook secret. Defaults to the stub.
		cfg.Billing.Provider = withDefault("PULSE_BILLING_PROVIDER", "stub")
		if cfg.Billing.Provider != "stub" && cfg.Billing.Provider != "paddle" {
			return nil, fmt.Errorf("PULSE_BILLING_PROVIDER must be \"stub\" or \"paddle\", got %q", cfg.Billing.Provider)
		}
		cfg.Billing.PaddleAPIKey = os.Getenv("PULSE_PADDLE_API_KEY")
		cfg.Billing.APIBase = paddleAPIBase(cfg.Billing.PaddleAPIKey)
	}

	// Only the billing service needs the full billing config (incl. webhook secret).
	if service == ServiceBilling {
		if cfg.Billing, err = loadBilling(); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// loadBilling reads the billing service config (RFC-018). The webhook secret is
// required and fails closed: a billing service with no secret would accept any
// signature. The provider defaults to the stub so the service boots without a real
// provider account (mirrors PULSE_BUS defaulting to kafka).
func loadBilling() (BillingConfig, error) {
	bc := BillingConfig{
		Provider:     withDefault("PULSE_BILLING_PROVIDER", "stub"),
		Addr:         withDefault("PULSE_BILLING_ADDR", ":8082"),
		PaddleAPIKey: os.Getenv("PULSE_PADDLE_API_KEY"),
	}
	bc.APIBase = paddleAPIBase(bc.PaddleAPIKey)
	if bc.Provider != "stub" && bc.Provider != "paddle" {
		return bc, fmt.Errorf("PULSE_BILLING_PROVIDER must be \"stub\" or \"paddle\", got %q", bc.Provider)
	}
	var err error
	if bc.WebhookSecret, err = required("PULSE_BILLING_WEBHOOK_SECRET"); err != nil {
		return bc, err
	}
	return bc, nil
}

// loadIdentity reads the api edge's auth config (RFC-003 8.1). The JWT signing key
// is required and fails closed when missing (no silent dev key in the real api;
// PULSE_DEV_AUTH bypasses this whole path by running devapi instead). The OAuth
// provider creds are optional: a provider is wired only when both id and secret are
// present, so the api can run with one or both providers.
func loadIdentity() (IdentityConfig, error) {
	var ic IdentityConfig
	var err error

	ic.AppBaseURL = os.Getenv("PULSE_APP_BASE_URL")
	if ic.CookieSecure, err = parseBool("PULSE_COOKIE_SECURE", true); err != nil {
		return ic, err
	}
	if ic.DevLogin, err = parseBool("PULSE_DEV_LOGIN", false); err != nil {
		return ic, err
	}

	ic.JWTIssuer = withDefault("PULSE_JWT_ISSUER", "pulse")
	ic.JWTAudience = withDefault("PULSE_JWT_AUDIENCE", "pulse-api")
	ic.JWTKeyID = withDefault("PULSE_JWT_KID", "pulse-1")

	ic.JWTPrivateKeyPEM = os.Getenv("PULSE_JWT_PRIVATE_KEY_PEM")
	ic.JWTPrivateKeyPath = os.Getenv("PULSE_JWT_PRIVATE_KEY_PATH")
	if ic.JWTPrivateKeyPEM == "" && ic.JWTPrivateKeyPath != "" {
		b, rerr := os.ReadFile(ic.JWTPrivateKeyPath)
		if rerr != nil {
			return ic, fmt.Errorf("read PULSE_JWT_PRIVATE_KEY_PATH: %w", rerr)
		}
		ic.JWTPrivateKeyPEM = string(b)
	}
	if ic.JWTPrivateKeyPEM == "" {
		return ic, fmt.Errorf("PULSE_JWT_PRIVATE_KEY_PEM or PULSE_JWT_PRIVATE_KEY_PATH is required")
	}

	ic.GoogleIssuerURL = withDefault("PULSE_GOOGLE_ISSUER_URL", "https://accounts.google.com")
	ic.GoogleClientID = os.Getenv("PULSE_GOOGLE_CLIENT_ID")
	ic.GoogleClientSecret = os.Getenv("PULSE_GOOGLE_CLIENT_SECRET")
	ic.GoogleRedirectURL = os.Getenv("PULSE_GOOGLE_REDIRECT_URL")

	ic.GitHubClientID = os.Getenv("PULSE_GITHUB_CLIENT_ID")
	ic.GitHubClientSecret = os.Getenv("PULSE_GITHUB_CLIENT_SECRET")
	ic.GitHubRedirectURL = os.Getenv("PULSE_GITHUB_REDIRECT_URL")

	// Transactional email for invitations (optional; dev mailer logs when unset).
	ic.SMTPHost = os.Getenv("PULSE_SMTP_HOST")
	ic.SMTPPort = os.Getenv("PULSE_SMTP_PORT")
	ic.SMTPUsername = os.Getenv("PULSE_SMTP_USERNAME")
	ic.SMTPPassword = os.Getenv("PULSE_SMTP_PASSWORD")
	ic.SMTPFrom = os.Getenv("PULSE_SMTP_FROM")
	ic.SMTPTLSMode = os.Getenv("PULSE_SMTP_TLS_MODE")

	// Platform admin allowlist (the operator admin panel). Comma-separated emails,
	// lowercased and trimmed so the gate is a plain case-insensitive set lookup.
	for _, e := range splitList(os.Getenv("PULSE_PLATFORM_ADMINS")) {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			ic.PlatformAdmins = append(ic.PlatformAdmins, e)
		}
	}

	// Cloudflare Access for the admin origin (optional). Both must be set to turn on
	// CF Access verification on the admin endpoint.
	ic.CFAccessTeamDomain = strings.TrimSpace(os.Getenv("PULSE_CF_ACCESS_TEAM_DOMAIN"))
	ic.CFAccessAUD = strings.TrimSpace(os.Getenv("PULSE_CF_ACCESS_AUD"))

	// With dev-login on, the api is allowed to boot with no OAuth provider so a
	// developer can sign in locally without any creds. Otherwise at least one provider
	// is still required.
	if !ic.DevLogin && ic.GoogleClientID == "" && ic.GitHubClientID == "" {
		return ic, fmt.Errorf("at least one OAuth provider must be configured (PULSE_GOOGLE_CLIENT_ID or PULSE_GITHUB_CLIENT_ID)")
	}

	return ic, nil
}

// paddleAPIBase picks the Paddle API base URL: an explicit override, else the sandbox
// host for a sandbox key (contains "sdbx"), else "" (the adapter defaults to production).
func paddleAPIBase(key string) string {
	if b := os.Getenv("PULSE_PADDLE_API_BASE"); b != "" {
		return b
	}
	if strings.Contains(key, "sdbx") {
		return "https://sandbox-api.paddle.com"
	}
	return ""
}

func required(key string) (string, error) {
	if v := os.Getenv(key); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is required", key)
}

func withDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean (got %q): %w", key, v, err)
	}
	return b, nil
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

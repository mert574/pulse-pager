package config

import (
	"encoding/base64"
	"testing"
)

func validKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func TestLoad_WorkerNeedsInfraNotSecret(t *testing.T) {
	t.Setenv("PULSE_POSTGRES_DSN", "postgres://localhost/pulse")
	t.Setenv("PULSE_REDIS_ADDR", "localhost:6379")
	t.Setenv("PULSE_KAFKA_BROKERS", "localhost:9092")
	// PULSE_SECRET_KEY deliberately unset: the worker does not touch encrypted secrets.

	cfg, err := Load(ServiceWorker)
	if err != nil {
		t.Fatalf("worker Load: %v", err)
	}
	if cfg.SecretKey != "" {
		t.Errorf("worker should not require a secret key, got %q", cfg.SecretKey)
	}
	if cfg.Region != "home" {
		t.Errorf("default region = %q, want home", cfg.Region)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("default log level = %q, want info", cfg.LogLevel)
	}
}

func TestLoad_WorkerRequiresPostgres(t *testing.T) {
	t.Setenv("PULSE_REDIS_ADDR", "localhost:6379")
	t.Setenv("PULSE_KAFKA_BROKERS", "localhost:9092")
	// no Postgres DSN: the worker persists results, so this must fail closed.

	if _, err := Load(ServiceWorker); err == nil {
		t.Fatal("expected error when worker has no Postgres DSN")
	}
}

func TestLoad_APIMissingPostgresFailsClosed(t *testing.T) {
	t.Setenv("PULSE_REDIS_ADDR", "localhost:6379")
	t.Setenv("PULSE_KAFKA_BROKERS", "localhost:9092")
	t.Setenv("PULSE_SECRET_KEY", validKey())
	// PULSE_POSTGRES_DSN deliberately unset.

	if _, err := Load(ServiceAPI); err == nil {
		t.Fatal("expected error when api has no Postgres DSN")
	}
}

// setAPIIdentityEnv sets the identity vars the api edge requires so a Load(ServiceAPI)
// gets past the auth config (a JWT signing key and at least one OAuth provider).
func setAPIIdentityEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PULSE_JWT_PRIVATE_KEY_PEM", "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----")
	t.Setenv("PULSE_GITHUB_CLIENT_ID", "gh-id")
	t.Setenv("PULSE_GITHUB_CLIENT_SECRET", "gh-secret")
}

func TestLoad_APIValid(t *testing.T) {
	t.Setenv("PULSE_POSTGRES_DSN", "postgres://localhost/pulse")
	t.Setenv("PULSE_REDIS_ADDR", "localhost:6379")
	t.Setenv("PULSE_KAFKA_BROKERS", "broker1:9092, broker2:9092")
	t.Setenv("PULSE_SECRET_KEY", validKey())
	setAPIIdentityEnv(t)

	cfg, err := Load(ServiceAPI)
	if err != nil {
		t.Fatalf("api Load: %v", err)
	}
	if got := len(cfg.KafkaBrokers); got != 2 {
		t.Fatalf("brokers parsed = %d, want 2 (%v)", got, cfg.KafkaBrokers)
	}
	if cfg.KafkaBrokers[1] != "broker2:9092" {
		t.Errorf("broker trimming failed: %q", cfg.KafkaBrokers[1])
	}
	// Identity defaults: cookies secure by default, default issuer/audience/kid.
	if !cfg.Identity.CookieSecure {
		t.Error("cookies should default to secure")
	}
	if cfg.Identity.JWTIssuer != "pulse" || cfg.Identity.JWTAudience != "pulse-api" {
		t.Errorf("unexpected jwt iss/aud: %q %q", cfg.Identity.JWTIssuer, cfg.Identity.JWTAudience)
	}
}

func TestLoad_APIMissingJWTKeyFailsClosed(t *testing.T) {
	t.Setenv("PULSE_POSTGRES_DSN", "postgres://localhost/pulse")
	t.Setenv("PULSE_REDIS_ADDR", "localhost:6379")
	t.Setenv("PULSE_KAFKA_BROKERS", "localhost:9092")
	t.Setenv("PULSE_SECRET_KEY", validKey())
	t.Setenv("PULSE_GITHUB_CLIENT_ID", "gh-id")
	t.Setenv("PULSE_GITHUB_CLIENT_SECRET", "gh-secret")
	// PULSE_JWT_PRIVATE_KEY_PEM and _PATH deliberately unset: the api must not boot
	// without a signing key.

	if _, err := Load(ServiceAPI); err == nil {
		t.Fatal("expected error when api has no JWT signing key")
	}
}

func TestLoad_APIMissingOAuthProviderFailsClosed(t *testing.T) {
	t.Setenv("PULSE_POSTGRES_DSN", "postgres://localhost/pulse")
	t.Setenv("PULSE_REDIS_ADDR", "localhost:6379")
	t.Setenv("PULSE_KAFKA_BROKERS", "localhost:9092")
	t.Setenv("PULSE_SECRET_KEY", validKey())
	t.Setenv("PULSE_JWT_PRIVATE_KEY_PEM", "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----")
	// No OAuth provider configured.

	if _, err := Load(ServiceAPI); err == nil {
		t.Fatal("expected error when api has no OAuth provider configured")
	}
}

func TestLoad_APIDevLoginAllowsNoOAuthProvider(t *testing.T) {
	t.Setenv("PULSE_POSTGRES_DSN", "postgres://localhost/pulse")
	t.Setenv("PULSE_REDIS_ADDR", "localhost:6379")
	t.Setenv("PULSE_KAFKA_BROKERS", "localhost:9092")
	t.Setenv("PULSE_SECRET_KEY", validKey())
	t.Setenv("PULSE_JWT_PRIVATE_KEY_PEM", "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----")
	// No OAuth provider, but dev-login is on, so the api is allowed to boot.
	t.Setenv("PULSE_DEV_LOGIN", "true")

	cfg, err := Load(ServiceAPI)
	if err != nil {
		t.Fatalf("dev-login should let the api boot with no OAuth provider: %v", err)
	}
	if !cfg.Identity.DevLogin {
		t.Fatal("DevLogin should be true when PULSE_DEV_LOGIN=true")
	}
}

func TestLoad_APIDevLoginDefaultsOff(t *testing.T) {
	t.Setenv("PULSE_POSTGRES_DSN", "postgres://localhost/pulse")
	t.Setenv("PULSE_REDIS_ADDR", "localhost:6379")
	t.Setenv("PULSE_KAFKA_BROKERS", "localhost:9092")
	t.Setenv("PULSE_SECRET_KEY", validKey())
	setAPIIdentityEnv(t)
	// PULSE_DEV_LOGIN deliberately unset.

	cfg, err := Load(ServiceAPI)
	if err != nil {
		t.Fatalf("api Load: %v", err)
	}
	if cfg.Identity.DevLogin {
		t.Fatal("DevLogin should default to false when PULSE_DEV_LOGIN is unset")
	}
}

func TestLoad_InvalidSecretKeyRejected(t *testing.T) {
	t.Setenv("PULSE_POSTGRES_DSN", "postgres://localhost/pulse")
	t.Setenv("PULSE_REDIS_ADDR", "localhost:6379")
	t.Setenv("PULSE_KAFKA_BROKERS", "localhost:9092")
	t.Setenv("PULSE_SECRET_KEY", "not-base64-or-32-bytes")

	if _, err := Load(ServiceAPI); err == nil {
		t.Fatal("expected error for an invalid secret key")
	}
}

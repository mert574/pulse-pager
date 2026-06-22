package authn

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pulse/internal/crypto"
	"pulse/internal/domain"
)

// API-key verify (RFC-003 5). A key is pulse_sk_<random>. The presented key is
// SHA-256 hashed and looked up via a Redis cache in front of Postgres; revocation
// busts the cache, and if Redis is down the verify falls back to the DB (fail
// closed: a key verify must never fail open, RFC-003 5.3).

// APIKeyPrefix is the fixed prefix for a Pulse secret key. RFC-003 5.1 standardizes
// on pulse_sk_ (PRD-005 examples used pulse_live_; RFC-012 aligns the OpenAPI spec).
const APIKeyPrefix = "pulse_sk_"

const (
	apiKeyCacheTTL = 5 * time.Minute // positive cache (RFC-003 5.3)
	apiKeyNegTTL   = 30 * time.Second // negative cache to blunt scans
)

// ErrAPIKeyInvalid is returned when a key is malformed, unknown, or revoked.
var ErrAPIKeyInvalid = errors.New("api key invalid")

// apiKeyStore is the subset of *store.Pool the verify path needs.
type apiKeyStore interface {
	GetAPIKeyByHash(ctx context.Context, tokenHash string) (*domain.APIKey, error)
	TouchAPIKey(ctx context.Context, keyID int64) error
}

// keyCache is the subset of *kv.Client used to cache key descriptors. Implemented
// by *kv.Client. When it is nil the verify goes straight to Postgres.
type keyCache interface {
	GetCache(ctx context.Context, key string) (string, bool, error)
	SetCache(ctx context.Context, key, value string, ttl time.Duration) error
	DelCache(ctx context.Context, key string) error
}

// KeyPrincipal is the resolved principal for an API-key request: the org is fixed
// by the key and the role is stamped on it, so it is fully resolved with no JWT and
// no org header (RFC-003 5.4).
type KeyPrincipal struct {
	KeyID int64
	OrgID int64
	Role  domain.Role
}

// cachedDescriptor is the non-secret key descriptor stored in Redis, keyed by the
// hash so the clear secret is never in Redis either (RFC-003 5.3). Revoked is
// cached as a negative result.
type cachedDescriptor struct {
	KeyID   int64       `json:"key_id"`
	OrgID   int64       `json:"org_id"`
	Role    domain.Role `json:"role"`
	Revoked bool        `json:"revoked"`
}

// APIKeyVerifier verifies API keys with a Redis-cached lookup.
type APIKeyVerifier struct {
	store apiKeyStore
	cache keyCache // may be nil; then DB-only
	now   func() time.Time
}

// NewAPIKeyVerifier builds a verifier. cache may be nil to skip caching.
func NewAPIKeyVerifier(s apiKeyStore, c keyCache) *APIKeyVerifier {
	return &APIKeyVerifier{store: s, cache: c, now: time.Now}
}

func cacheKey(hash string) string { return "apikey:" + hash }

// Verify resolves a presented key to its (org, role) principal (RFC-003 5.3). It
// checks the Redis cache first (a cached revoked descriptor fails fast), falls back
// to Postgres on a miss, caches the result, and refuses an unknown or revoked key.
// If Redis errors it falls through to the DB (fail closed, never fail open).
func (v *APIKeyVerifier) Verify(ctx context.Context, presented string) (*KeyPrincipal, error) {
	if !strings.HasPrefix(presented, APIKeyPrefix) || len(presented) <= len(APIKeyPrefix) {
		return nil, ErrAPIKeyInvalid
	}
	hash := crypto.HashToken(presented)
	ck := cacheKey(hash)

	// 1. Cache lookup. A Redis error is ignored and we fall through to the DB.
	if v.cache != nil {
		if raw, ok, err := v.cache.GetCache(ctx, ck); err == nil && ok {
			var d cachedDescriptor
			if json.Unmarshal([]byte(raw), &d) == nil {
				if d.Revoked {
					return nil, ErrAPIKeyInvalid
				}
				return &KeyPrincipal{KeyID: d.KeyID, OrgID: d.OrgID, Role: d.Role}, nil
			}
		}
	}

	// 2. Postgres source of truth.
	key, err := v.store.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			v.cacheDescriptor(ctx, ck, cachedDescriptor{Revoked: true}, apiKeyNegTTL)
			return nil, ErrAPIKeyInvalid
		}
		return nil, err
	}
	if key.RevokedAt != nil {
		v.cacheDescriptor(ctx, ck, cachedDescriptor{Revoked: true}, apiKeyNegTTL)
		return nil, ErrAPIKeyInvalid
	}

	v.cacheDescriptor(ctx, ck, cachedDescriptor{
		KeyID: key.ID, OrgID: key.OrgID, Role: key.Role,
	}, apiKeyCacheTTL)

	return &KeyPrincipal{KeyID: key.ID, OrgID: key.OrgID, Role: key.Role}, nil
}

func (v *APIKeyVerifier) cacheDescriptor(ctx context.Context, ck string, d cachedDescriptor, ttl time.Duration) {
	if v.cache == nil {
		return
	}
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	_ = v.cache.SetCache(ctx, ck, string(b), ttl)
}

// InvalidateAPIKey busts the cache entry for a hash on revocation so the next
// request misses the cache and re-reads the (now revoked) row (RFC-003 5.3). The
// caller passes the full presented key or its hash via APIKeyCacheKey.
func (v *APIKeyVerifier) InvalidateAPIKey(ctx context.Context, hash string) error {
	if v.cache == nil {
		return nil
	}
	return v.cache.DelCache(ctx, cacheKey(hash))
}

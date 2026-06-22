// Package kv is the fast shared key-value store (Redis): the helpers the platform
// needs across its several Redis roles, a short-lived lock (the per-monitor
// "check now" exclusion, RFC-004), a get/set cache (the entitlement cache,
// RFC-009), and room for rate-limit counters and dedup sets. Named by role (kv)
// rather than by technology, matching store and bus. Bodies are minimal in the
// barebones; the point is that the package and its shape exist for callers.
package kv

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Client is a Redis connection.
type Client struct {
	*goredis.Client
}

// Open dials Redis and verifies it with a ping.
func Open(ctx context.Context, addr string) (*Client, error) {
	c := goredis.NewClient(&goredis.Options{Addr: addr})
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return &Client{Client: c}, nil
}

// Ping reports Redis health for readiness checks. It overrides the embedded
// client's Ping (which returns a command) with the error-returning shape the
// health server wants.
func (c *Client) Ping(ctx context.Context) error {
	return c.Client.Ping(ctx).Err()
}

// AcquireLock takes a lock with SET NX PX. token identifies the holder so only it
// can release. Returns true if acquired, false if already held.
func (c *Client) AcquireLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	return c.SetNX(ctx, key, token, ttl).Result()
}

// releaseScript deletes the key only if the caller still holds it, so a release
// after the ttl expired (and someone else acquired) does not free their lock.
var releaseScript = goredis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// ReleaseLock frees a lock the caller holds (no-op if it no longer holds it).
func (c *Client) ReleaseLock(ctx context.Context, key, token string) error {
	return releaseScript.Run(ctx, c.Client, []string{key}, token).Err()
}

// SetIfAbsent sets key to value with a TTL only if the key does not exist (SET NX
// EX). It returns true when the key was newly set (the caller is first), false when
// it already existed. This is the notify-dedup fast path (RFC-007 4.2): a redelivered
// event finds the key present and is recognized as a duplicate. It is named distinctly
// from the embedded client's SetNX (which returns a command) so the error-returning
// shape callers want is the one they get.
func (c *Client) SetIfAbsent(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return c.SetNX(ctx, key, value, ttl).Result()
}

// TTL returns the remaining time to live of a key. It is negative when the key has
// no expiry (-1) or does not exist (-2), matching Redis semantics; callers that only
// want a positive remaining duration should treat a non-positive value as "expired".
// Used by the check-now cooldown to tell the caller how long to wait (Retry-After).
func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.Client.TTL(ctx, key).Result()
}

// Incr atomically increments the integer at key (creating it at 0 first) and returns
// the new value. The key has no expiry until Expire is called, so a fixed-window
// counter sets the window with Expire on the first increment. Used by the check-now
// sustained-rate layer.
func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	return c.Client.Incr(ctx, key).Result()
}

// Expire sets a time to live on an existing key. Used to start the window on a
// fixed-window counter's first increment, and to keep the check-run hash alive.
func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return c.Client.Expire(ctx, key, ttl).Err()
}

// HSet sets one field of a hash. Used by the check-run state (one field per region).
func (c *Client) HSet(ctx context.Context, key, field, value string) error {
	return c.Client.HSet(ctx, key, field, value).Err()
}

// HGetAll returns every field of a hash as a map (empty map if the key is missing).
// Used to read a whole check-run's per-region state in one call.
func (c *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.Client.HGetAll(ctx, key).Result()
}

// HGetAllMulti reads several hashes in one round-trip via a pipeline, returning a map
// of key -> its fields. Used by the live region-state batch read so a monitors page
// fetches every monitor's per-region state at once. A missing key yields an empty map.
func (c *Client) HGetAllMulti(ctx context.Context, keys []string) (map[string]map[string]string, error) {
	pipe := c.Client.Pipeline()
	cmds := make(map[string]*goredis.MapStringStringCmd, len(keys))
	for _, k := range keys {
		cmds[k] = pipe.HGetAll(ctx, k)
	}
	if _, err := pipe.Exec(ctx); err != nil && err != goredis.Nil {
		return nil, err
	}
	out := make(map[string]map[string]string, len(keys))
	for k, cmd := range cmds {
		m, err := cmd.Result()
		if err != nil {
			return nil, err
		}
		out[k] = m
	}
	return out, nil
}

// GetCache returns the cached string and whether it was present.
func (c *Client) GetCache(ctx context.Context, key string) (string, bool, error) {
	v, err := c.Get(ctx, key).Result()
	if err == goredis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetCache stores a value with a TTL.
func (c *Client) SetCache(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.Set(ctx, key, value, ttl).Err()
}

// DelCache removes a cached key. Used to bust a cache entry on revocation (for
// example an API key, RFC-003 5.3). Deleting a missing key is a no-op.
func (c *Client) DelCache(ctx context.Context, key string) error {
	return c.Del(ctx, key).Err()
}

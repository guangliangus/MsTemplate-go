// Package cache is a two-level read-through cache: a small in-process L1 with a short TTL in front
// of Redis as L2, with single-flight loading so a cold key is computed once per replica rather
// than once per concurrent request (stampede protection). Invalidate after the commit that changed
// the source (pkg/postgres AfterCommit), never inside the transaction.
//
// Consistency across replicas: Remove clears this replica's L1 and the shared L2, but no other
// replica's L1 — nothing broadcasts invalidation. The local TTL therefore bounds how long another
// pod can serve a value that was just changed; keep it short (the default is 30s) and read
// through the database for anything that must be current.
//
// Redis failures are fail-open: a miss is computed from the source and the error is logged.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"example.com/msvcservice/pkg/redisx"
)

// Config is bound from the "cache" section.
type Config struct {
	// DefaultTTL is the L2 lifetime used when a call passes no TTL.
	DefaultTTL time.Duration `koanf:"default_ttl" validate:"min=1s"`
	// LocalTTL caps the L1 lifetime of every entry; the cross-replica staleness window.
	LocalTTL time.Duration `koanf:"local_ttl" validate:"min=1s"`
	// LocalMaxEntries bounds L1 memory (least recently used entries are evicted).
	LocalMaxEntries int `koanf:"local_max_entries" validate:"min=1"`
}

type localEntry struct {
	expires time.Time
	body    []byte
}

// Cache is the two-level cache.
type Cache struct {
	cfg   Config
	local *expirable.LRU[string, localEntry]
	rdb   *redisx.Client
	group singleflight.Group
	log   *slog.Logger
}

// New builds the cache; rdb may be nil for an L1-only cache (tests, a service without Redis).
func New(cfg Config, rdb *redisx.Client, log *slog.Logger) *Cache {
	return &Cache{
		cfg:   cfg,
		local: expirable.NewLRU[string, localEntry](cfg.LocalMaxEntries, nil, cfg.LocalTTL),
		rdb:   rdb,
		log:   log,
	}
}

// GetOrCreate returns the cached value for key or loads, stores and returns it. ttl <= 0 means
// the configured default. Values travel as JSON through both levels, so T must be JSON-encodable
// and callers never share a mutable instance.
func GetOrCreate[T any](ctx context.Context, c *Cache, key string, ttl time.Duration,
	load func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if ttl <= 0 {
		ttl = c.cfg.DefaultTTL
	}
	if entry, ok := c.local.Get(key); ok && time.Now().Before(entry.expires) {
		return decode[T](entry.body)
	}

	body, err, _ := c.group.Do(key, func() (any, error) {
		// The load is shared by every concurrent caller of this key, so it must not end when the
		// caller that happened to start it goes away: it keeps that caller's deadline (the work stays
		// bounded) but not its cancellation (a client disconnect does not fail everyone else).
		ctx, cancel := detach(ctx)
		defer cancel()
		if hit := c.readL2(ctx, key); hit != nil {
			c.setLocal(key, hit, ttl)
			return hit, nil
		}
		value, err := load(ctx)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("cache: encode %s: %w", key, err)
		}
		c.writeL2(ctx, key, encoded, ttl)
		c.setLocal(key, encoded, ttl)
		return encoded, nil
	})
	if err != nil {
		return zero, err
	}
	return decode[T](body.([]byte))
}

// detach keeps ctx's values and deadline but drops its cancellation.
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(base, deadline)
	}
	return context.WithCancel(base)
}

// Remove drops the key from this replica's L1 and from L2.
func (c *Cache) Remove(ctx context.Context, key string) error {
	c.local.Remove(key)
	if c.rdb == nil {
		return nil
	}
	if err := c.rdb.Del(ctx, c.rdb.Key("cache", key)).Err(); err != nil {
		return fmt.Errorf("cache: remove %s: %w", key, err)
	}
	return nil
}

func (c *Cache) setLocal(key string, body []byte, ttl time.Duration) {
	c.local.Add(key, localEntry{expires: time.Now().Add(min(ttl, c.cfg.LocalTTL)), body: body})
}

func (c *Cache) readL2(ctx context.Context, key string) []byte {
	if c.rdb == nil {
		return nil
	}
	body, err := c.rdb.Get(ctx, c.rdb.Key("cache", key)).Bytes()
	switch {
	case err == nil:
		return body
	case errors.Is(err, redis.Nil):
		return nil
	default:
		c.log.WarnContext(ctx, "cache read failed; loading from source",
			slog.String("key", key), slog.Any("error", err))
		return nil
	}
}

func (c *Cache) writeL2(ctx context.Context, key string, body []byte, ttl time.Duration) {
	if c.rdb == nil {
		return
	}
	if err := c.rdb.Set(ctx, c.rdb.Key("cache", key), body, ttl).Err(); err != nil {
		c.log.WarnContext(ctx, "cache write failed; value served uncached",
			slog.String("key", key), slog.Any("error", err))
	}
}

func decode[T any](body []byte) (T, error) {
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		return value, fmt.Errorf("cache: decode: %w", err)
	}
	return value, nil
}

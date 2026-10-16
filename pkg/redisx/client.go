// Package redisx is the service's Redis client plus the primitives built on it: a distributed
// lock, the idempotency store behind the Idempotency-Key middleware and a cross-replica business
// rate limiter. Every key goes through Key, so a shared Redis stays namespaced per service.
//
// Failure semantics are the caller's to choose and to write down: the cache and the business rate
// limiter fail open (there is something underneath them), the idempotency store fails closed
// (there is not). IsStoreFailure is the one classification they all share.
package redisx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
)

// Config is bound from the "redis" section.
type Config struct {
	// Addr is host:port.
	Addr string `koanf:"addr" validate:"required"`
	// Username and Password are optional (ACL users).
	Username string `koanf:"username"`
	Password string `koanf:"password"`
	// DB is the logical database index.
	DB int `koanf:"db" validate:"min=0"`
	// KeyPrefix namespaces every key ("orderservice:"); required and must end with ':'.
	KeyPrefix string `koanf:"key_prefix" validate:"required"`
	// DialTimeout bounds establishing a connection.
	DialTimeout time.Duration `koanf:"dial_timeout" validate:"min=100ms"`
	// ReadTimeout and WriteTimeout bound one command. Short on purpose: an unreachable Redis must
	// surface as a fast error the caller can apply its failure policy to, not as a latency outage
	// for every request.
	ReadTimeout  time.Duration `koanf:"read_timeout" validate:"min=50ms"`
	WriteTimeout time.Duration `koanf:"write_timeout" validate:"min=50ms"`
	// MaxRetries is how many times a command is retried on a transport error (0 = none).
	MaxRetries int `koanf:"max_retries" validate:"min=0"`
	// PoolSize caps connections per replica.
	PoolSize int `koanf:"pool_size" validate:"min=1"`
	// TLS enables TLS to the server with the system roots.
	TLS bool `koanf:"tls"`
}

var keyPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9_.\-]+:$`)

// Validate enforces the key prefix shape.
func (c *Config) Validate() error {
	if !keyPrefixPattern.MatchString(c.KeyPrefix) {
		return fmt.Errorf("key_prefix %q must be letters, digits, '_', '.', '-' and end with ':'", c.KeyPrefix)
	}
	return nil
}

// Client wraps the go-redis client with the key prefix.
type Client struct {
	*redis.Client
	prefix string
}

// Open creates the client. Connections are dialled lazily; the readiness probe reports an
// unreachable server.
func Open(cfg Config, log *slog.Logger) (*Client, error) {
	opts := &redis.Options{
		Addr:         cfg.Addr,
		Username:     cfg.Username,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		MaxRetries:   cfg.MaxRetries,
		PoolSize:     cfg.PoolSize,
	}
	if cfg.TLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	rdb := redis.NewClient(opts)
	if err := redisotel.InstrumentTracing(rdb, redisotel.WithDBStatement(false)); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redisx: tracing: %w", err)
	}
	if err := redisotel.InstrumentMetrics(rdb); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redisx: metrics: %w", err)
	}
	log.Info("redis client created", slog.String("addr", cfg.Addr), slog.String("key_prefix", cfg.KeyPrefix))
	return &Client{Client: rdb, prefix: cfg.KeyPrefix}, nil
}

// Key builds a namespaced key: Key("idem", "abc") -> "<prefix>idem:abc".
func (c *Client) Key(parts ...string) string {
	return c.prefix + strings.Join(parts, ":")
}

// Prefix returns the configured key prefix.
func (c *Client) Prefix() string { return c.prefix }

// Ping doubles as the readiness check.
func (c *Client) Ping(ctx context.Context) error {
	return c.Client.Ping(ctx).Err()
}

// IsStoreFailure reports whether err means the store could not answer (down, timing out, loading,
// read-only, pool exhausted), as opposed to an answer the caller must handle (a missing key, a
// WRONGTYPE that is a bug). Only store failures should trigger a fail-open or fail-closed policy.
func IsStoreFailure(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) {
		return false
	}
	if errors.Is(err, redis.ErrClosed) || errors.Is(err, redis.ErrPoolTimeout) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	for _, prefix := range []string{"LOADING", "READONLY", "MASTERDOWN", "CLUSTERDOWN", "BUSY", "TRYAGAIN", "NOAUTH"} {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return strings.Contains(msg, "max number of clients")
}

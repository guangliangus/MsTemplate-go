package redisx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ErrLockHeld is returned when another owner holds the lock.
var ErrLockHeld = errors.New("redisx: lock is held by another owner")

// Lock is a single-instance Redis lock (SET NX PX with an owner token). It is a best-effort
// mutual exclusion for work that is also idempotent — a Redis failover can hand the lock to two
// owners, which is why the task queue uses SKIP LOCKED in PostgreSQL instead for anything that
// must run exactly once.
type Lock struct {
	client *Client
	key    string
	token  string
}

var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

var extendScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`)

// AcquireLock takes the named lock for ttl, or returns ErrLockHeld.
func (c *Client) AcquireLock(ctx context.Context, name string, ttl time.Duration) (*Lock, error) {
	key := c.Key("lock", name)
	token := uuid.NewString()
	ok, err := c.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("redisx: acquire lock %s: %w", name, err)
	}
	if !ok {
		return nil, ErrLockHeld
	}
	return &Lock{client: c, key: key, token: token}, nil
}

// Release deletes the lock only if this owner still holds it.
func (l *Lock) Release(ctx context.Context) error {
	err := releaseScript.Run(ctx, l.client, []string{l.key}, l.token).Err()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("redisx: release lock: %w", err)
	}
	return nil
}

// Extend pushes the expiry out by ttl while this owner still holds the lock.
func (l *Lock) Extend(ctx context.Context, ttl time.Duration) error {
	n, err := extendScript.Run(ctx, l.client, []string{l.key}, l.token, ttl.Milliseconds()).Int()
	if err != nil {
		return fmt.Errorf("redisx: extend lock: %w", err)
	}
	if n == 0 {
		return ErrLockHeld
	}
	return nil
}

package redisx

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"example.com/msvcservice/pkg/idempotency"
)

func newClient(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	srv := miniredis.RunT(t)
	c, err := Open(Config{Addr: srv.Addr(), KeyPrefix: "svc:", DialTimeout: time.Second, ReadTimeout: time.Second,
		WriteTimeout: time.Second, PoolSize: 2}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, srv
}

func TestConfigValidateKeyPrefix(t *testing.T) {
	for _, bad := range []string{"", "svc", "svc/x:", "svc::x"} {
		cfg := Config{KeyPrefix: bad}
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	cfg := Config{KeyPrefix: "order-svc.v2:"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestKeyIsPrefixed(t *testing.T) {
	c, _ := newClient(t)
	if c.Key("idem", "abc") != "svc:idem:abc" {
		t.Fatal(c.Key("idem", "abc"))
	}
}

func TestLock(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	l1, err := c.AcquireLock(ctx, "job", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AcquireLock(ctx, "job", time.Minute); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second owner must be refused: %v", err)
	}
	if err := l1.Extend(ctx, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AcquireLock(ctx, "job", time.Minute); err != nil {
		t.Fatalf("released lock is free: %v", err)
	}
	if err := l1.Release(ctx); err != nil {
		t.Fatal("releasing somebody else's lock is a no-op, not an error")
	}
}

func TestIdempotencyStoreStates(t *testing.T) {
	c, srv := newClient(t)
	store := NewIdempotencyStore(c)
	ctx := context.Background()

	begin, err := store.TryBegin(ctx, "k", time.Minute)
	if err != nil || begin.Status != idempotency.Claimed {
		t.Fatalf("first claim: %+v %v", begin, err)
	}
	begin, _ = store.TryBegin(ctx, "k", time.Minute)
	if begin.Status != idempotency.InFlight {
		t.Fatalf("second claim while in flight: %+v", begin)
	}
	resp := idempotency.StoredResponse{Status: 201, Headers: map[string]string{"Location": "/x/1"}, Body: []byte(`{"id":1}`)}
	if err := store.Complete(ctx, "k", resp, time.Hour); err != nil {
		t.Fatal(err)
	}
	begin, _ = store.TryBegin(ctx, "k", time.Minute)
	if begin.Status != idempotency.Completed || begin.Response.Status != 201 || string(begin.Response.Body) != `{"id":1}` {
		t.Fatalf("completed: %+v", begin)
	}
	if err := store.Abandon(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	begin, _ = store.TryBegin(ctx, "k", time.Minute)
	if begin.Status != idempotency.Claimed {
		t.Fatal("abandoned key is claimable again")
	}

	// The in-flight claim expires on its own, so a crashed pod cannot pin a key forever.
	srv.FastForward(2 * time.Minute)
	begin, _ = store.TryBegin(ctx, "k", time.Minute)
	if begin.Status != idempotency.Claimed {
		t.Fatalf("expired claim: %+v", begin)
	}
}

func TestFixedWindowAllow(t *testing.T) {
	c, srv := newClient(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		d, err := c.FixedWindowAllow(ctx, "user:1", 2, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if (i <= 2) != d.Allowed {
			t.Fatalf("hit %d allowed=%v", i, d.Allowed)
		}
		if i == 3 && d.RetryAfter <= 0 {
			t.Fatal("a refusal carries the wait")
		}
	}
	if ttl := srv.TTL("svc:ratelimit:user:1"); ttl <= 0 || ttl > time.Minute {
		t.Fatalf("the key must expire with the window: %s", ttl)
	}
	srv.FastForward(2 * time.Minute)
	if d, _ := c.FixedWindowAllow(ctx, "user:1", 2, time.Minute); !d.Allowed {
		t.Fatal("a new window starts fresh")
	}

	srv.Close()
	d, err := c.FixedWindowAllow(ctx, "user:1", 2, time.Minute)
	if err == nil || !d.Allowed {
		t.Fatal("a store failure returns the error AND an allow decision, so callers can fail open")
	}
}

func TestIsStoreFailure(t *testing.T) {
	if IsStoreFailure(nil) || IsStoreFailure(redis.Nil) || IsStoreFailure(errors.New("WRONGTYPE Operation")) {
		t.Fatal("answers and bugs are not store failures")
	}
	if !IsStoreFailure(&net.OpError{Op: "dial", Err: errors.New("refused")}) || !IsStoreFailure(errors.New("LOADING Redis is loading")) ||
		!IsStoreFailure(context.DeadlineExceeded) || !IsStoreFailure(redis.ErrClosed) {
		t.Fatal("outages are store failures")
	}
}

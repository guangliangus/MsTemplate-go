//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"example.com/msvcservice/pkg/idempotency"
	"example.com/msvcservice/pkg/redisx"
	"example.com/msvcservice/pkg/testutil"
)

// Real Redis (not miniredis): SET NX GET needs Redis >= 7 and the scripts need real Lua.
func TestRedisPrimitivesAgainstRealRedis(t *testing.T) {
	addr := testutil.Redis(t)
	client, err := redisx.Open(redisx.Config{Addr: addr, KeyPrefix: "it:", DialTimeout: 2 * time.Second,
		ReadTimeout: time.Second, WriteTimeout: time.Second, PoolSize: 4}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	store := redisx.NewIdempotencyStore(client)
	if b, err := store.TryBegin(ctx, "k", time.Minute); err != nil || b.Status != idempotency.Claimed {
		t.Fatalf("%+v %v", b, err)
	}
	if b, _ := store.TryBegin(ctx, "k", time.Minute); b.Status != idempotency.InFlight {
		t.Fatalf("%+v", b)
	}
	if err := store.Complete(ctx, "k", idempotency.StoredResponse{Status: 200, Body: []byte("x")}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if b, _ := store.TryBegin(ctx, "k", time.Minute); b.Status != idempotency.Completed || string(b.Response.Body) != "x" {
		t.Fatalf("%+v", b)
	}

	lock, err := client.AcquireLock(ctx, "job", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AcquireLock(ctx, "job", time.Minute); err == nil {
		t.Fatal("lock must be exclusive")
	}
	if err := lock.Release(ctx); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 3; i++ {
		d, err := client.FixedWindowAllow(ctx, "s", 2, time.Minute)
		if err != nil || d.Allowed != (i <= 2) {
			t.Fatalf("hit %d: %+v %v", i, d, err)
		}
	}
}

package cache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"example.com/msvcservice/pkg/redisx"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newCache(t *testing.T, localTTL time.Duration) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	srv := miniredis.RunT(t)
	client, err := redisx.Open(redisx.Config{Addr: srv.Addr(), KeyPrefix: "t:", DialTimeout: time.Second,
		ReadTimeout: time.Second, WriteTimeout: time.Second, PoolSize: 4}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return New(Config{DefaultTTL: time.Minute, LocalTTL: localTTL, LocalMaxEntries: 10}, client, quiet), srv
}

type value struct {
	N int `json:"n"`
}

func TestGetOrCreateLoadsOnceAndServesBothLevels(t *testing.T) {
	c, srv := newCache(t, time.Minute)
	ctx := context.Background()
	var loads atomic.Int32
	load := func(context.Context) (value, error) {
		loads.Add(1)
		return value{N: 42}, nil
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := GetOrCreate(ctx, c, "k", 0, load)
			if err != nil || v.N != 42 {
				t.Errorf("%v %v", v, err)
			}
		}()
	}
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("single-flight: loaded %d times", loads.Load())
	}
	if !srv.Exists("t:cache:k") {
		t.Fatal("L2 must hold the value")
	}

	// L1 cleared, L2 still answers: no load.
	c.local.Purge()
	if _, err := GetOrCreate(ctx, c, "k", 0, load); err != nil || loads.Load() != 1 {
		t.Fatal("L2 hit must not load")
	}

	if err := c.Remove(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetOrCreate(ctx, c, "k", 0, load); err != nil || loads.Load() != 2 {
		t.Fatal("after Remove the value is loaded again")
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	c, _ := newCache(t, time.Minute)
	boom := errors.New("boom")
	_, err := GetOrCreate(context.Background(), c, "k", 0, func(context.Context) (value, error) { return value{}, boom })
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	v, err := GetOrCreate(context.Background(), c, "k", 0, func(context.Context) (value, error) { return value{N: 1}, nil })
	if err != nil || v.N != 1 {
		t.Fatal("the next call loads afresh")
	}
}

func TestRedisOutageFailsOpen(t *testing.T) {
	c, srv := newCache(t, time.Minute)
	srv.Close()
	v, err := GetOrCreate(context.Background(), c, "k", 0, func(context.Context) (value, error) { return value{N: 9}, nil })
	if err != nil || v.N != 9 {
		t.Fatalf("the source still answers: %v %v", v, err)
	}
	if err := c.Remove(context.Background(), "k"); err == nil {
		t.Fatal("Remove reports the outage so the caller can log it")
	}
}

func TestShortTTLDoesNotOutliveInL1(t *testing.T) {
	c, _ := newCache(t, time.Hour)
	loads := 0
	load := func(context.Context) (value, error) { loads++; return value{N: loads}, nil }
	if _, err := GetOrCreate(context.Background(), c, "k", 30*time.Millisecond, load); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	// L2 expired too (miniredis honours TTL only on FastForward, so clear it by hand).
	c.rdb.Del(context.Background(), c.rdb.Key("cache", "k"))
	v, _ := GetOrCreate(context.Background(), c, "k", 30*time.Millisecond, load)
	if v.N != 2 {
		t.Fatalf("L1 entry must honour the shorter TTL: got %d", v.N)
	}
}

// The load is shared by every caller of the key: the caller that started it going away (a client
// disconnect) must not fail the others.
func TestASharedLoadOutlivesTheCallerThatStartedIt(t *testing.T) {
	c, _ := newCache(t, time.Minute)
	release := make(chan struct{})
	started := make(chan struct{})
	load := func(ctx context.Context) (value, error) {
		close(started)
		select {
		case <-release:
			return value{N: 7}, nil
		case <-ctx.Done():
			return value{}, ctx.Err()
		}
	}
	first, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := GetOrCreate(first, c, "k", 0, load)
		firstDone <- err
	}()
	<-started
	second := make(chan value, 1)
	go func() {
		v, _ := GetOrCreate(context.Background(), c, "k", 0, load)
		second <- v
	}()
	time.Sleep(20 * time.Millisecond) // the second caller joins the flight
	cancelFirst()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if v := <-second; v.N != 7 {
		t.Fatalf("the second caller got %+v", v)
	}
	<-firstDone
}

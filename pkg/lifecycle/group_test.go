package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type recorder struct {
	mu    sync.Mutex
	order []string
}

func (r *recorder) runner(name string) RunnerFunc {
	return func(ctx context.Context) error {
		<-ctx.Done()
		r.mu.Lock()
		r.order = append(r.order, name)
		r.mu.Unlock()
		return nil
	}
}

func TestStopsInReverseOrder(t *testing.T) {
	rec := &recorder{}
	g := NewGroup(quiet, 2*time.Second)
	g.Add("first", rec.runner("first"))
	g.Add("second", rec.runner("second"))
	g.Add("third", rec.runner("third"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := g.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.order; len(got) != 3 || got[0] != "third" || got[2] != "first" {
		t.Fatalf("stop order %v", got)
	}
}

func TestFailureStopsTheGroup(t *testing.T) {
	rec := &recorder{}
	g := NewGroup(quiet, time.Second)
	g.Add("ok", rec.runner("ok"))
	boom := errors.New("boom")
	g.Add("bad", RunnerFunc(func(context.Context) error { return boom }))

	err := g.Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("expected the failure, got %v", err)
	}
	if len(rec.order) != 1 {
		t.Fatal("the healthy runner must have been stopped")
	}
}

func TestEarlyNilExitIsNotAFailure(t *testing.T) {
	g := NewGroup(quiet, time.Second)
	g.Add("one-shot", RunnerFunc(func(context.Context) error { return nil }))
	g.Add("long", RunnerFunc(func(ctx context.Context) error { <-ctx.Done(); return nil }))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownBudgetIsBounded(t *testing.T) {
	g := NewGroup(quiet, 50*time.Millisecond)
	g.Add("stuck", RunnerFunc(func(context.Context) error { select {} }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_ = g.Run(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("a stuck runner must not hold the shutdown")
	}
}

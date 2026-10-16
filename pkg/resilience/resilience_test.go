package resilience

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestBackoffStaysWithinBounds(t *testing.T) {
	base, maxDelay := 100*time.Millisecond, time.Second
	for attempt := 1; attempt <= 8; attempt++ {
		for range 50 {
			d := Backoff(attempt, base, maxDelay)
			capped := min(base<<(attempt-1), maxDelay)
			if d < capped/2 || d > capped {
				t.Fatalf("attempt %d: %s outside [%s, %s]", attempt, d, capped/2, capped)
			}
		}
	}
}

func TestRetryStopsOnNonTransientAndHonoursBudget(t *testing.T) {
	transientErr := errors.New("transient")
	fatal := errors.New("fatal")
	classify := func(err error) bool { return errors.Is(err, transientErr) }
	cfg := RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}

	calls := 0
	err := Retry(context.Background(), "t", cfg, classify, func(context.Context) error {
		calls++
		return transientErr
	})
	if !errors.Is(err, transientErr) || calls != 3 {
		t.Fatalf("budget: err=%v calls=%d", err, calls)
	}

	calls = 0
	err = Retry(context.Background(), "t", cfg, classify, func(context.Context) error {
		calls++
		return fatal
	})
	if !errors.Is(err, fatal) || calls != 1 {
		t.Fatalf("non-transient must not retry: err=%v calls=%d", err, calls)
	}

	calls = 0
	err = Retry(context.Background(), "t", cfg, classify, func(context.Context) error {
		calls++
		if calls < 2 {
			return transientErr
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("success after a retry: err=%v calls=%d", err, calls)
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := RetryConfig{MaxAttempts: 10, BaseDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond}
	calls := 0
	err := Retry(ctx, "t", cfg, func(error) bool { return true }, func(context.Context) error {
		calls++
		cancel()
		return errors.New("again")
	})
	if err == nil || calls != 1 {
		t.Fatalf("cancelled context must stop the loop: err=%v calls=%d", err, calls)
	}
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cb := NewBreaker[int]("t", BreakerConfig{Enabled: true, FailureThreshold: 2, OpenDuration: time.Minute, HalfOpenMaxRequests: 1}, log)
	boom := errors.New("boom")
	for range 2 {
		_, _ = Execute(cb, func() (int, error) { return 0, boom })
	}
	_, err := Execute(cb, func() (int, error) { return 1, nil })
	if !IsBreakerOpen(err) {
		t.Fatalf("expected the breaker to be open, got %v", err)
	}
	if v, err := Execute[int](nil, func() (int, error) { return 7, nil }); err != nil || v != 7 {
		t.Fatal("a nil breaker is pass-through")
	}
}

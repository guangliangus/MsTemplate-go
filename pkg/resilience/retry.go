// Package resilience is retry with backoff and a circuit breaker for everything that is not an
// HTTP client (which has its own pipeline in pkg/httpclient): Redis calls, raw SQL, external SDKs.
//
// The value is not the retry count but the classification: a unique violation will never succeed
// on retry, a serialization failure is exactly what retry is for. The classifier is the caller's
// (pkg/postgres.IsTransient, pkg/redisx.IsStoreFailure); this package only runs the loop. And a
// classifier cannot know whether the OPERATION is safe to repeat — after a timeout no predicate
// knows whether the first attempt committed — so retry reads, ON CONFLICT DO NOTHING writes and
// writes with an idempotency key, never a bare INSERT or a payment.
package resilience

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RetryConfig is bound from a "retry" subsection.
type RetryConfig struct {
	// MaxAttempts counts the first try; 1 means no retry.
	MaxAttempts int `koanf:"max_attempts" validate:"min=1"`
	// BaseDelay is the first backoff; it doubles per attempt up to MaxDelay, with jitter.
	BaseDelay time.Duration `koanf:"base_delay" validate:"min=0"`
	// MaxDelay caps the backoff.
	MaxDelay time.Duration `koanf:"max_delay" validate:"min=0"`
}

// DefaultRetry is used for zero-valued fields.
var DefaultRetry = RetryConfig{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: 2 * time.Second}

func (c RetryConfig) withDefaults() RetryConfig {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = DefaultRetry.MaxAttempts
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = DefaultRetry.BaseDelay
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = DefaultRetry.MaxDelay
	}
	return c
}

// Classifier reports whether an error is transient (worth another attempt).
type Classifier func(error) bool

var retries = sync.OnceValue(func() metric.Int64Counter {
	counter, _ := otel.Meter("msvc/resilience").Int64Counter("msvc.resilience.retries",
		metric.WithDescription("Retried attempts, by operation name."))
	return counter
})

// Retry runs op until it succeeds, fails non-transiently, exhausts the attempts or the context
// ends. name tags the retry metric.
func Retry(ctx context.Context, name string, cfg RetryConfig, transient Classifier,
	op func(ctx context.Context) error) error {
	cfg = cfg.withDefaults()
	var err error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		err = op(ctx)
		if err == nil || !transient(err) || attempt == cfg.MaxAttempts {
			return err
		}
		retries().Add(ctx, 1, metric.WithAttributes(attribute.String("operation", name)))
		if sleepErr := sleep(ctx, Backoff(attempt, cfg.BaseDelay, cfg.MaxDelay)); sleepErr != nil {
			return err
		}
	}
	return err
}

// Backoff is exponential with equal jitter: the delay for attempt n lies in [cap/2, cap] where
// cap = min(base * 2^(n-1), maxDelay), so retries from many replicas spread out instead of
// hammering a recovering dependency in lockstep.
func Backoff(attempt int, base, maxDelay time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	capped := base
	for i := 1; i < attempt && capped < maxDelay; i++ {
		capped *= 2
	}
	capped = min(capped, maxDelay)
	half := capped / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not security
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

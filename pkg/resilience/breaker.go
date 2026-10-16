package resilience

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// BreakerConfig is bound from a "breaker" subsection.
//
// Breakers are ON by default for a single downstream (pkg/httpclient) and OFF by default for a
// shared resource: a breaker in front of the database or Redis is opened by one pathological
// caller and then refuses every unrelated call, turning a local failure into a global one. The
// blast radius of a breaker is whatever is behind it; put it in front of one dependency.
type BreakerConfig struct {
	Enabled bool `koanf:"enabled"`
	// FailureThreshold is the consecutive failures that open the breaker.
	FailureThreshold uint32 `koanf:"failure_threshold" validate:"min=1"`
	// OpenDuration is how long the breaker stays open before letting probes through.
	OpenDuration time.Duration `koanf:"open_duration" validate:"min=1s"`
	// HalfOpenMaxRequests is how many probes the half-open state admits.
	HalfOpenMaxRequests uint32 `koanf:"half_open_max_requests" validate:"min=1"`
	// Interval resets the closed-state counts so old failures do not accumulate forever (0 = never).
	Interval time.Duration `koanf:"interval" validate:"min=0"`
}

// Errors the breaker returns instead of calling the operation.
var (
	ErrBreakerOpen         = gobreaker.ErrOpenState
	ErrBreakerTooManyProbe = gobreaker.ErrTooManyRequests
)

// IsBreakerOpen reports whether err came from the breaker rather than the operation.
func IsBreakerOpen(err error) bool {
	return errors.Is(err, ErrBreakerOpen) || errors.Is(err, ErrBreakerTooManyProbe)
}

var stateChanges = sync.OnceValue(func() metric.Int64Counter {
	counter, _ := otel.Meter("msvc/resilience").Int64Counter("msvc.resilience.breaker_state_changes",
		metric.WithDescription("Circuit breaker transitions, by breaker name and new state."))
	return counter
})

// BreakerOption tunes a breaker beyond its configuration section.
type BreakerOption func(*gobreaker.Settings)

// IgnoreErrors keeps the errors excluded reports out of the breaker's counts: neither a success nor
// a failure. The canonical use is the caller's own cancellation, which says nothing about the
// dependency's health — counting it lets a burst of client disconnects open the breaker on a
// healthy downstream.
func IgnoreErrors(excluded func(error) bool) BreakerOption {
	return func(s *gobreaker.Settings) { s.IsExcluded = excluded }
}

// NewBreaker builds a generic breaker; a disabled config returns nil, and Execute treats nil as
// pass-through.
func NewBreaker[T any](name string, cfg BreakerConfig, log *slog.Logger, opts ...BreakerOption,
) *gobreaker.CircuitBreaker[T] {
	if !cfg.Enabled {
		return nil
	}
	settings := gobreaker.Settings{
		Name:        name,
		MaxRequests: cfg.HalfOpenMaxRequests,
		Interval:    cfg.Interval,
		Timeout:     cfg.OpenDuration,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= cfg.FailureThreshold
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			log.Warn("circuit breaker state changed",
				slog.String("breaker", name), slog.String("from", from.String()), slog.String("to", to.String()))
			stateChanges().Add(context.Background(), 1, metric.WithAttributes(
				attribute.String("breaker", name), attribute.String("to", to.String())))
		},
	}
	for _, opt := range opts {
		opt(&settings)
	}
	return gobreaker.NewCircuitBreaker[T](settings)
}

// Execute runs op through the breaker, or directly when cb is nil.
func Execute[T any](cb *gobreaker.CircuitBreaker[T], op func() (T, error)) (T, error) {
	if cb == nil {
		return op()
	}
	return cb.Execute(op)
}

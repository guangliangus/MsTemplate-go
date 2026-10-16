package messaging

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Instrumentation names; the Prometheus exporter renders them as msvc_messaging_published_total,
// msvc_messaging_consumed_total{queue,outcome}, msvc_messaging_consume_duration_seconds,
// msvc_messaging_outbox_dispatched_total{outcome} and msvc_messaging_outbox_pending — the same
// names every service on the platform exports, so one dashboard reads them all.
const scopeName = "msvc/messaging"

// OutcomeBrokerUnavailable tags an outbox row whose publish found the broker unavailable: the row
// stays PENDING with its attempt budget untouched.
const OutcomeBrokerUnavailable = "broker_unavailable"

// Outcomes tagged on the consumed counter.
const (
	OutcomeOK         = "ok"
	OutcomeDuplicate  = "duplicate"
	OutcomeRetry      = "retry"
	OutcomeDeadLetter = "dead_letter"
	OutcomeRequeued   = "requeued"
)

// Reasons on the dead-lettered counter: exhausted retries mean a dependency stayed down for the
// whole ladder and deserve an alert; a non-retryable rejection is the handler saying the message
// itself is hopeless, which is routine and must not drown the former.
const (
	DeadLetterExhausted    = "exhausted"
	DeadLetterNonRetryable = "non_retryable"
)

type instruments struct {
	published       metric.Int64Counter
	consumed        metric.Int64Counter
	consumeDuration metric.Float64Histogram
	deadLettered    metric.Int64Counter
	dispatched      metric.Int64Counter
	outboxPending   atomic.Int64
	outboxOldestAge atomic.Uint64 // float64 bits
}

var metrics = sync.OnceValue(func() *instruments {
	m := otel.Meter(scopeName)
	in := &instruments{}
	in.published, _ = m.Int64Counter("msvc.messaging.published",
		metric.WithDescription("Events published to the broker, by event name."))
	in.consumed, _ = m.Int64Counter("msvc.messaging.consumed",
		metric.WithDescription("Messages consumed, by queue and outcome (ok|duplicate|retry|dead_letter|requeued)."))
	in.consumeDuration, _ = m.Float64Histogram("msvc.messaging.consume.duration",
		metric.WithUnit("s"), metric.WithDescription("Handler execution time, by queue."))
	in.deadLettered, _ = m.Int64Counter("msvc.messaging.dead_lettered",
		metric.WithDescription("Messages sent to the dead-letter queue, by queue and reason "+
			"(exhausted|non_retryable)."))
	in.dispatched, _ = m.Int64Counter("msvc.messaging.outbox_dispatched",
		metric.WithDescription("Outbox rows processed, by outcome (published|retry|failed|broker_unavailable)."))
	_, _ = m.Int64ObservableGauge("msvc.messaging.outbox_pending",
		metric.WithDescription("Outbox rows waiting for dispatch (sampled every 10s, apart from dispatching). "+
			"Every replica reports the same global value: aggregate with max, not sum."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(in.outboxPending.Load())
			return nil
		}))
	_, _ = m.Float64ObservableGauge("msvc.messaging.outbox_oldest_pending_age", metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest PENDING outbox row: how late the next event already is. "+
			"Aggregate with max, not sum."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(math.Float64frombits(in.outboxOldestAge.Load()))
			return nil
		}))
	return in
})

// Tracer is the messaging tracer.
func Tracer() trace.Tracer { return otel.Tracer(scopeName) }

// RecordPublished counts a publish.
func RecordPublished(ctx context.Context, eventName string) {
	metrics().published.Add(ctx, 1, metric.WithAttributes(attribute.String("event", eventName)))
}

// RecordConsumed counts a consumed message with its outcome and handler duration in seconds.
func RecordConsumed(ctx context.Context, queue, outcome string, seconds float64) {
	in := metrics()
	in.consumed.Add(ctx, 1, metric.WithAttributes(
		attribute.String("queue", queue), attribute.String("outcome", outcome)))
	in.consumeDuration.Record(ctx, seconds, metric.WithAttributes(attribute.String("queue", queue)))
}

// RecordDispatched counts an outbox row outcome.
func RecordDispatched(ctx context.Context, outcome string) {
	metrics().dispatched.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// RecordDeadLettered counts a message sent to the dead-letter queue with its reason.
func RecordDeadLettered(ctx context.Context, queue, reason string) {
	metrics().deadLettered.Add(ctx, 1, metric.WithAttributes(
		attribute.String("queue", queue), attribute.String("reason", reason)))
}

// SetOutboxPending publishes the latest backlog sample: how many rows wait, and for how long the
// oldest has.
func SetOutboxPending(n int64, oldestAgeSeconds float64) {
	metrics().outboxPending.Store(n)
	metrics().outboxOldestAge.Store(math.Float64bits(oldestAgeSeconds))
}

package taskqueue

import (
	"context"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Six counters answer "what happened to the work" and two gauges "how much is waiting, and for
// how long": a backlog that grows while completed is flat means the pod stopped working; a backlog
// that grows while failed or retried climbs means it is working and losing (failed: the handler
// errored or panicked; retried: it asked to run again); reclaimed means a worker went away holding
// a claim (a crash, or a drain that ran out of time); misfired climbing means
// scheduled firings are found late, which is the queue having been off or saturated. The oldest
// pending age is the SLO signal: depth says how many are waiting, age says how late the next one
// already is. Exported as msvc_tasks_*.
const scopeName = "msvc/taskqueue"

// Reasons on the dead counter: an exhausted attempt budget is the one worth an alert; a handler
// discarding a task it recognised as hopeless is routine and would drown it.
const (
	DeadReasonExhausted = "exhausted"
	DeadReasonDiscarded = "discarded"
)

type instruments struct {
	completed metric.Int64Counter
	retried   metric.Int64Counter
	failed    metric.Int64Counter
	dead      metric.Int64Counter
	reclaimed metric.Int64Counter
	misfired  metric.Int64Counter
	stats     atomic.Pointer[map[string]QueueStats]
}

var metrics = sync.OnceValue(func() *instruments {
	m := otel.Meter(scopeName)
	in := &instruments{}
	in.completed, _ = m.Int64Counter("msvc.tasks.completed",
		metric.WithDescription("Tasks that finished and had their row removed (or re-armed, for recurring tasks)."))
	in.retried, _ = m.Int64Counter("msvc.tasks.retried",
		metric.WithDescription("Tasks re-armed to run again after a handler asked to retry."))
	in.failed, _ = m.Int64Counter("msvc.tasks.failed",
		metric.WithDescription("Handler runs that returned an error or panicked; each is re-armed with backoff "+
			"until the attempt budget is spent."))
	in.dead, _ = m.Int64Counter("msvc.tasks.dead",
		metric.WithDescription("Tasks given up on, by reason: discarded by the handler, or exhausted attempt budget."))
	in.reclaimed, _ = m.Int64Counter("msvc.tasks.reclaimed",
		metric.WithDescription("Claims released by the fixer because the worker holding them went away "+
			"(a crash, or a shutdown drain that ran out of time)."))
	in.misfired, _ = m.Int64Counter("msvc.tasks.misfired",
		metric.WithDescription("Cron firings claimed later than the misfire threshold, by policy."))
	empty := map[string]QueueStats{}
	in.stats.Store(&empty)
	_, _ = m.Int64ObservableGauge("msvc.tasks.queue_depth",
		metric.WithDescription("Claimable tasks per queue: unclaimed and already due (sampled by the fixer loop). "+
			"Every replica reports the same global value: aggregate with max, not sum."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			for queue, s := range *in.stats.Load() {
				o.Observe(s.Depth, metric.WithAttributes(attribute.String("queue", queue)))
			}
			return nil
		}))
	_, _ = m.Float64ObservableGauge("msvc.tasks.oldest_pending_age", metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest claimable task per queue: how late the next task already is. "+
			"Aggregate with max, not sum."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for queue, s := range *in.stats.Load() {
				o.Observe(s.OldestAge.Seconds(), metric.WithAttributes(attribute.String("queue", queue)))
			}
			return nil
		}))
	return in
})

func tracer() trace.Tracer { return otel.Tracer(scopeName) }

func queueAttr(queue string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("queue", queue))
}

func recordDead(ctx context.Context, queue, reason string) {
	metrics().dead.Add(ctx, 1, metric.WithAttributes(
		attribute.String("queue", queue), attribute.String("reason", reason)))
}

func setQueueStats(stats map[string]QueueStats) {
	metrics().stats.Store(&stats)
}

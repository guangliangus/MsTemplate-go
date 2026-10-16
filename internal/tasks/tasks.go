// Package tasks holds the service's background tasks on the database task queue: a recurring
// (interval) task and a cron (wall-clock) task as references. Each firing is claimed by exactly
// one replica; a failed run is retried with backoff, and the fixer covers crashes.
package tasks

import (
	"context"
	"log/slog"
	"time"

	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/pkg/taskqueue"
)

// PendingOrdersReaper cancels orders that stayed PENDING too long. Recurring: re-armed to
// now + interval after each run ("tasks.recurring.pending-orders-reaper").
type PendingOrdersReaper struct {
	orders  *order.Service
	timeout time.Duration
	log     *slog.Logger
}

// NewPendingOrdersReaper builds the task.
func NewPendingOrdersReaper(svc *order.Service, pendingTimeout time.Duration, log *slog.Logger) *PendingOrdersReaper {
	return &PendingOrdersReaper{orders: svc, timeout: pendingTimeout, log: log}
}

// Handle cancels one batch; a failure is retried with backoff (at-least-once: the batch is
// idempotent, a cancelled order is no longer PENDING).
func (t *PendingOrdersReaper) Handle(ctx context.Context, task taskqueue.Task) (taskqueue.Result, error) {
	n, err := t.orders.ReapPending(ctx, t.timeout)
	if err != nil {
		return taskqueue.Result{}, err
	}
	if n > 0 {
		t.log.InfoContext(ctx, "cancelled stale pending orders",
			slog.Int64("count", n), slog.Int("attempt", task.Attempt))
	}
	return taskqueue.Done(), nil
}

// Heartbeat is the cron reference: it only logs that the schedule fired, with the misfire flag.
type Heartbeat struct {
	log *slog.Logger
}

// NewHeartbeat builds the task.
func NewHeartbeat(log *slog.Logger) *Heartbeat {
	return &Heartbeat{log: log}
}

// Handle logs the firing.
func (t *Heartbeat) Handle(ctx context.Context, task taskqueue.Task) (taskqueue.Result, error) {
	t.log.InfoContext(ctx, "heartbeat fired", slog.Time("scheduled_for", task.DeliverOn),
		slog.Bool("misfired", task.Misfired), slog.Int("attempt", task.Attempt))
	return taskqueue.Done(), nil
}

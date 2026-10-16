package taskqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"example.com/msvcservice/pkg/actor"
	"example.com/msvcservice/pkg/resilience"
)

type registration struct {
	queue    string
	handler  Handler
	interval time.Duration
	cron     *CronSchedule
}

func (r registration) recurring() bool { return r.interval > 0 || r.cron != nil }

// Runner polls the registered queues: one loop per queue plus the fixer loop. Each loop claims up
// to its free worker slots (short poll while saturated, long poll while idle) and hands tasks to
// the handler. On shutdown it stops claiming and drains what is already claimed for at most
// DrainTimeout; anything left stays claimed for the fixer.
type Runner struct {
	store    *Store
	cfg      Config
	log      *slog.Logger
	runnerID string
	regs     []registration
	inFlight atomic.Int32
}

// NewRunner builds a runner with no registrations.
func NewRunner(store *Store, cfg Config, log *slog.Logger) *Runner {
	host, _ := os.Hostname()
	return &Runner{
		store:    store,
		cfg:      cfg,
		log:      log,
		runnerID: host + "-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
}

// Handle binds a one-off queue to a handler; work arrives through Store.Enqueue.
func (r *Runner) Handle(queue string, h Handler) error {
	return r.register(registration{queue: queue, handler: h})
}

// Recurring registers a self-perpetuating task: one row per name, seeded at startup and re-armed
// to now + interval after each run. The interval is measured from the end of a run, so it is a
// guaranteed gap, not a wall-clock schedule; "tasks.recurring.<name>" overrides defaultEvery.
func (r *Runner) Recurring(name string, defaultEvery time.Duration, h Handler) error {
	every := defaultEvery
	if override, ok := r.cfg.Recurring[name]; ok && override > 0 {
		every = override
	}
	if every <= 0 {
		return fmt.Errorf("taskqueue: recurring task %q needs a positive interval", name)
	}
	return r.register(registration{queue: name, handler: h, interval: every})
}

// CronOption tunes a cron registration.
type CronOption func(*cronSpec)

type cronSpec struct {
	timeZone string
	misfire  MisfirePolicy
}

// WithTimeZone reads the expression in an IANA zone.
func WithTimeZone(tz string) CronOption { return func(s *cronSpec) { s.timeZone = tz } }

// WithMisfire sets the policy for a firing found late.
func WithMisfire(p MisfirePolicy) CronOption { return func(s *cronSpec) { s.misfire = p } }

// Cron registers a wall-clock schedule: one row per name, seeded for the next occurrence and
// re-armed to the following one after each run. "tasks.cron.<name>" overrides the defaults.
func (r *Runner) Cron(name, defaultExpression string, h Handler, opts ...CronOption) error {
	spec := cronSpec{}
	for _, o := range opts {
		o(&spec)
	}
	expression := defaultExpression
	if override, ok := r.cfg.Cron[name]; ok {
		if override.Expression != "" {
			expression = override.Expression
		}
		if override.TimeZone != "" {
			spec.timeZone = override.TimeZone
		}
		if override.Misfire != "" {
			spec.misfire = MisfirePolicy(override.Misfire)
		}
	}
	schedule, err := ParseCron(expression, spec.timeZone, spec.misfire)
	if err != nil {
		return fmt.Errorf("taskqueue: cron task %q: %w", name, err)
	}
	return r.register(registration{queue: name, handler: h, cron: schedule})
}

func (r *Runner) register(reg registration) error {
	for _, existing := range r.regs {
		if existing.queue == reg.queue {
			return fmt.Errorf("taskqueue: queue %q has more than one handler registered", reg.queue)
		}
	}
	r.regs = append(r.regs, reg)
	return nil
}

// Run polls until ctx is cancelled. With max_workers = 0 it logs one line and returns at once.
func (r *Runner) Run(ctx context.Context) error {
	if len(r.regs) == 0 {
		return nil
	}
	if r.cfg.MaxWorkers == 0 {
		r.log.Info("task queue disabled on this host (tasks.max_workers=0)", slog.Int("queues", len(r.regs)))
		return nil
	}
	var wg sync.WaitGroup
	for _, reg := range r.regs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.queueLoop(ctx, reg)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.fixerLoop(ctx)
	}()
	wg.Wait()
	if n := r.inFlight.Load(); n > 0 {
		r.log.Warn("task queue drain gave up with tasks still running; "+
			"their rows stay claimed and the fixer will re-arm them",
			slog.Int("in_flight", int(n)), slog.Duration("drain_timeout", r.cfg.DrainTimeout),
			slog.Duration("stale_after", r.cfg.StaleAfter))
	}
	return nil
}

func (r *Runner) queueLoop(ctx context.Context, reg registration) {
	settings := r.cfg.settingsFor(reg.queue)
	if settings.maxWorkers == 0 {
		r.log.Info("task queue disabled on this host (per-queue max_workers=0)", slog.String("queue", reg.queue))
		return
	}
	if reg.recurring() && !r.seed(ctx, reg, settings) {
		return
	}

	base := context.WithoutCancel(ctx)
	slots := make(chan struct{}, settings.maxWorkers)
	var wg sync.WaitGroup
	for ctx.Err() == nil {
		free := settings.maxWorkers - len(slots)
		if free < 1 {
			sleep(ctx, settings.shortPoll)
			continue
		}
		tasks, err := r.store.Pop(ctx, reg.queue, free, r.runnerID)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("task queue poll failed", slog.String("queue", reg.queue), slog.Any("error", err))
				sleep(ctx, settings.shortPoll)
			}
			continue
		}
		if len(tasks) == 0 {
			sleep(ctx, settings.longPoll)
			continue
		}
		for _, task := range tasks {
			slots <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() {
					<-slots
					wg.Done()
				}()
				r.execute(base, reg, settings, task)
			}()
		}
	}
	r.drain(&wg)
}

// seed inserts the recurring row (idempotent on UNIQUE(queue_name, task_id)): every replica tries,
// one insert wins, and the row re-arms itself forever. Retried rather than fatal — a failure here
// must degrade, not stop the host.
func (r *Runner) seed(ctx context.Context, reg registration, settings queueSettings) bool {
	for ctx.Err() == nil {
		err := r.seedOnce(ctx, reg)
		if err == nil {
			return true
		}
		r.log.Error("seeding the recurring task failed; retrying",
			slog.String("queue", reg.queue), slog.Any("error", err))
		sleep(ctx, settings.longPoll)
	}
	return false
}

func (r *Runner) seedOnce(ctx context.Context, reg registration) error {
	ctx = actor.WithName(ctx, actor.System)
	req := EnqueueRequest{QueueName: reg.queue, TaskID: reg.queue}
	if reg.cron == nil {
		_, err := r.store.Enqueue(ctx, req)
		return err
	}
	next := reg.cron.NextAfter(time.Now())
	req.DeliverOn = next
	if _, err := r.store.Enqueue(ctx, req); err != nil {
		return err
	}
	return r.store.RescheduleIfPending(ctx, reg.queue, reg.queue, next)
}

func (r *Runner) drain(wg *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(r.cfg.DrainTimeout):
	}
}

func (r *Runner) execute(base context.Context, reg registration, settings queueSettings, task Task) {
	r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	log := r.log.With(slog.String("queue", task.QueueName), slog.String("task_id", task.TaskID),
		slog.Int("attempt", task.Attempt))

	// The handler's ONLY cancellation source: the task timeout, validated to be strictly below
	// stale_after, so a handler is always cancelled before the fixer can hand its row away. The
	// span continues the producer's trace when the payload carries one.
	ctx, cancel := context.WithTimeout(actor.WithName(base, actor.System), r.cfg.TaskTimeout)
	defer cancel()
	ctx, span := tracer().Start(extractTrace(ctx, task.Payload), "task "+task.QueueName,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attribute.String("task.queue", task.QueueName),
			attribute.String("task.id", task.TaskID), attribute.Int("task.attempt", task.Attempt)))
	defer span.End()
	storeCtx, cancelStore := context.WithTimeout(actor.WithName(base, actor.System), 30*time.Second)
	defer cancelStore()

	if reg.cron != nil && task.Attempt == 1 {
		if skip := r.applyMisfire(storeCtx, reg, &task, log); skip {
			return
		}
	}

	result, err := r.invoke(ctx, reg, task, log)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s: %w", r.cfg.TaskTimeout, err)
		}
		if err := r.retryAfterFailure(storeCtx, reg, settings, task, err, log); err != nil {
			log.Error("recording the task failure failed; the fixer will re-arm it", slog.Any("error", err))
		}
		return
	}
	if err := r.settle(storeCtx, reg, settings, task, result); err != nil {
		log.Error("recording the task result failed; the fixer will re-arm it", slog.Any("error", err))
		return
	}
	if result.ErrorCode != "" || result.ErrorMessage != "" {
		span.SetAttributes(attribute.String("task.error_code", result.ErrorCode))
		log.Warn("task result", slog.String("action", result.Action.String()),
			slog.String("error_code", result.ErrorCode), slog.String("error_message", result.ErrorMessage))
	}
}

// invoke runs the handler, turning a panic into an error: a panicking task takes the failure path
// (backoff, attempt budget) instead of crashing the replica that claimed it — and, after the fixer
// hands the row on, the next one.
func (r *Runner) invoke(ctx context.Context, reg registration, task Task, log *slog.Logger,
) (result Result, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("task handler panicked", slog.Any("panic", rec), slog.String("stack", string(debug.Stack())))
			result, err = Result{}, fmt.Errorf("taskqueue: handler panicked: %v", rec)
		}
	}()
	return reg.handler.Handle(ctx, task)
}

// retryAfterFailure re-arms a task whose handler failed with exponential backoff, or dead-letters a
// one-off task that has used its attempt budget. Recurring tasks are exempt from the budget; their
// growing attempt count keeps the backoff growing until a run succeeds and resets it.
func (r *Runner) retryAfterFailure(ctx context.Context, reg registration, settings queueSettings, task Task,
	failure error, log *slog.Logger) error {
	metrics().failed.Add(ctx, 1, queueAttr(task.QueueName))
	if budgetExhausted(reg, settings, task) {
		recordDead(ctx, task.QueueName, DeadReasonExhausted)
		log.Error("task dead: still failing after the attempt budget",
			slog.Int("max_attempts", settings.maxAttempts), slog.Any("error", failure))
		_, err := r.store.Complete(ctx, task.QueueName, task.TaskID)
		return err
	}
	delay := resilience.Backoff(task.Attempt, r.cfg.RetryBaseDelay, r.cfg.RetryMaxDelay)
	log.Error("task failed; retrying after backoff", slog.Duration("retry_in", delay), slog.Any("error", failure))
	return r.store.ReArm(ctx, task.QueueName, task.TaskID, delay, false)
}

// applyMisfire decides a late cron firing before any handler runs, on the first claim only: a
// reclaim or retry is the same firing coming back, and a policy that skipped it would drop work it
// had already decided to do.
func (r *Runner) applyMisfire(ctx context.Context, reg registration, task *Task, log *slog.Logger) bool {
	lateness := time.Since(task.DeliverOn)
	if lateness <= r.cfg.MisfireThreshold {
		return false
	}
	task.Misfired = true
	metrics().misfired.Add(ctx, 1, metric.WithAttributes(
		attribute.String("queue", task.QueueName), attribute.String("policy", string(reg.cron.Misfire))))
	if reg.cron.Misfire != MisfireSkip {
		log.Warn("cron task misfired; running now", slog.Time("scheduled_for", task.DeliverOn),
			slog.Duration("late_by", lateness), slog.String("policy", string(reg.cron.Misfire)))
		return false
	}
	next := reg.cron.NextAfter(time.Now())
	if err := r.store.ReArmAt(ctx, task.QueueName, task.TaskID, next, true); err != nil {
		log.Error("re-arming the skipped cron task failed; the fixer will re-arm it", slog.Any("error", err))
		return true
	}
	log.Warn("cron task misfired; policy skip, not run", slog.Time("scheduled_for", task.DeliverOn),
		slog.Duration("late_by", lateness), slog.Time("next", next))
	return true
}

func (r *Runner) settle(ctx context.Context, reg registration, settings queueSettings, task Task, result Result) error {
	queue := queueAttr(task.QueueName)
	switch {
	case (result.Action == ActionDone || result.Action == ActionSettled) && reg.cron != nil:
		// A recurring row is its own schedule: re-armed whichever way the handler finished.
		metrics().completed.Add(ctx, 1, queue)
		next := reg.cron.NextFireTime(task.DeliverOn, time.Now())
		return r.store.ReArmAt(ctx, task.QueueName, task.TaskID, next, true)
	case (result.Action == ActionDone || result.Action == ActionSettled) && reg.interval > 0:
		metrics().completed.Add(ctx, 1, queue)
		return r.store.ReArm(ctx, task.QueueName, task.TaskID, reg.interval, true)
	case result.Action == ActionDone:
		metrics().completed.Add(ctx, 1, queue)
		_, err := r.store.Complete(ctx, task.QueueName, task.TaskID)
		return err
	case result.Action == ActionSettled:
		// The handler removed the row inside its own transaction.
		metrics().completed.Add(ctx, 1, queue)
		return nil
	case result.Action == ActionDiscard:
		recordDead(ctx, task.QueueName, DeadReasonDiscarded)
		_, err := r.store.Complete(ctx, task.QueueName, task.TaskID)
		return err
	case result.Action == ActionRetry && budgetExhausted(reg, settings, task):
		recordDead(ctx, task.QueueName, DeadReasonExhausted)
		r.log.Error("task dead: still failing after the attempt budget", slog.String("queue", task.QueueName),
			slog.String("task_id", task.TaskID), slog.Int("attempts", task.Attempt),
			slog.String("error_code", result.ErrorCode), slog.String("error_message", result.ErrorMessage))
		_, err := r.store.Complete(ctx, task.QueueName, task.TaskID)
		return err
	case result.Action == ActionRetry:
		metrics().retried.Add(ctx, 1, queue)
		return r.store.ReArm(ctx, task.QueueName, task.TaskID, result.RetryAfter, false)
	default:
		return fmt.Errorf("taskqueue: unknown result action %d", result.Action)
	}
}

// budgetExhausted reports a one-off task that has used every attempt it was allowed.
func budgetExhausted(reg registration, settings queueSettings, task Task) bool {
	return !reg.recurring() && settings.maxAttempts > 0 && task.Attempt >= settings.maxAttempts
}

func (r *Runner) fixerLoop(ctx context.Context) {
	for ctx.Err() == nil {
		reclaimed, err := r.store.ReclaimStale(ctx, r.cfg.StaleAfter)
		switch {
		case err != nil && ctx.Err() == nil:
			r.log.Error("task fixer sweep failed", slog.Any("error", err))
		case reclaimed > 0:
			metrics().reclaimed.Add(ctx, reclaimed)
			r.log.Warn("task fixer re-armed stale claims",
				slog.Int64("count", reclaimed), slog.Duration("stale_after", r.cfg.StaleAfter))
		}
		// Sampled here rather than from the gauge callback: one grouped count per sweep is
		// predictable load; a callback that queries would put a round trip on the scrape cadence.
		if stats, err := r.store.Stats(ctx); err == nil {
			setQueueStats(stats)
		}
		sleep(ctx, r.cfg.FixerInterval)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

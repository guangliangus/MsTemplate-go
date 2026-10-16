package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"example.com/msvcservice/pkg/postgres"
)

const (
	maxLastErrorLength = 2000
	// backlogSampleInterval is how often the backlog gauges are refreshed, independently of the
	// dispatch loop, so they keep moving exactly when dispatch is stuck.
	backlogSampleInterval = 10 * time.Second
	// cleanupBatchSize bounds one DELETE of the cleanup: a single unbounded statement over a large
	// backlog would hit statement_timeout every time and never make progress.
	cleanupBatchSize = 5000
)

// Readiness is implemented by a Transport that knows whether it can deliver right now. The
// dispatcher asks before claiming a batch, so a broker outage never holds outbox rows locked
// inside an open transaction.
type Readiness interface {
	Ready() bool
}

// OutboxDispatcher polls PENDING rows and publishes them through the Transport. FOR UPDATE SKIP
// LOCKED makes concurrent dispatchers (every replica runs one) safe: each row is claimed by
// exactly one.
//
// Failures are told apart by whose fault they are. When the broker is unavailable (the transport
// reports not ready, or the publish ran out of time) the batch stops, the reason is kept in
// last_error and the attempt budget is left alone: an outage delays events, it never parks them.
// Any other failure is the message's own (the broker refused it) and counts an attempt; past
// MaxAttempts the row is parked as FAILED (watch msvc_messaging_outbox_dispatched
// {outcome="failed"}). last_error is cleared on success, because a stale reason beside a PUBLISHED
// row sends whoever reads the table chasing a failure that is over.
//
// The backlog gauges and the cleanup run on their own loop, so the gauges keep reporting while
// dispatch is stalled — which is when the oldest-pending-age alert has to fire.
//
// Delivery is at-least-once: a crash between the broker's confirm and the row update republishes
// the message on the next cycle, which the consumer-side inbox absorbs.
type OutboxDispatcher struct {
	db        *postgres.DB
	transport Transport
	cfg       OutboxConfig
	log       *slog.Logger
}

// NewOutboxDispatcher builds the dispatcher.
func NewOutboxDispatcher(db *postgres.DB, transport Transport, cfg OutboxConfig, log *slog.Logger) *OutboxDispatcher {
	return &OutboxDispatcher{db: db, transport: transport, cfg: cfg, log: log}
}

// Run polls until ctx is cancelled.
func (d *OutboxDispatcher) Run(ctx context.Context) error {
	cleanup := "disabled: rows are kept forever"
	if d.cfg.CleanupEnabled {
		cleanup = "after " + d.cfg.Retention.String()
	}
	d.log.Info("outbox dispatcher started",
		slog.Duration("poll", d.cfg.PollInterval), slog.Int("batch", d.cfg.BatchSize), slog.String("cleanup", cleanup))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.housekeeping(ctx)
	}()

	for ctx.Err() == nil {
		if !d.transportReady() {
			sleep(ctx, d.cfg.PollInterval)
			continue
		}
		dispatched, stalled, err := d.dispatchBatch(ctx)
		if err != nil && ctx.Err() == nil {
			d.log.Error("outbox dispatch cycle failed; retrying after the poll interval", slog.Any("error", err))
		}
		if err != nil || stalled || dispatched == 0 {
			sleep(ctx, d.cfg.PollInterval)
		}
	}
	wg.Wait()
	d.log.Info("outbox dispatcher stopped")
	return nil
}

func (d *OutboxDispatcher) transportReady() bool {
	r, ok := d.transport.(Readiness)
	return !ok || r.Ready()
}

// dispatchBatch claims and publishes one batch. stalled reports that the broker became
// unavailable part-way: the rest of the batch was left untouched for the next cycle.
func (d *OutboxDispatcher) dispatchBatch(ctx context.Context) (count int, stalled bool, err error) {
	err = d.db.WithTx(ctx, func(ctx context.Context) error {
		q := d.db.Q(ctx)
		rows, err := q.Query(ctx, `
			SELECT id, message_id, event_name, payload, attempts, occurred_at, trace_parent, correlation_id
			FROM   outbox_messages
			WHERE  status = 'PENDING'
			ORDER  BY id
			LIMIT  $1
			FOR UPDATE SKIP LOCKED`, d.cfg.BatchSize)
		if err != nil {
			return fmt.Errorf("claim outbox batch: %w", err)
		}
		batch, err := pgx.CollectRows(rows, pgx.RowToStructByName[outboxRow])
		if err != nil {
			return fmt.Errorf("read outbox batch: %w", err)
		}
		for _, row := range batch {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			unavailable, err := d.dispatchOne(ctx, q, row)
			if err != nil {
				return err
			}
			if unavailable {
				stalled = true
				return nil
			}
			count++
		}
		return nil
	})
	return count, stalled, err
}

// dispatchOne publishes one row and records the outcome; unavailable reports a broker-side failure
// that should end the batch.
func (d *OutboxDispatcher) dispatchOne(ctx context.Context, q postgres.Querier, row outboxRow) (bool, error) {
	env := row.envelope()
	spanCtx := ctx
	if env.TraceParent != "" {
		spanCtx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{HeaderTraceParent: env.TraceParent})
	}
	spanCtx, span := Tracer().Start(spanCtx, env.EventName+" outbox dispatch",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attribute.String("messaging.message.id", env.MessageID.String())))
	defer span.End()

	publishErr := d.transport.Publish(spanCtx, env)
	if publishErr == nil {
		_, err := q.Exec(ctx, `
			UPDATE outbox_messages
			SET    status = 'PUBLISHED', published_at = now(), last_error = NULL
			WHERE  id = $1`, row.ID)
		if err != nil {
			return false, fmt.Errorf("mark outbox row %d published: %w", row.ID, err)
		}
		RecordDispatched(ctx, "published")
		return false, nil
	}
	span.SetStatus(codes.Error, publishErr.Error())
	if ctx.Err() != nil {
		return false, ctx.Err() // shutting down: roll back, the rows stay PENDING
	}
	if d.brokerUnavailable(publishErr) {
		return true, d.recordUnavailable(ctx, spanCtx, q, row, env, publishErr)
	}
	return false, d.recordRejection(ctx, spanCtx, q, row, env, publishErr)
}

// brokerUnavailable tells an outage (not the message's fault) from a refusal of this message.
func (d *OutboxDispatcher) brokerUnavailable(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || !d.transportReady()
}

func (d *OutboxDispatcher) recordUnavailable(ctx, logCtx context.Context, q postgres.Querier, row outboxRow,
	env Envelope, publishErr error) error {
	_, err := q.Exec(ctx, `UPDATE outbox_messages SET last_error = $2 WHERE id = $1`,
		row.ID, truncate(publishErr.Error(), maxLastErrorLength))
	if err != nil {
		return fmt.Errorf("record outbox failure for row %d: %w", row.ID, err)
	}
	RecordDispatched(ctx, OutcomeBrokerUnavailable)
	d.log.WarnContext(logCtx, "broker unavailable; outbox dispatch paused without spending attempts",
		slog.String("message_id", env.MessageID.String()), slog.String("event", env.EventName),
		slog.Any("error", publishErr))
	return nil
}

func (d *OutboxDispatcher) recordRejection(ctx, logCtx context.Context, q postgres.Querier, row outboxRow,
	env Envelope, publishErr error) error {
	attempts := row.Attempts + 1
	parked := attempts >= d.cfg.MaxAttempts
	_, err := q.Exec(ctx, `
		UPDATE outbox_messages
		SET    attempts = $2, last_error = $3, status = CASE WHEN $4 THEN 'FAILED' ELSE status END
		WHERE  id = $1`, row.ID, attempts, truncate(publishErr.Error(), maxLastErrorLength), parked)
	if err != nil {
		return fmt.Errorf("record outbox failure for row %d: %w", row.ID, err)
	}
	if parked {
		RecordDispatched(ctx, "failed")
		d.log.ErrorContext(logCtx, "outbox message parked as FAILED",
			slog.String("message_id", env.MessageID.String()), slog.String("event", env.EventName),
			slog.Int("attempts", attempts), slog.Any("error", publishErr))
		return nil
	}
	RecordDispatched(ctx, "retry")
	d.log.WarnContext(logCtx, "outbox publish failed; will retry",
		slog.String("message_id", env.MessageID.String()), slog.String("event", env.EventName),
		slog.Int("attempts", attempts), slog.Any("error", publishErr))
	return nil
}

// housekeeping samples the backlog gauges and runs the cleanup, outside any dispatch transaction.
func (d *OutboxDispatcher) housekeeping(ctx context.Context) {
	nextCleanup := time.Now()
	for ctx.Err() == nil {
		d.sampleBacklog(ctx)
		if d.cfg.CleanupEnabled && !time.Now().Before(nextCleanup) {
			d.cleanup(ctx)
			nextCleanup = time.Now().Add(d.cfg.CleanupInterval)
		}
		sleep(ctx, backlogSampleInterval)
	}
}

func (d *OutboxDispatcher) sampleBacklog(ctx context.Context) {
	var pending int64
	var oldest float64
	err := d.db.Pool.QueryRow(ctx, `
		SELECT count(*), coalesce(extract(epoch FROM now() - min(occurred_at)), 0)::float8
		FROM   outbox_messages
		WHERE  status = 'PENDING'`).Scan(&pending, &oldest)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("sampling the outbox backlog failed", slog.Any("error", err))
		}
		return
	}
	SetOutboxPending(pending, oldest)
}

func (d *OutboxDispatcher) cleanup(ctx context.Context) {
	cutoff := time.Now().Add(-d.cfg.Retention)
	outbox, err := d.deleteInBatches(ctx, `
		DELETE FROM outbox_messages
		WHERE  id IN (SELECT id FROM outbox_messages WHERE status = 'PUBLISHED' AND published_at < $1 LIMIT $2)`,
		cutoff)
	if err != nil {
		d.log.Warn("outbox cleanup failed", slog.Int64("removed", outbox), slog.Any("error", err))
		return
	}
	inbox, err := d.deleteInBatches(ctx, `
		DELETE FROM inbox_messages
		WHERE  id IN (SELECT id FROM inbox_messages WHERE processed_at < $1 LIMIT $2)`, cutoff)
	if err != nil {
		d.log.Warn("inbox cleanup failed", slog.Int64("removed", inbox), slog.Any("error", err))
		return
	}
	if outbox+inbox > 0 {
		d.log.Info("outbox cleanup removed rows", slog.Int64("outbox", outbox),
			slog.Int64("inbox", inbox), slog.Time("cutoff", cutoff))
	}
}

// deleteInBatches repeats a "DELETE ... LIMIT $2" statement until it removes less than a batch.
func (d *OutboxDispatcher) deleteInBatches(ctx context.Context, sql string, cutoff time.Time) (int64, error) {
	var total int64
	for ctx.Err() == nil {
		tag, err := d.db.Pool.Exec(ctx, sql, cutoff, cleanupBatchSize)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < cleanupBatchSize {
			return total, nil
		}
	}
	return total, ctx.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

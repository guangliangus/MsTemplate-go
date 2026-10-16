package taskqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"example.com/msvcservice/pkg/actor"
	"example.com/msvcservice/pkg/postgres"
)

// Store is the raw-SQL persistence of the queue over the shared pool. Every method runs on the
// transaction carried by the context when there is one, which is what makes Enqueue and Complete
// transactional with the caller's own writes: a business row and its task row commit together, or
// a handler's write and its row's completion do.
type Store struct {
	db *postgres.DB
}

// NewStore builds the store.
func NewStore(db *postgres.DB) *Store {
	return &Store{db: db}
}

// EnqueueRequest describes one task to enqueue.
type EnqueueRequest struct {
	QueueName string
	// TaskID is the business key of the work item: the idempotency handle within its queue.
	TaskID string
	// Payload is JSON-encoded; nil means {}.
	Payload any
	// DeliverOn is the earliest claim time; zero means "now" on the DATABASE clock.
	DeliverOn time.Time
	// Priority: higher claims first, then oldest deliver_on.
	Priority int
}

func (r EnqueueRequest) payloadJSON() ([]byte, error) {
	if r.Payload == nil {
		return []byte("{}"), nil
	}
	if raw, ok := r.Payload.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(r.Payload)
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Enqueue inserts the task unless (queue, task id) already exists, so producers enqueue
// idempotently (retried requests, replayed events). It returns false when the slot was taken.
// Inside WithTx it joins the caller's transaction — the one thing this table has that a broker
// does not: enqueueing on a separate connection reintroduces the dual write the queue exists to
// avoid ("order accepted, and no email" when the process dies between two commits).
func (s *Store) Enqueue(ctx context.Context, req EnqueueRequest) (bool, error) {
	payload, err := req.payloadJSON()
	if err != nil {
		return false, fmt.Errorf("taskqueue: encode payload: %w", err)
	}
	payload = injectTrace(ctx, payload)
	// deliver_on defaults to the DATABASE clock: "run now" must not depend on this pod's clock,
	// because the claim query compares against the database's now().
	tag, err := s.db.Q(ctx).Exec(ctx, `
		INSERT INTO task_queues
			(queue_name, task_id, priority, payload, deliver_on, popped, popped_by,
			 created_at, created_by, updated_at, updated_by)
		VALUES ($1, $2, $3, $4, COALESCE($5, now()), false, '', now(), $6, now(), $6)
		ON CONFLICT (queue_name, task_id) DO NOTHING`,
		req.QueueName, req.TaskID, req.Priority, payload, nullableTime(req.DeliverOn), actor.FromContext(ctx))
	if err != nil {
		return false, fmt.Errorf("taskqueue: enqueue %s/%s: %w", req.QueueName, req.TaskID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// EnqueueBatch inserts a whole batch in ONE statement (unnest), ignoring taken slots, and returns
// how many rows were inserted. One statement rather than a loop: N round trips held open inside
// the producer's transaction, and a partial failure meaning "some of them enqueued".
func (s *Store) EnqueueBatch(ctx context.Context, reqs []EnqueueRequest) (int64, error) {
	if len(reqs) == 0 {
		return 0, nil
	}
	n := len(reqs)
	queues, ids, payloads := make([]string, n), make([]string, n), make([]string, n)
	priorities, deliverOns := make([]int32, n), make([]*time.Time, n)
	for i, req := range reqs {
		payload, err := req.payloadJSON()
		if err != nil {
			return 0, fmt.Errorf("taskqueue: encode payload %d: %w", i, err)
		}
		queues[i], ids[i], payloads[i] = req.QueueName, req.TaskID, string(injectTrace(ctx, payload))
		priorities[i], deliverOns[i] = int32(req.Priority), nullableTime(req.DeliverOn) //nolint:gosec // small
	}
	// Every array is cast so PostgreSQL knows the column types even when a whole array is NULLs.
	tag, err := s.db.Q(ctx).Exec(ctx, `
		INSERT INTO task_queues
			(queue_name, task_id, priority, payload, deliver_on, popped, popped_by,
			 created_at, created_by, updated_at, updated_by)
		SELECT c.queue_name, c.task_id, c.priority, CAST(c.payload AS jsonb), COALESCE(c.deliver_on, now()),
		       false, '', now(), $6, now(), $6
		FROM   unnest(CAST($1 AS text[]), CAST($2 AS text[]), CAST($3 AS integer[]),
		              CAST($4 AS text[]), CAST($5 AS timestamptz[]))
		       AS c(queue_name, task_id, priority, payload, deliver_on)
		ON CONFLICT (queue_name, task_id) DO NOTHING`,
		queues, ids, priorities, payloads, deliverOns, actor.FromContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("taskqueue: enqueue batch: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Complete removes the row; inside WithTx it joins the caller's transaction (the Settled
// pattern). It returns false when the row was already gone.
func (s *Store) Complete(ctx context.Context, queueName, taskID string) (bool, error) {
	tag, err := s.db.Q(ctx).Exec(ctx,
		`DELETE FROM task_queues WHERE queue_name = $1 AND task_id = $2`, queueName, taskID)
	if err != nil {
		return false, fmt.Errorf("taskqueue: complete %s/%s: %w", queueName, taskID, err)
	}
	return tag.RowsAffected() > 0, nil
}

type taskRow struct {
	ID        int             `db:"id"`
	QueueName string          `db:"queue_name"`
	TaskID    string          `db:"task_id"`
	Priority  int             `db:"priority"`
	Attempts  int             `db:"attempts"`
	Payload   json.RawMessage `db:"payload"`
	DeliverOn time.Time       `db:"deliver_on"`
}

// Pop claims up to limit due tasks atomically: a CTE selects candidates with FOR UPDATE SKIP
// LOCKED (replicas never contend), the UPDATE marks them popped and bumps attempts. The filter and
// ORDER BY match ix_task_queues_lookup column for column.
func (s *Store) Pop(ctx context.Context, queueName string, limit int, poppedBy string) ([]Task, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.Q(ctx).Query(ctx, `
		WITH candidate AS (
			SELECT id
			FROM   task_queues
			WHERE  queue_name = $1
			  AND  popped = false
			  AND  deliver_on <= now()
			ORDER  BY priority DESC, deliver_on ASC, id ASC
			LIMIT  $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE task_queues q
		SET    popped = true, popped_at = now(), popped_by = $3,
		       attempts = q.attempts + 1, updated_at = now(), updated_by = $3
		FROM   candidate
		WHERE  q.id = candidate.id
		RETURNING q.id, q.queue_name, q.task_id, q.priority, q.attempts, q.payload, q.deliver_on`,
		queueName, limit, poppedBy)
	if err != nil {
		return nil, fmt.Errorf("taskqueue: pop %s: %w", queueName, err)
	}
	claimed, err := pgx.CollectRows(rows, pgx.RowToStructByName[taskRow])
	if err != nil {
		return nil, fmt.Errorf("taskqueue: read popped rows: %w", err)
	}
	tasks := make([]Task, len(claimed))
	for i, r := range claimed {
		tasks[i] = Task{QueueName: r.QueueName, TaskID: r.TaskID, Payload: r.Payload,
			Priority: r.Priority, Attempt: r.Attempts, DeliverOn: r.DeliverOn}
	}
	return tasks, nil
}

// ReArm resets a claimed row to pending, due delay from now on the database clock. Retries keep
// the attempt count (it feeds the budget and backoff ladders); a recurring task's next cycle
// resets it.
func (s *Store) ReArm(ctx context.Context, queueName, taskID string, delay time.Duration, resetAttempts bool) error {
	_, err := s.db.Q(ctx).Exec(ctx, `
		UPDATE task_queues
		SET    deliver_on = now() + ($3 * interval '1 second'),
		       popped = false, popped_at = NULL, popped_by = '',
		       attempts = CASE WHEN $4 THEN 0 ELSE attempts END,
		       updated_at = now()
		WHERE  queue_name = $1 AND task_id = $2`,
		queueName, taskID, delay.Seconds(), resetAttempts)
	if err != nil {
		return fmt.Errorf("taskqueue: re-arm %s/%s: %w", queueName, taskID, err)
	}
	return nil
}

// ReArmAt resets a claimed row to pending at an absolute time (a cron occurrence).
func (s *Store) ReArmAt(ctx context.Context, queueName, taskID string, at time.Time, resetAttempts bool) error {
	_, err := s.db.Q(ctx).Exec(ctx, `
		UPDATE task_queues
		SET    deliver_on = $3,
		       popped = false, popped_at = NULL, popped_by = '',
		       attempts = CASE WHEN $4 THEN 0 ELSE attempts END,
		       updated_at = now()
		WHERE  queue_name = $1 AND task_id = $2`,
		queueName, taskID, at, resetAttempts)
	if err != nil {
		return fmt.Errorf("taskqueue: re-arm %s/%s at %s: %w", queueName, taskID, at, err)
	}
	return nil
}

// RescheduleIfPending moves a pending, not-yet-due row to a new time: how a cron row follows an
// expression that changed between deployments. A row that is already due is left alone (it is a
// missed firing for the misfire policy).
func (s *Store) RescheduleIfPending(ctx context.Context, queueName, taskID string, at time.Time) error {
	_, err := s.db.Q(ctx).Exec(ctx, `
		UPDATE task_queues
		SET    deliver_on = $3, updated_at = now()
		WHERE  queue_name = $1 AND task_id = $2 AND popped = false AND deliver_on > now() AND deliver_on <> $3`,
		queueName, taskID, at)
	if err != nil {
		return fmt.Errorf("taskqueue: reschedule %s/%s: %w", queueName, taskID, err)
	}
	return nil
}

// ReclaimStale re-arms rows claimed longer than staleAfter ago: their worker died mid-flight. A
// non-positive window is a no-op, because it would release every just-claimed row.
func (s *Store) ReclaimStale(ctx context.Context, staleAfter time.Duration) (int64, error) {
	if staleAfter <= 0 {
		return 0, nil
	}
	tag, err := s.db.Q(ctx).Exec(ctx, `
		UPDATE task_queues
		SET    popped = false, popped_at = NULL, popped_by = '', updated_at = now()
		WHERE  popped = true AND popped_at < now() - ($1 * interval '1 second')`, staleAfter.Seconds())
	if err != nil {
		return 0, fmt.Errorf("taskqueue: reclaim stale: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountPending reports the claimable backlog per queue (unclaimed and already due). The predicate
// is spelled popped = false exactly as the claim query's and the partial index's.
func (s *Store) CountPending(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.Q(ctx).Query(ctx, `
		SELECT queue_name, count(*)
		FROM   task_queues
		WHERE  popped = false AND deliver_on <= now()
		GROUP  BY 1`)
	if err != nil {
		return nil, fmt.Errorf("taskqueue: count pending: %w", err)
	}
	defer rows.Close()
	depths := map[string]int64{}
	for rows.Next() {
		var queue string
		var depth int64
		if err := rows.Scan(&queue, &depth); err != nil {
			return nil, fmt.Errorf("taskqueue: scan depth: %w", err)
		}
		depths[queue] = depth
	}
	return depths, rows.Err()
}

// QueueStats is one queue's claimable backlog as the fixer samples it.
type QueueStats struct {
	// Depth is how many tasks are unclaimed and already due.
	Depth int64
	// OldestAge is how long the oldest of them has been due: the lateness the queue is at.
	OldestAge time.Duration
}

// Stats reports the claimable backlog per queue with the age of its oldest task, in one grouped
// query on the database clock. The predicate is spelled popped = false exactly as the claim
// query's and the partial index's.
func (s *Store) Stats(ctx context.Context) (map[string]QueueStats, error) {
	rows, err := s.db.Q(ctx).Query(ctx, `
		SELECT queue_name, count(*), coalesce(extract(epoch FROM now() - min(deliver_on)), 0)::float8
		FROM   task_queues
		WHERE  popped = false AND deliver_on <= now()
		GROUP  BY 1`)
	if err != nil {
		return nil, fmt.Errorf("taskqueue: queue stats: %w", err)
	}
	defer rows.Close()
	stats := map[string]QueueStats{}
	for rows.Next() {
		var queue string
		var depth int64
		var oldest float64
		if err := rows.Scan(&queue, &depth, &oldest); err != nil {
			return nil, fmt.Errorf("taskqueue: scan queue stats: %w", err)
		}
		stats[queue] = QueueStats{Depth: depth, OldestAge: time.Duration(oldest * float64(time.Second))}
	}
	return stats, rows.Err()
}

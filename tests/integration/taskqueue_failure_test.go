//go:build integration

package integration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"example.com/msvcservice/pkg/actor"
	"example.com/msvcservice/pkg/taskqueue"
)

// A handler error or panic spends an attempt and re-arms the task with backoff; a one-off task
// that keeps failing is dead after max_attempts instead of coming back through the fixer forever.
func TestFailingTasksBackOffAndDieAfterTheirBudget(t *testing.T) {
	db := openDB(t)
	store := taskqueue.NewStore(db)
	cfg := taskqueue.Config{MaxWorkers: 2, MaxAttempts: 3, RetryBaseDelay: time.Second, RetryMaxDelay: time.Second,
		ShortPollInterval: 50 * time.Millisecond, LongPollInterval: 100 * time.Millisecond, TaskTimeout: 5 * time.Second,
		StaleAfter: time.Hour, DrainTimeout: 2 * time.Second, FixerInterval: time.Hour}
	runner := taskqueue.NewRunner(store, cfg, quiet)
	var runs atomic.Int32
	err := runner.Handle("flaky", taskqueue.HandlerFunc(func(context.Context, taskqueue.Task) (taskqueue.Result, error) {
		if runs.Add(1) == 2 {
			panic("the second run panics")
		}
		return taskqueue.Result{}, errors.New("still failing")
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := store.Enqueue(actor.WithName(ctx, "tester"), taskqueue.EnqueueRequest{QueueName: "flaky", TaskID: "t1"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	// The fixer never runs (stale_after is an hour): only the failure path can bring the task back.
	waitFor(t, func() bool {
		var n int
		_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM task_queues WHERE queue_name = 'flaky'`).Scan(&n)
		return n == 0
	})
	if got := runs.Load(); got != 3 {
		t.Fatalf("ran %d times, want max_attempts (3)", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

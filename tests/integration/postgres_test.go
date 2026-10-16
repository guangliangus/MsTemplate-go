//go:build integration

// Package integration runs the kit and the reference module against real PostgreSQL, Redis and
// RabbitMQ containers, on the schema from db/*.sql. Run with `make test-integration`.
package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/order"
	svcpg "example.com/msvcservice/internal/postgres"
	"example.com/msvcservice/pkg/actor"
	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/messaging"
	"example.com/msvcservice/pkg/paging"
	"example.com/msvcservice/pkg/postgres"
	"example.com/msvcservice/pkg/taskqueue"
	"example.com/msvcservice/pkg/testutil"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func openDB(t *testing.T) *postgres.DB {
	t.Helper()
	dsn := testutil.Postgres(t)
	db, err := postgres.Open(context.Background(), postgres.Config{DSN: dsn, MaxConns: 8, MinConns: 1,
		ConnectTimeout: 5 * time.Second, StatementTimeout: 10 * time.Second}, "it", quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestOrderRepositoryRoundTrip(t *testing.T) {
	db := openDB(t)
	repo := svcpg.NewOrderRepository(db)
	ctx := actor.WithName(context.Background(), "tester")

	o, err := order.New("cust-1", []order.Item{
		{SKU: "A", Quantity: 2, UnitPrice: decimal.RequireFromString("19.99")},
		{SKU: "B", Quantity: 1, UnitPrice: decimal.RequireFromString("0.50")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Insert(ctx, o); err != nil {
		t.Fatal(err)
	}
	if o.ID == 0 || o.Items[1].ID == 0 || o.Version == 0 || o.CreatedBy != "tester" {
		t.Fatalf("insert did not fill the row back: %+v", o)
	}

	got, err := repo.Get(ctx, o.ID, "cust-1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.TotalAmount.Equal(decimal.RequireFromString("40.48")) || len(got.Items) != 2 || got.Items[0].SKU != "A" {
		t.Fatalf("%+v", got)
	}
	if _, err := repo.Get(ctx, o.ID, "someone-else"); apperror.StatusOf(err) != 404 {
		t.Fatalf("scoped read: %v", err)
	}

	// Optimistic concurrency: a stale version loses.
	stale := *got
	if err := got.Confirm(); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatus(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := stale.Cancel(); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatus(ctx, &stale); apperror.StatusOf(err) != 409 {
		t.Fatalf("stale xmin must conflict: %v", err)
	}

	items, total, err := repo.List(ctx, "cust-1", paging.Request{Page: 1, PageSize: 10})
	if err != nil || total != 1 || len(items) != 1 || items[0].Status != order.StatusConfirmed {
		t.Fatalf("list: %v %d %+v", err, total, items)
	}

	// The reaper only touches old PENDING rows, and names the ones it cancelled.
	ids, err := repo.CancelPendingOlderThan(ctx, time.Now().Add(time.Minute), 100)
	if err != nil || len(ids) != 0 {
		t.Fatalf("confirmed orders are not reaped: %v %v", ids, err)
	}
	pending, _ := order.New("cust-1", []order.Item{{SKU: "C", Quantity: 1, UnitPrice: decimal.NewFromInt(3)}})
	if err := repo.Insert(ctx, pending); err != nil {
		t.Fatal(err)
	}
	ids, err = repo.CancelPendingOlderThan(ctx, time.Now().Add(time.Minute), 100)
	if err != nil || len(ids) != 1 || ids[0] != pending.ID {
		t.Fatalf("reaped ids: %v %v", ids, err)
	}
}

// Keyset paging over every customer: newest first, each page continuing below the last id.
func TestOrderRepositoryListsAllByKeyset(t *testing.T) {
	db := openDB(t)
	repo := svcpg.NewOrderRepository(db)
	ctx := actor.WithName(context.Background(), "tester")
	for _, customer := range []string{"a", "b", "a", "c", "b"} {
		o, _ := order.New(customer, []order.Item{{SKU: "X", Quantity: 1, UnitPrice: decimal.NewFromInt(1)}})
		if err := repo.Insert(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	var seen []int
	page := paging.CursorRequest{Limit: 2}
	for range 5 {
		rows, err := repo.ListAll(ctx, page)
		if err != nil {
			t.Fatal(err)
		}
		res := paging.NewCursorResult(rows, page, func(o order.Order) int64 { return int64(o.ID) })
		for _, o := range res.Items {
			seen = append(seen, o.ID)
			if len(o.Items) != 1 {
				t.Fatalf("items loaded: %+v", o)
			}
		}
		if !res.HasMore {
			break
		}
		page, err = paging.ParseCursor(map[string][]string{"cursor": {res.NextCursor}, "limit": {"2"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 5 || seen[0] != 5 || seen[4] != 1 {
		t.Fatalf("every order once, newest first: %v", seen)
	}
	mine, total, err := repo.List(ctx, "a", paging.Request{Page: 1, PageSize: 10})
	if err != nil || total != 2 || len(mine) != 2 {
		t.Fatalf("one customer's page: %d %v", total, err)
	}
}

// AfterCommit work runs once the outermost transaction commits, never on a rollback, and at once
// outside a transaction.
func TestAfterCommitRunsOnlyAfterTheCommit(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	var ran []string
	err := db.WithTx(ctx, func(ctx context.Context) error {
		db.AfterCommit(ctx, func(context.Context) { ran = append(ran, "outer") })
		return db.WithTx(ctx, func(ctx context.Context) error { // joins the outer transaction
			db.AfterCommit(ctx, func(context.Context) { ran = append(ran, "inner") })
			if len(ran) != 0 {
				t.Error("nothing runs before the commit")
			}
			return nil
		})
	})
	if err != nil || len(ran) != 2 || ran[0] != "outer" || ran[1] != "inner" {
		t.Fatalf("after the commit, in registration order: %v %v", ran, err)
	}
	ran = nil
	_ = db.WithTx(ctx, func(ctx context.Context) error {
		db.AfterCommit(ctx, func(context.Context) { ran = append(ran, "rolled back") })
		return errors.New("refused")
	})
	if len(ran) != 0 {
		t.Fatalf("a rollback runs nothing: %v", ran)
	}
	db.AfterCommit(ctx, func(context.Context) { ran = append(ran, "now") })
	if len(ran) != 1 {
		t.Fatal("outside a transaction it runs at once")
	}
}

func TestTransactionsCarryOnTheContext(t *testing.T) {
	db := openDB(t)
	repo := svcpg.NewOrderRepository(db)
	publisher := messaging.NewOutboxPublisher(db)
	ctx := actor.WithName(context.Background(), "tester")

	// Everything in one WithTx commits together ...
	err := db.WithTx(ctx, func(ctx context.Context) error {
		o, _ := order.New("c", []order.Item{{SKU: "A", Quantity: 1, UnitPrice: decimal.NewFromInt(1)}})
		if err := repo.Insert(ctx, o); err != nil {
			return err
		}
		return publisher.Publish(ctx, testEvent{OrderID: o.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_messages WHERE status = 'PENDING'`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("outbox row committed with the order: %d %v", pending, err)
	}

	// ... and rolls back together.
	rollback := apperror.DomainRule("X", "refused")
	err = db.WithTx(ctx, func(ctx context.Context) error {
		o, _ := order.New("c", []order.Item{{SKU: "A", Quantity: 1, UnitPrice: decimal.NewFromInt(1)}})
		if err := repo.Insert(ctx, o); err != nil {
			return err
		}
		if err := publisher.Publish(ctx, testEvent{OrderID: o.ID}); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("expected the rollback error, got %v", err)
	}
	var orders int
	_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&orders)
	_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_messages`).Scan(&pending)
	if orders != 1 || pending != 1 {
		t.Fatalf("rollback must undo both writes: orders=%d outbox=%d", orders, pending)
	}
}

type testEvent struct {
	OrderID int `json:"orderId"`
}

func (testEvent) EventName() string { return "test.event.v1" }

func TestInboxProcessesOnce(t *testing.T) {
	db := openDB(t)
	inbox := messaging.NewInbox(db)
	ctx := context.Background()
	id := uuid.New()
	runs := 0
	for i := range 2 {
		processed, err := inbox.ProcessOnce(ctx, id, "consumer-a", func(context.Context) error { runs++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if first := i == 0; processed != first {
			t.Fatalf("delivery %d: processed=%v runs=%d", i+1, processed, runs)
		}
	}
	if runs != 1 {
		t.Fatalf("handler ran %d times", runs)
	}
	// A different consumer of the same message is a different ledger entry.
	if processed, _ := inbox.ProcessOnce(ctx, id, "consumer-b", func(context.Context) error { return nil }); !processed {
		t.Fatal("per-consumer deduplication")
	}
	// A failing handler rolls the ledger row back, so a redelivery runs again.
	boom := apperror.Internal(nil)
	other := uuid.New()
	if _, err := inbox.ProcessOnce(ctx, other, "consumer-a", func(context.Context) error { return boom }); err == nil {
		t.Fatal("handler error must surface")
	}
	if processed, _ := inbox.ProcessOnce(ctx, other, "consumer-a", func(context.Context) error { return nil }); !processed {
		t.Fatal("after a failure the message is processable again")
	}
}

func TestTaskQueueStore(t *testing.T) {
	db := openDB(t)
	store := taskqueue.NewStore(db)
	ctx := actor.WithName(context.Background(), "tester")

	inserted, err := store.Enqueue(ctx, taskqueue.EnqueueRequest{QueueName: "q", TaskID: "t1", Payload: map[string]int{"n": 1}, Priority: 1})
	if err != nil || !inserted {
		t.Fatalf("enqueue: %v %v", inserted, err)
	}
	if again, _ := store.Enqueue(ctx, taskqueue.EnqueueRequest{QueueName: "q", TaskID: "t1"}); again {
		t.Fatal("enqueue is idempotent on (queue, task id)")
	}
	n, err := store.EnqueueBatch(ctx, []taskqueue.EnqueueRequest{
		{QueueName: "q", TaskID: "t1"},
		{QueueName: "q", TaskID: "t2", DeliverOn: time.Now().Add(time.Hour)},
		{QueueName: "q", TaskID: "t3"},
	})
	if err != nil || n != 2 {
		t.Fatalf("batch inserted %d: %v", n, err)
	}

	popped, err := store.Pop(ctx, "q", 10, "runner-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(popped) != 2 || popped[0].TaskID != "t1" || popped[0].Attempt != 1 || string(popped[0].Payload) != `{"n": 1}` {
		t.Fatalf("pop order/attempts/payload: %+v", popped)
	}
	if again, _ := store.Pop(ctx, "q", 10, "runner-2"); len(again) != 0 {
		t.Fatal("claimed rows are invisible to other runners")
	}

	if err := store.ReArm(ctx, "q", "t1", 0, false); err != nil {
		t.Fatal(err)
	}
	retried, _ := store.Pop(ctx, "q", 10, "runner-1")
	if len(retried) != 1 || retried[0].Attempt != 2 {
		t.Fatalf("a retry keeps counting attempts: %+v", retried)
	}

	depths, err := store.CountPending(ctx)
	if err != nil || depths["q"] != 0 {
		t.Fatalf("depth counts only claimable rows: %v %v", depths, err)
	}
	if reclaimed, _ := store.ReclaimStale(ctx, time.Nanosecond); reclaimed != 2 {
		t.Fatalf("stale claims re-armed: %d", reclaimed)
	}
	if done, _ := store.Complete(ctx, "q", "t3"); !done {
		t.Fatal("complete deletes the row")
	}
	if gone, _ := store.Complete(ctx, "q", "t3"); gone {
		t.Fatal("completing twice is a no-op")
	}
}

func TestTaskQueueRunnerExecutesRecurringTask(t *testing.T) {
	db := openDB(t)
	store := taskqueue.NewStore(db)
	cfg := taskqueue.Config{MaxWorkers: 2, ShortPollInterval: 50 * time.Millisecond, LongPollInterval: 100 * time.Millisecond,
		TaskTimeout: 5 * time.Second, StaleAfter: 10 * time.Second, DrainTimeout: 2 * time.Second, FixerInterval: time.Second}
	runner := taskqueue.NewRunner(store, cfg, quiet)
	fired := make(chan taskqueue.Task, 4)
	if err := runner.Recurring("tick", 200*time.Millisecond, taskqueue.HandlerFunc(func(_ context.Context, task taskqueue.Task) (taskqueue.Result, error) {
		fired <- task
		return taskqueue.Done(), nil
	})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	var first, second taskqueue.Task
	select {
	case first = <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("the recurring task never fired")
	}
	select {
	case second = <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("the recurring task did not re-arm")
	}
	if first.Attempt != 1 || second.Attempt != 1 {
		t.Fatalf("a recurring run starts a fresh attempt cycle: %d %d", first.Attempt, second.Attempt)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

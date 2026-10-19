//go:build integration

// Package integration runs the service's own code against a real PostgreSQL container, on the
// schema from db/*.sql. The kit's behaviour (outbox, inbox, task queue, broker, Redis) is tested in
// go-msvc-kit; what is left here is what only this service has: its repository, and its rows
// committing in one transaction with the kit's outbox row. Run with `make test-integration`.
package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/order"
	svcpg "example.com/msvcservice/internal/postgres"
	"github.com/guangliangus/go-msvc-kit/actor"
	"github.com/guangliangus/go-msvc-kit/apperror"
	"github.com/guangliangus/go-msvc-kit/messaging"
	"github.com/guangliangus/go-msvc-kit/paging"
	"github.com/guangliangus/go-msvc-kit/postgres"
	"github.com/guangliangus/go-msvc-kit/testutil"
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

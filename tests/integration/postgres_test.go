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
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/internal/order/ordertest"
	svcpg "example.com/msvcservice/internal/postgres"
	"github.com/guangliangus/go-msvc-kit/actor"
	"github.com/guangliangus/go-msvc-kit/apperror"
	"github.com/guangliangus/go-msvc-kit/cache"
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

func newOrderRepository(t *testing.T, db *postgres.DB) *svcpg.OrderRepository {
	t.Helper()
	g, err := svcpg.OpenGorm(db)
	if err != nil {
		t.Fatal(err)
	}
	return svcpg.NewOrderRepository(g)
}

func TestOrderRepositoryRoundTrip(t *testing.T) {
	db := openDB(t)
	repo := newOrderRepository(t, db)
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
		t.Fatalf("stale version must conflict: %v", err)
	}
	if got.Version != stale.Version+1 {
		t.Fatalf("an update bumps the version: %d after %d", got.Version, stale.Version)
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
	// The reaper's UPDATE bumps the version too: a writer still holding the pre-reap version (a
	// confirmation racing the reaper) must lose rather than resurrect a cancelled order.
	if err := pending.Confirm(); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatus(ctx, pending); apperror.StatusOf(err) != 409 {
		t.Fatalf("a write carrying the version from before the reap must conflict: %v", err)
	}
}

// Keyset paging over every customer: newest first, each page continuing below the last id.
func TestOrderRepositoryListsAllByKeyset(t *testing.T) {
	db := openDB(t)
	repo := newOrderRepository(t, db)
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
	repo := newOrderRepository(t, db)
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

// GORM's optimisticlock plugin adds "WHERE version = ?" only for a non-zero version; an order
// whose version was never loaded would be written with no concurrency check at all. The
// repository refuses it instead, and the row is left as it was.
func TestUpdatingWithoutALoadedVersionIsRefused(t *testing.T) {
	db := openDB(t)
	repo := newOrderRepository(t, db)
	ctx := actor.WithName(context.Background(), "tester")
	o, _ := order.New("cust-1", []order.Item{{SKU: "A", Quantity: 1, UnitPrice: decimal.NewFromInt(1)}})
	if err := repo.Insert(ctx, o); err != nil {
		t.Fatal(err)
	}
	if o.Version != 1 {
		t.Fatalf("a new order starts at version 1: %d", o.Version)
	}
	blind := *o
	blind.Version = 0
	if err := blind.Confirm(); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatus(ctx, &blind); err == nil || apperror.StatusOf(err) == 409 {
		t.Fatalf("an update without a loaded version must be refused outright: %v", err)
	}
	got, err := repo.Get(ctx, o.ID, "")
	if err != nil || got.Status != order.StatusPending || got.Version != 1 {
		t.Fatalf("the row is untouched: %+v %v", got, err)
	}
}

// GORM checks nothing against the DDL at build time. With QueryFields every query names each
// mapped column, so one query per model against the schema db/*.sql builds fails on any column a
// model names but its table lacks — a renamed column, a typo, a field added before its script.
func TestEveryGormModelMatchesTheSchema(t *testing.T) {
	db := openDB(t)
	g, err := svcpg.OpenGorm(db)
	if err != nil {
		t.Fatal(err)
	}
	for table, dest := range svcpg.Models() {
		if err := g.On(context.Background()).Limit(1).Find(dest).Error; err != nil {
			t.Errorf("the %s model does not match db/*.sql: %v", table, err)
		}
	}
}

// The request id is unique per customer, and only when present: the index refuses a repeat as
// order.ErrDuplicateRequest, lets another customer use the same id, and leaves orders without one
// alone.
func TestARepeatedRequestIDIsRefusedByTheIndex(t *testing.T) {
	db := openDB(t)
	repo := newOrderRepository(t, db)
	ctx := actor.WithName(context.Background(), "tester")
	insert := func(customer, requestID string) (*order.Order, error) {
		o, _ := order.New(customer, []order.Item{{SKU: "A", Quantity: 1, UnitPrice: decimal.NewFromInt(1)}})
		o.RequestID = requestID
		return o, repo.Insert(ctx, o)
	}

	first, err := insert("c1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insert("c1", "key-1"); !errors.Is(err, order.ErrDuplicateRequest) {
		t.Fatalf("a repeated request id: %v", err)
	}
	for _, tc := range []struct{ customer, requestID string }{{"c2", "key-1"}, {"c1", ""}, {"c1", ""}} {
		if _, err := insert(tc.customer, tc.requestID); err != nil {
			t.Fatalf("%+v must insert: %v", tc, err)
		}
	}
	found, ok, err := repo.FindByRequestID(ctx, "c1", "key-1")
	if err != nil || !ok || found.ID != first.ID || found.RequestID != "key-1" || len(found.Items) != 1 {
		t.Fatalf("lookup: %+v %v %v", found, ok, err)
	}
	if _, ok, err := repo.FindByRequestID(ctx, "c3", "key-1"); err != nil || ok {
		t.Fatalf("another customer's id finds nothing: %v %v", ok, err)
	}
}

// Concurrent repeats of one create — a client retrying before the first answer came back — make
// one order and one event. Repeats that got past the lookup wait for the first transaction at the
// unique index, are refused once it commits, and answer with its order.
func TestConcurrentRepeatsOfACreateMakeOneOrder(t *testing.T) {
	db := openDB(t)
	c := cache.NewRedis(cache.Config{DefaultTTL: time.Minute}, nil, quiet) // nothing cached: not what this tests
	svc := order.NewService(newOrderRepository(t, db), db, ordertest.Inventory{UnitPrice: decimal.NewFromInt(2)},
		messaging.NewOutboxPublisher(db), c, quiet)
	ctx := actor.WithName(context.Background(), "tester")

	const repeats = 6
	ids, created, errs := make([]int, repeats), make([]bool, repeats), make([]error, repeats)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range repeats {
		wg.Go(func() {
			<-start
			o, isNew, err := svc.Create(ctx, "c1", "key-1", []order.Line{{SKU: "A", Quantity: 1}})
			if o != nil {
				ids[i] = o.ID
			}
			created[i], errs[i] = isNew, err
		})
	}
	close(start)
	wg.Wait()

	news := 0
	for i := range repeats {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("repeat %d: id %d (first %d), err %v", i, ids[i], ids[0], errs[i])
		}
		if created[i] {
			news++
		}
	}
	var orders, events int
	_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE request_id = 'key-1'`).Scan(&orders)
	_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_messages`).Scan(&events)
	if news != 1 || orders != 1 || events != 1 {
		t.Fatalf("one request, one order, one event: created=%d orders=%d events=%d", news, orders, events)
	}
}

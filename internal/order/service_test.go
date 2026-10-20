package order_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/internal/order/ordertest"
	"github.com/guangliangus/go-msvc-kit/apperror"
)

// twoEach is these tests' inventory: every SKU sells at 2 per unit, so totals are easy to read.
var twoEach = ordertest.Inventory{UnitPrice: decimal.NewFromInt(2)}

func lines(skus ...string) []order.Line {
	out := make([]order.Line, 0, len(skus))
	for _, s := range skus {
		out = append(out, order.Line{SKU: s, Quantity: 1})
	}
	return out
}

func TestCreateCommitsOrderAndEventTogether(t *testing.T) {
	svc, repo, pub := ordertest.NewService(t, twoEach)
	o, _, err := svc.Create(context.Background(), "c1", "", lines("A", "B"))
	if err != nil {
		t.Fatal(err)
	}
	// The price is the inventory service's (2 per unit), never the client's.
	if o.ID != 1 || o.Status != order.StatusPending || !o.TotalAmount.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("%+v", o)
	}
	if len(repo.InTx()) != 1 || !pub.InTx() {
		t.Fatal("the insert and the publish must run inside the transaction")
	}
	ev, ok := pub.Events()[0].(order.CreatedEvent)
	if !ok || ev.OrderID != 1 || ev.EventName() != order.EventCreated {
		t.Fatalf("event %+v", pub.Events()[0])
	}
}

func TestCreateRefusesUnavailableAndPropagatesUpstreamErrors(t *testing.T) {
	svc, _, pub := ordertest.NewService(t, ordertest.Inventory{UnitPrice: twoEach.UnitPrice, Unavailable: map[string]bool{"B": true}})
	_, _, err := svc.Create(context.Background(), "c1", "", lines("A", "B"))
	if e, _ := apperror.As(err); e == nil || e.Code != order.CodeItemUnavailable || e.Status != 422 {
		t.Fatalf("got %v", err)
	}
	if len(pub.Events()) != 0 {
		t.Fatal("nothing is published when the order is refused")
	}

	down := apperror.Upstream("inventory", "down", errors.New("x"))
	svc, _, _ = ordertest.NewService(t, ordertest.Inventory{Err: down})
	if _, _, err := svc.Create(context.Background(), "c1", "", lines("A")); !errors.Is(err, down) {
		t.Fatalf("upstream errors pass through: %v", err)
	}
}

func TestGetScopesByCustomerWithoutLeaking(t *testing.T) {
	svc, _, _ := ordertest.NewService(t, twoEach)
	o, _, _ := svc.Create(context.Background(), "c1", "", lines("A"))
	if _, err := svc.Get(context.Background(), o.ID, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(context.Background(), o.ID, "someone-else"); apperror.StatusOf(err) != 404 {
		t.Fatalf("another customer's order must look missing: %v", err)
	}
	if _, err := svc.Get(context.Background(), o.ID, ""); err != nil {
		t.Fatal("the back office reads any order")
	}
}

func TestConfirmIsIdempotentAndCancelFollowsTheRules(t *testing.T) {
	svc, repo, _ := ordertest.NewService(t, twoEach)
	o, _, _ := svc.Create(context.Background(), "c1", "", lines("A"))
	if err := svc.Confirm(context.Background(), o.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Confirm(context.Background(), o.ID); err != nil {
		t.Fatal("a second confirmation is a no-op, not an error")
	}
	if stored, _ := repo.Stored(o.ID); stored.Status != order.StatusConfirmed {
		t.Fatal("status persisted")
	}
	if err := svc.Cancel(context.Background(), o.ID, "c1"); apperror.StatusOf(err) != 409 {
		t.Fatalf("confirmed orders cannot be cancelled: %v", err)
	}
	// The cache was invalidated by Confirm, so the read sees CONFIRMED.
	got, _ := svc.Get(context.Background(), o.ID, "c1")
	if got.Status != order.StatusConfirmed {
		t.Fatal("cache invalidated after a status change")
	}
}

// The line rules run before anything is priced: no downstream call for an order that could never
// be accepted, whether a line is wrong or there are too many of them.
func TestCreateChecksTheRequestBeforeCallingInventory(t *testing.T) {
	svc, _, _ := ordertest.NewService(t, ordertest.Inventory{Err: errors.New("inventory must not be called")})
	_, _, err := svc.Create(context.Background(), "c1", "", []order.Line{{SKU: "A", Quantity: 0}})
	if e, _ := apperror.As(err); e == nil || e.Code != order.CodeItemInvalid {
		t.Fatalf("got %v", err)
	}
	tooMany := make([]order.Line, order.MaxItems+1)
	for i := range tooMany {
		tooMany[i] = order.Line{SKU: "A", Quantity: 1}
	}
	_, _, err = svc.Create(context.Background(), "c1", "", tooMany)
	if e, _ := apperror.As(err); e == nil || e.Code != order.CodeItemInvalid || e.Status != 422 {
		t.Fatalf("got %v", err)
	}
	_, _, err = svc.Create(context.Background(), "c1", "", nil)
	if e, _ := apperror.As(err); e == nil || e.Code != order.CodeItemsRequired || e.Status != 422 {
		t.Fatalf("got %v", err)
	}
}

// The consumer confirms inside the inbox transaction: a read that lands before the commit must not
// find the entry already gone (it would cache the uncommitted-away PENDING row for a whole TTL).
func TestStatusChangesInvalidateTheCacheOnlyAfterCommit(t *testing.T) {
	svc, _, _ := ordertest.NewService(t, twoEach)
	ctx := context.Background()
	o, _, _ := svc.Create(ctx, "c1", "", lines("A"))
	if got, _ := svc.Get(ctx, o.ID, "c1"); got.Status != order.StatusPending { // now cached
		t.Fatal(got.Status)
	}
	err := ordertest.Transactor{}.WithTx(ctx, func(ctx context.Context) error {
		if err := svc.Confirm(ctx, o.ID); err != nil {
			return err
		}
		if got, _ := svc.Get(ctx, o.ID, "c1"); got.Status != order.StatusPending {
			t.Error("the entry was dropped before the commit")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, o.ID, "c1"); got.Status != order.StatusConfirmed {
		t.Fatalf("after the commit the read sees the change: %s", got.Status)
	}
}

func TestReapingInvalidatesEveryCancelledOrder(t *testing.T) {
	svc, _, _ := ordertest.NewService(t, twoEach)
	ctx := context.Background()
	o, _, _ := svc.Create(ctx, "c1", "", lines("A"))
	_, _ = svc.Get(ctx, o.ID, "c1") // cached as PENDING
	n, err := svc.ReapPending(ctx, time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("reaped %d %v", n, err)
	}
	if got, _ := svc.Get(ctx, o.ID, "c1"); got.Status != order.StatusCancelled {
		t.Fatalf("a reaped order must not be served PENDING from the cache: %s", got.Status)
	}
}

// One run drains a backlog larger than a batch: the reaper keeps going until a short batch
// comes back, rather than leaving the rest for the next minute.
func TestReapPendingDrainsABacklogAboveTheBatchSize(t *testing.T) {
	svc, repo, _ := ordertest.NewService(t, twoEach)
	ctx := context.Background()
	backlog := 2*order.ReapBatchSize + 1
	for range backlog {
		if _, _, err := svc.Create(ctx, "c1", "", lines("A")); err != nil {
			t.Fatal(err)
		}
	}
	n, err := svc.ReapPending(ctx, time.Minute)
	if err != nil || n != int64(backlog) {
		t.Fatalf("reaped %d of %d: %v", n, backlog, err)
	}
	if got, _ := repo.Stored(backlog); got.Status != order.StatusCancelled {
		t.Fatalf("the last order of the backlog was not reached: %s", got.Status)
	}
	if n, _ := svc.ReapPending(ctx, time.Minute); n != 0 {
		t.Fatalf("nothing is left: %d", n)
	}
}

// endlessBacklog is a repository whose backlog never ends: every batch comes back full.
type endlessBacklog struct {
	*ordertest.Repo
	batches int
}

func (r *endlessBacklog) CancelPendingOlderThan(_ context.Context, _ time.Time, limit int) ([]int, error) {
	r.batches++
	ids := make([]int, limit)
	for i := range ids {
		ids[i] = r.batches*limit + i
	}
	return ids, nil
}

// A run is bounded: past maxReapBatches it returns what it did and leaves the rest to the next
// firing, so one task never holds its slot indefinitely.
func TestReapPendingIsBoundedPerRun(t *testing.T) {
	repo := &endlessBacklog{Repo: ordertest.NewRepo()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := ordertest.NewCache(t)
	svc := order.NewService(repo, ordertest.Transactor{}, twoEach, &ordertest.Publisher{}, c, log)
	n, err := svc.ReapPending(context.Background(), time.Minute)
	if err != nil || repo.batches != order.MaxReapBatches || n != int64(order.MaxReapBatches*order.ReapBatchSize) {
		t.Fatalf("reaped %d in %d batches: %v", n, repo.batches, err)
	}
}

// A request id makes a create idempotent per customer: the repeat gets the first order back
// (created=false) and nothing is written or published again; the same id from another customer is
// another request, and the id reused for a different basket is refused, not answered with the
// first basket's order.
func TestCreateIsIdempotentPerCustomerAndRequestID(t *testing.T) {
	svc, repo, pub := ordertest.NewService(t, twoEach)
	ctx := context.Background()
	first, created, err := svc.Create(ctx, "c1", "key-1", lines("A", "B"))
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	again, created, err := svc.Create(ctx, "c1", "key-1", lines("A", "B"))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("repeat: %+v created=%v err=%v", again, created, err)
	}
	if len(repo.InTx()) != 1 || len(pub.Events()) != 1 {
		t.Fatalf("the repeat wrote or published again: writes=%v events=%d", repo.InTx(), len(pub.Events()))
	}

	other, created, err := svc.Create(ctx, "c2", "key-1", lines("A", "B"))
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("the same id from another customer is another request: %+v created=%v err=%v", other, created, err)
	}
	_, _, err = svc.Create(ctx, "c1", "key-1", lines("A"))
	if e, _ := apperror.As(err); e == nil || e.Code != order.CodeRequestIDReused || e.Status != 422 {
		t.Fatalf("an id reused for another basket: %v", err)
	}
	for range 2 {
		if _, created, err := svc.Create(ctx, "c1", "", lines("A")); err != nil || !created {
			t.Fatalf("without an id every request creates: created=%v err=%v", created, err)
		}
	}
}

// missOnce hides the first lookup, as when a repeat reaches the lookup before the first request
// committed and reaches the insert after it did.
type missOnce struct {
	*ordertest.Repo
	missed bool
}

func (r *missOnce) FindByRequestID(ctx context.Context, customerID, requestID string) (*order.Order, bool, error) {
	if !r.missed {
		r.missed = true
		return nil, false, nil
	}
	return r.Repo.FindByRequestID(ctx, customerID, requestID)
}

func TestARepeatThatRacedTheFirstRequestGetsItsOrder(t *testing.T) {
	svc, repo, pub := ordertest.NewService(t, twoEach)
	ctx := context.Background()
	first, _, err := svc.Create(ctx, "c1", "key-1", lines("A"))
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := ordertest.NewCache(t)
	racing := order.NewService(&missOnce{Repo: repo}, ordertest.Transactor{}, twoEach, pub, c, log)
	again, created, err := racing.Create(ctx, "c1", "key-1", lines("A"))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("the insert's duplicate is answered with the first order: %+v created=%v err=%v", again, created, err)
	}
	if len(pub.Events()) != 1 {
		t.Fatalf("the refused insert published nothing: %d events", len(pub.Events()))
	}
}

package order

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/cache"
	"example.com/msvcservice/pkg/messaging"
	"example.com/msvcservice/pkg/paging"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeRepo is an in-memory Repository that also records whether calls happened inside the
// transaction (the context carries a marker while fakeTransactor runs fn).
type fakeRepo struct {
	orders map[int]*Order
	next   int
	inTx   []string
}

type txKey struct{}

func inTx(ctx context.Context) bool { v, _ := ctx.Value(txKey{}).(bool); return v }

func (r *fakeRepo) Insert(ctx context.Context, o *Order) error {
	r.next++
	o.ID = r.next
	o.Version = 1
	o.CreatedAt, o.UpdatedAt = time.Now(), time.Now()
	copied := *o
	r.orders[o.ID] = &copied
	if inTx(ctx) {
		r.inTx = append(r.inTx, "insert")
	}
	return nil
}

func (r *fakeRepo) Get(_ context.Context, id int, customerID string) (*Order, error) {
	o, ok := r.orders[id]
	if !ok || (customerID != "" && o.CustomerID != customerID) {
		return nil, apperror.NotFound("Order", id)
	}
	copied := *o
	return &copied, nil
}

func (r *fakeRepo) List(_ context.Context, customerID string, _ paging.Request) ([]Order, int64, error) {
	var out []Order
	for _, o := range r.orders {
		if o.CustomerID == customerID {
			out = append(out, *o)
		}
	}
	return out, int64(len(out)), nil
}

func (r *fakeRepo) ListAll(context.Context, paging.CursorRequest) ([]Order, error) {
	out := make([]Order, 0, len(r.orders))
	for _, o := range r.orders {
		out = append(out, *o)
	}
	return out, nil
}

func (r *fakeRepo) UpdateStatus(_ context.Context, o *Order) error {
	stored := r.orders[o.ID]
	if stored.Version != o.Version {
		return apperror.ConcurrencyConflict()
	}
	stored.Status = o.Status
	stored.Version++
	o.Version = stored.Version
	return nil
}

// CancelPendingOlderThan cancels every PENDING order (the fake ignores the cutoff).
func (r *fakeRepo) CancelPendingOlderThan(context.Context, time.Time, int) ([]int, error) {
	var ids []int
	for id, o := range r.orders {
		if o.Status == StatusPending {
			o.Status = StatusCancelled
			o.Version++
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// fakeTransactor marks the context as transactional and runs the AfterCommit work only once fn
// returned without an error, the way pkg/postgres does.
type fakeTransactor struct{}

type hooksKey struct{}

func (fakeTransactor) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	var hooks []func(context.Context)
	if err := fn(context.WithValue(context.WithValue(ctx, txKey{}, true), hooksKey{}, &hooks)); err != nil {
		return err
	}
	for _, h := range hooks {
		h(ctx)
	}
	return nil
}

func (fakeTransactor) AfterCommit(ctx context.Context, fn func(ctx context.Context)) {
	if hooks, ok := ctx.Value(hooksKey{}).(*[]func(context.Context)); ok {
		*hooks = append(*hooks, fn)
		return
	}
	fn(ctx)
}

type fakeInventory struct {
	unavailable map[string]bool
	err         error
}

func (f fakeInventory) Quote(_ context.Context, sku string, _ int) (Quote, error) {
	if f.err != nil {
		return Quote{}, f.err
	}
	return Quote{Available: !f.unavailable[sku], UnitPrice: decimal.NewFromInt(2)}, nil
}

type fakePublisher struct {
	events []messaging.Event
	inTx   bool
}

func (p *fakePublisher) Publish(ctx context.Context, e messaging.Event) error {
	p.events = append(p.events, e)
	p.inTx = inTx(ctx)
	return nil
}

func newService(inv fakeInventory) (*Service, *fakeRepo, *fakePublisher) {
	repo := &fakeRepo{orders: map[int]*Order{}}
	pub := &fakePublisher{}
	c := cache.New(cache.Config{DefaultTTL: time.Minute, LocalTTL: time.Minute, LocalMaxEntries: 10}, nil, quiet)
	return NewService(repo, fakeTransactor{}, inv, pub, c, quiet), repo, pub
}

func lines(skus ...string) []Line {
	out := make([]Line, 0, len(skus))
	for _, s := range skus {
		out = append(out, Line{SKU: s, Quantity: 1})
	}
	return out
}

func TestCreateCommitsOrderAndEventTogether(t *testing.T) {
	svc, repo, pub := newService(fakeInventory{})
	o, err := svc.Create(context.Background(), "c1", lines("A", "B"))
	if err != nil {
		t.Fatal(err)
	}
	// The price is the inventory service's (2 per unit), never the client's.
	if o.ID != 1 || o.Status != StatusPending || !o.TotalAmount.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("%+v", o)
	}
	if len(repo.inTx) != 1 || !pub.inTx {
		t.Fatal("the insert and the publish must run inside the transaction")
	}
	ev, ok := pub.events[0].(CreatedEvent)
	if !ok || ev.OrderID != 1 || ev.EventName() != EventCreated {
		t.Fatalf("event %+v", pub.events[0])
	}
}

func TestCreateRefusesUnavailableAndPropagatesUpstreamErrors(t *testing.T) {
	svc, _, pub := newService(fakeInventory{unavailable: map[string]bool{"B": true}})
	_, err := svc.Create(context.Background(), "c1", lines("A", "B"))
	if e, _ := apperror.As(err); e == nil || e.Code != CodeItemUnavailable || e.Status != 422 {
		t.Fatalf("got %v", err)
	}
	if len(pub.events) != 0 {
		t.Fatal("nothing is published when the order is refused")
	}

	down := apperror.Upstream("inventory", "down", errors.New("x"))
	svc, _, _ = newService(fakeInventory{err: down})
	if _, err := svc.Create(context.Background(), "c1", lines("A")); !errors.Is(err, down) {
		t.Fatalf("upstream errors pass through: %v", err)
	}
}

func TestGetScopesByCustomerWithoutLeaking(t *testing.T) {
	svc, _, _ := newService(fakeInventory{})
	o, _ := svc.Create(context.Background(), "c1", lines("A"))
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
	svc, repo, _ := newService(fakeInventory{})
	o, _ := svc.Create(context.Background(), "c1", lines("A"))
	if err := svc.Confirm(context.Background(), o.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Confirm(context.Background(), o.ID); err != nil {
		t.Fatal("a second confirmation is a no-op, not an error")
	}
	if repo.orders[o.ID].Status != StatusConfirmed {
		t.Fatal("status persisted")
	}
	if err := svc.Cancel(context.Background(), o.ID, "c1"); apperror.StatusOf(err) != 409 {
		t.Fatalf("confirmed orders cannot be cancelled: %v", err)
	}
	// The cache was invalidated by Confirm, so the read sees CONFIRMED.
	got, _ := svc.Get(context.Background(), o.ID, "c1")
	if got.Status != StatusConfirmed {
		t.Fatal("cache invalidated after a status change")
	}
}

func TestCreateChecksTheRequestBeforeCallingInventory(t *testing.T) {
	svc, _, _ := newService(fakeInventory{err: errors.New("inventory must not be called")})
	_, err := svc.Create(context.Background(), "c1", []Line{{SKU: "A", Quantity: 0}})
	if e, _ := apperror.As(err); e == nil || e.Code != CodeItemInvalid {
		t.Fatalf("got %v", err)
	}
}

// The consumer confirms inside the inbox transaction: a read that lands before the commit must not
// find the entry already gone (it would cache the uncommitted-away PENDING row for a whole TTL).
func TestStatusChangesInvalidateTheCacheOnlyAfterCommit(t *testing.T) {
	svc, _, _ := newService(fakeInventory{})
	ctx := context.Background()
	o, _ := svc.Create(ctx, "c1", lines("A"))
	if got, _ := svc.Get(ctx, o.ID, "c1"); got.Status != StatusPending { // now cached
		t.Fatal(got.Status)
	}
	err := fakeTransactor{}.WithTx(ctx, func(ctx context.Context) error {
		if err := svc.Confirm(ctx, o.ID); err != nil {
			return err
		}
		if got, _ := svc.Get(ctx, o.ID, "c1"); got.Status != StatusPending {
			t.Error("the entry was dropped before the commit")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(ctx, o.ID, "c1"); got.Status != StatusConfirmed {
		t.Fatalf("after the commit the read sees the change: %s", got.Status)
	}
}

func TestReapingInvalidatesEveryCancelledOrder(t *testing.T) {
	svc, _, _ := newService(fakeInventory{})
	ctx := context.Background()
	o, _ := svc.Create(ctx, "c1", lines("A"))
	_, _ = svc.Get(ctx, o.ID, "c1") // cached as PENDING
	n, err := svc.ReapPending(ctx, time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("reaped %d %v", n, err)
	}
	if got, _ := svc.Get(ctx, o.ID, "c1"); got.Status != StatusCancelled {
		t.Fatalf("a reaped order must not be served PENDING from the cache: %s", got.Status)
	}
}

// Package ordertest holds the in-memory fakes of the order package's ports, shared by every test
// that drives order.Service without a database, a broker or the inventory service: the service's
// own tests, the handlers', the consumers' and the tasks'. They are fakes, not mocks: each one
// behaves like its adapter (keyset pages newest first, the version check on a status change,
// after-commit hooks) and records what a test may want to assert. Nothing here reaches outside
// the process, so the package stays under internal/order and keeps that package's import rules.
package ordertest

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/order"
	"github.com/guangliangus/go-msvc-kit/apperror"
	"github.com/guangliangus/go-msvc-kit/cache"
	"github.com/guangliangus/go-msvc-kit/messaging"
	"github.com/guangliangus/go-msvc-kit/paging"
)

type txKey struct{}

type hooksKey struct{}

// InTx reports whether ctx is inside a Transactor transaction.
func InTx(ctx context.Context) bool { v, _ := ctx.Value(txKey{}).(bool); return v }

// NewService wires an order.Service over the fakes, with an L1-only cache and a silent logger:
// the one-line setup for a test that drives the service. The repository and the publisher come
// back for assertions.
func NewService(inv Inventory) (*order.Service, *Repo, *Publisher) {
	repo, pub := NewRepo(), &Publisher{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := cache.New(cache.Config{DefaultTTL: time.Minute, LocalTTL: time.Minute, LocalMaxEntries: 10}, nil, log)
	return order.NewService(repo, Transactor{}, inv, pub, c, log), repo, pub
}

// Repo is an in-memory order.Repository: ids are issued in insert order, lists are newest first,
// ListAll pages by keyset, UpdateStatus checks the version the way the xmin guard does, and every
// write made inside a Transactor transaction is recorded (see InTx).
type Repo struct {
	mu     sync.Mutex
	orders map[int]*order.Order
	next   int
	inTx   []string
}

// NewRepo builds an empty repository.
func NewRepo() *Repo { return &Repo{orders: map[int]*order.Order{}} }

// Stored returns a copy of the stored order, for assertions.
func (r *Repo) Stored(id int) (order.Order, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[id]
	if !ok {
		return order.Order{}, false
	}
	return *o, true
}

// InTx lists the writes that ran inside a transaction, in order: "insert", "update_status" or
// "cancel_pending".
func (r *Repo) InTx() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.inTx...)
}

func (r *Repo) record(ctx context.Context, write string) {
	if InTx(ctx) {
		r.inTx = append(r.inTx, write)
	}
}

// Insert implements order.Repository.
func (r *Repo) Insert(ctx context.Context, o *order.Order) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	o.ID = r.next
	o.Version = 1
	o.CreatedAt, o.UpdatedAt = time.Now(), time.Now()
	copied := *o
	r.orders[o.ID] = &copied
	r.record(ctx, "insert")
	return nil
}

// Get implements order.Repository: a missing order and another customer's look the same (404).
func (r *Repo) Get(_ context.Context, id int, customerID string) (*order.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[id]
	if !ok || (customerID != "" && o.CustomerID != customerID) {
		return nil, apperror.NotFound("Order", id)
	}
	copied := *o
	return &copied, nil
}

// List implements order.Repository: the requested page of one customer's orders, newest first,
// and that customer's total.
func (r *Repo) List(_ context.Context, customerID string, page paging.Request) ([]order.Order, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []order.Order
	for id := r.next; id >= 1; id-- {
		if o, ok := r.orders[id]; ok && o.CustomerID == customerID {
			all = append(all, *o)
		}
	}
	start := min(page.Offset(), len(all))
	end := min(start+page.Limit(), len(all))
	return all[start:end], int64(len(all)), nil
}

// ListAll implements order.Repository: ids below the cursor, newest first, up to page.Fetch().
func (r *Repo) ListAll(_ context.Context, page paging.CursorRequest) ([]order.Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []order.Order
	for id := r.next; id >= 1 && len(out) < page.Fetch(); id-- {
		o, ok := r.orders[id]
		if ok && (page.After == 0 || int64(id) < page.After) {
			out = append(out, *o)
		}
	}
	return out, nil
}

// UpdateStatus implements order.Repository with the version check: a stale version is a 409.
func (r *Repo) UpdateStatus(ctx context.Context, o *order.Order) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, ok := r.orders[o.ID]
	if !ok {
		return apperror.NotFound("Order", o.ID)
	}
	if stored.Version != o.Version {
		return apperror.ConcurrencyConflict()
	}
	stored.Status = o.Status
	stored.UpdatedAt = time.Now()
	stored.Version++
	o.Version = stored.Version
	r.record(ctx, "update_status")
	return nil
}

// CancelPendingOlderThan implements order.Repository for tests, whose orders are all fresh: it
// ignores the cutoff and cancels up to limit PENDING orders, oldest first, returning their ids.
func (r *Repo) CancelPendingOlderThan(ctx context.Context, _ time.Time, limit int) ([]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []int
	for id := 1; id <= r.next && len(ids) < limit; id++ {
		if o, ok := r.orders[id]; ok && o.Status == order.StatusPending {
			o.Status = order.StatusCancelled
			o.UpdatedAt = time.Now()
			o.Version++
			ids = append(ids, id)
		}
	}
	r.record(ctx, "cancel_pending")
	return ids, nil
}

// Transactor is the order.Transactor fake: it marks the context as transactional (InTx) and runs
// the AfterCommit hooks only once fn returned without an error, the way the kit's postgres
// package does. Outside a transaction AfterCommit runs fn at once.
type Transactor struct{}

// WithTx implements order.Transactor.
func (Transactor) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	var hooks []func(context.Context)
	if err := fn(context.WithValue(context.WithValue(ctx, txKey{}, true), hooksKey{}, &hooks)); err != nil {
		return err
	}
	for _, h := range hooks {
		h(ctx)
	}
	return nil
}

// AfterCommit implements order.Transactor.
func (Transactor) AfterCommit(ctx context.Context, fn func(ctx context.Context)) {
	if hooks, ok := ctx.Value(hooksKey{}).(*[]func(context.Context)); ok {
		*hooks = append(*hooks, fn)
		return
	}
	fn(ctx)
}

// Inventory is the order.InventoryClient fake: every SKU is available at UnitPrice unless it is
// listed in Unavailable; Err, when set, fails every quote the way an outage would.
type Inventory struct {
	UnitPrice   decimal.Decimal
	Unavailable map[string]bool
	Err         error
}

// Quote implements order.InventoryClient.
func (inv Inventory) Quote(_ context.Context, sku string, _ int) (order.Quote, error) {
	if inv.Err != nil {
		return order.Quote{}, inv.Err
	}
	return order.Quote{Available: !inv.Unavailable[sku], UnitPrice: inv.UnitPrice}, nil
}

// Publisher is the messaging.Publisher fake: it records every event and whether the last Publish
// ran inside a transaction, which the outbox publisher needs to write its row with the order.
type Publisher struct {
	mu     sync.Mutex
	events []messaging.Event
	inTx   bool
}

// Publish implements messaging.Publisher.
func (p *Publisher) Publish(ctx context.Context, e messaging.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	p.inTx = InTx(ctx)
	return nil
}

// Events returns the published events in order.
func (p *Publisher) Events() []messaging.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]messaging.Event(nil), p.events...)
}

// InTx reports whether the last Publish ran inside a transaction.
func (p *Publisher) InTx() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inTx
}

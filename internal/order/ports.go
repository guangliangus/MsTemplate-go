package order

import (
	"context"
	"time"

	"github.com/shopspring/decimal"

	"github.com/guangliangus/go-msvc-kit/paging"
)

// Repository is the persistence port; internal/postgres implements it. Only interfaces for things
// that cross a process boundary, or must be swapped in a unit test, belong in this package: pure
// computation, validation and mapping do not.
type Repository interface {
	// Insert stores the order and its items and fills in the ids, audit fields and version.
	Insert(ctx context.Context, o *Order) error
	// Get loads an order by id, scoped to a customer when customerID is not empty (404 otherwise).
	Get(ctx context.Context, id int, customerID string) (*Order, error)
	// List returns a numbered page of ONE customer's orders, newest first, and that customer's total.
	List(ctx context.Context, customerID string, page paging.Request) ([]Order, int64, error)
	// ListAll returns every customer's orders newest first as a keyset page: up to page.Fetch()
	// orders whose id is below page.After (any id when After is 0). Nothing is counted.
	ListAll(ctx context.Context, page paging.CursorRequest) ([]Order, error)
	// UpdateStatus persists a status change guarded by the order's version (409 on a stale one).
	UpdateStatus(ctx context.Context, o *Order) error
	// CancelPendingOlderThan cancels up to limit PENDING orders created before cutoff and returns
	// their ids.
	CancelPendingOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]int, error)
}

// Transactor runs fn inside one database transaction carried on the context, so every repository
// call made with that context joins it; the kit's postgres.DB satisfies it.
type Transactor interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
	// AfterCommit runs fn once the transaction on ctx has committed (at once when there is none):
	// the place for side effects outside the database, such as invalidating a cache entry.
	AfterCommit(ctx context.Context, fn func(ctx context.Context))
}

// Quote is the inventory service's answer for one line: whether it can be sold, and at what price.
type Quote struct {
	Available bool
	UnitPrice decimal.Decimal
}

// InventoryClient is the downstream inventory port; internal/inventory implements it.
type InventoryClient interface {
	// Quote reports whether quantity units of the SKU can be sold, and the unit price.
	Quote(ctx context.Context, sku string, quantity int) (Quote, error)
}

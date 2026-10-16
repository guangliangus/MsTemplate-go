// Package postgres holds the service's own repositories: raw SQL over the kit's pool, with the
// transaction taken from the context, audit columns written explicitly from the context's actor,
// and xmin as the optimistic concurrency token.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/pkg/actor"
	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/paging"
	kitpg "example.com/msvcservice/pkg/postgres"
)

// OrderRepository implements order.Repository.
type OrderRepository struct {
	db *kitpg.DB
}

// NewOrderRepository builds the repository.
func NewOrderRepository(db *kitpg.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

type orderRow struct {
	ID          int             `db:"id"`
	CustomerID  string          `db:"customer_id"`
	Status      string          `db:"status"`
	TotalAmount decimal.Decimal `db:"total_amount"`
	CreatedAt   time.Time       `db:"created_at"`
	CreatedBy   string          `db:"created_by"`
	UpdatedAt   time.Time       `db:"updated_at"`
	UpdatedBy   string          `db:"updated_by"`
	Version     uint32          `db:"xmin"`
}

type itemRow struct {
	ID        int             `db:"id"`
	OrderID   int             `db:"order_id"`
	SKU       string          `db:"sku"`
	Quantity  int             `db:"quantity"`
	UnitPrice decimal.Decimal `db:"unit_price"`
}

const orderColumns = `id, customer_id, status, total_amount, created_at, created_by, updated_at, updated_by, xmin`

// Insert writes the order and its items; the context's actor becomes created_by / updated_by.
func (r *OrderRepository) Insert(ctx context.Context, o *order.Order) error {
	by := actor.FromContext(ctx)
	q := r.db.Q(ctx)
	row := q.QueryRow(ctx, `
		INSERT INTO orders (customer_id, status, total_amount, created_at, created_by, updated_at, updated_by)
		VALUES ($1, $2, $3, now(), $4, now(), $4)
		RETURNING id, created_at, updated_at, xmin`,
		o.CustomerID, string(o.Status), o.TotalAmount, by)
	if err := row.Scan(&o.ID, &o.CreatedAt, &o.UpdatedAt, &o.Version); err != nil {
		return fmt.Errorf("orders: insert: %w", err)
	}
	o.CreatedBy, o.UpdatedBy = by, by

	n := len(o.Items)
	skus, quantities, prices := make([]string, n), make([]int32, n), make([]decimal.Decimal, n)
	for i, it := range o.Items {
		skus[i], prices[i] = it.SKU, it.UnitPrice
		quantities[i] = int32(it.Quantity) //nolint:gosec // bounded by the domain's quantity check
	}
	rows, err := q.Query(ctx, `
		INSERT INTO order_items (order_id, sku, quantity, unit_price)
		SELECT $1, c.sku, c.quantity, CAST(c.unit_price AS numeric)
		FROM   unnest(CAST($2 AS text[]), CAST($3 AS integer[]), CAST($4 AS text[])) AS c(sku, quantity, unit_price)
		RETURNING id`, o.ID, skus, quantities, decimalsAsText(prices))
	if err != nil {
		return fmt.Errorf("orders: insert items: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return fmt.Errorf("orders: read item ids: %w", err)
	}
	for i := range o.Items {
		o.Items[i].ID = ids[i]
	}
	return nil
}

func decimalsAsText(values []decimal.Decimal) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = v.String()
	}
	return out
}

// Get loads one order with its items; a missing or foreign order is 404.
func (r *OrderRepository) Get(ctx context.Context, id int, customerID string) (*order.Order, error) {
	q := r.db.Q(ctx)
	rows, err := q.Query(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE id = $1 AND ($2 = '' OR customer_id = $2)`, id, customerID)
	if err != nil {
		return nil, fmt.Errorf("orders: get %d: %w", id, err)
	}
	head, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[orderRow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperror.NotFound("Order", id)
	}
	if err != nil {
		return nil, fmt.Errorf("orders: read %d: %w", id, err)
	}
	items, err := r.itemsFor(ctx, []int{id})
	if err != nil {
		return nil, err
	}
	o := toDomain(head)
	o.Items = items[id]
	return &o, nil
}

// List pages one customer's orders newest first; both statements walk ix_orders_customer_id.
// One query per access path rather than a "$1 = ” OR customer_id = $1" catch-all: a generic plan
// for the catch-all cannot use the index for either case.
func (r *OrderRepository) List(ctx context.Context, customerID string, page paging.Request,
) ([]order.Order, int64, error) {
	q := r.db.Q(ctx)
	var total int64
	err := q.QueryRow(ctx, `SELECT count(*) FROM orders WHERE customer_id = $1`, customerID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("orders: count: %w", err)
	}
	rows, err := q.Query(ctx, `
		SELECT `+orderColumns+`
		FROM   orders
		WHERE  customer_id = $1
		ORDER  BY id DESC
		LIMIT  $2 OFFSET $3`, customerID, page.Limit(), page.Offset())
	if err != nil {
		return nil, 0, fmt.Errorf("orders: list: %w", err)
	}
	out, err := r.withItems(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListAll pages every customer's orders newest first by keyset: an index range scan on the
// primary key that costs the same on any page, and no count.
func (r *OrderRepository) ListAll(ctx context.Context, page paging.CursorRequest) ([]order.Order, error) {
	// Two statements, not "($1 = 0 OR id < $1)": the bound must reach the planner as a plain range
	// condition, or a generic plan walks the index from the newest row down to the cursor on every
	// page. The cursor is compared as bigint, so any value a client sends — even a forged one past
	// the int range — is a valid bound rather than an encoding error.
	q := r.db.Q(ctx)
	var rows pgx.Rows
	var err error
	if page.After > 0 {
		rows, err = q.Query(ctx, `
			SELECT `+orderColumns+`
			FROM   orders
			WHERE  id < $2::bigint
			ORDER  BY id DESC
			LIMIT  $1`, page.Fetch(), page.After)
	} else {
		rows, err = q.Query(ctx, `
			SELECT `+orderColumns+`
			FROM   orders
			ORDER  BY id DESC
			LIMIT  $1`, page.Fetch())
	}
	if err != nil {
		return nil, fmt.Errorf("orders: list all: %w", err)
	}
	return r.withItems(ctx, rows)
}

// withItems collects order rows and loads their items in one more query.
func (r *OrderRepository) withItems(ctx context.Context, rows pgx.Rows) ([]order.Order, error) {
	heads, err := pgx.CollectRows(rows, pgx.RowToStructByName[orderRow])
	if err != nil {
		return nil, fmt.Errorf("orders: read list: %w", err)
	}
	ids := make([]int, len(heads))
	for i, h := range heads {
		ids[i] = h.ID
	}
	items, err := r.itemsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]order.Order, len(heads))
	for i, h := range heads {
		out[i] = toDomain(h)
		out[i].Items = items[h.ID]
	}
	return out, nil
}

// UpdateStatus writes the status guarded by xmin: zero rows means somebody else wrote first.
func (r *OrderRepository) UpdateStatus(ctx context.Context, o *order.Order) error {
	by := actor.FromContext(ctx)
	row := r.db.Q(ctx).QueryRow(ctx, `
		UPDATE orders
		SET    status = $2, updated_at = now(), updated_by = $3
		WHERE  id = $1 AND xmin = $4
		RETURNING updated_at, xmin`, o.ID, string(o.Status), by, o.Version)
	err := row.Scan(&o.UpdatedAt, &o.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ConcurrencyConflict()
	}
	if err != nil {
		return fmt.Errorf("orders: update status of %d: %w", o.ID, err)
	}
	o.UpdatedBy = by
	return nil
}

// CancelPendingOlderThan cancels up to limit stale PENDING orders, claiming them with SKIP LOCKED
// so concurrent reapers never fight over a row, and returns their ids (the caller drops them from
// the cache).
func (r *OrderRepository) CancelPendingOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]int, error) {
	rows, err := r.db.Q(ctx).Query(ctx, `
		UPDATE orders
		SET    status = $3, updated_at = now(), updated_by = $4
		WHERE  id IN (
			SELECT id FROM orders
			WHERE  status = $5 AND created_at < $1
			ORDER  BY id
			LIMIT  $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id`,
		cutoff, limit, string(order.StatusCancelled), actor.FromContext(ctx), string(order.StatusPending))
	if err != nil {
		return nil, fmt.Errorf("orders: reap pending: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, fmt.Errorf("orders: read reaped ids: %w", err)
	}
	return ids, nil
}

func (r *OrderRepository) itemsFor(ctx context.Context, orderIDs []int) (map[int][]order.Item, error) {
	out := make(map[int][]order.Item, len(orderIDs))
	if len(orderIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.Q(ctx).Query(ctx, `
		SELECT id, order_id, sku, quantity, unit_price
		FROM   order_items
		WHERE  order_id = ANY($1)
		ORDER  BY id`, orderIDs)
	if err != nil {
		return nil, fmt.Errorf("orders: items: %w", err)
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[itemRow])
	if err != nil {
		return nil, fmt.Errorf("orders: read items: %w", err)
	}
	for _, it := range items {
		out[it.OrderID] = append(out[it.OrderID],
			order.Item{ID: it.ID, SKU: it.SKU, Quantity: it.Quantity, UnitPrice: it.UnitPrice})
	}
	return out, nil
}

func toDomain(r orderRow) order.Order {
	return order.Order{
		ID:          r.ID,
		CustomerID:  r.CustomerID,
		Status:      order.Status(r.Status),
		TotalAmount: r.TotalAmount,
		Items:       []order.Item{},
		CreatedAt:   r.CreatedAt,
		CreatedBy:   r.CreatedBy,
		UpdatedAt:   r.UpdatedAt,
		UpdatedBy:   r.UpdatedBy,
		Version:     r.Version,
	}
}

// Package postgres holds the service's own repositories, written with GORM over the kit's pool.
// Every call goes through Gorm.On(ctx), so it joins the transaction the context carries and the
// business rows commit together with the kit's outbox, inbox and task rows. The models are
// mappings of the hand-written DDL in db/*.sql (never migrated from here); audit columns come from
// the context's actor in the models' hooks, and orders.version is the optimistic concurrency
// token, kept by GORM's optimisticlock plugin.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/plugin/optimisticlock"

	"example.com/msvcservice/internal/order"
	"github.com/guangliangus/go-msvc-kit/actor"
	"github.com/guangliangus/go-msvc-kit/apperror"
	"github.com/guangliangus/go-msvc-kit/paging"
)

// requestIDIndex is the unique index that makes a create idempotent (db/0003_orders_request_id.sql).
const requestIDIndex = "ix_orders_customer_id_request_id"

// orderModel maps the orders table (db/0001_init.sql, 0002_orders_version.sql, 0003_orders_request_id.sql).
type orderModel struct {
	ID          int `gorm:"primaryKey"`
	CustomerID  string
	Status      string
	TotalAmount decimal.Decimal
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
	UpdatedBy   string
	RequestID   string
	// Version is bumped by every update made through GORM's model API (the plugin adds
	// "SET version = version + 1" and "WHERE version = <loaded>"). Raw SQL bypasses the plugin and
	// must bump it by hand.
	Version optimisticlock.Version
	Items   []orderItemModel `gorm:"foreignKey:OrderID"`
}

func (orderModel) TableName() string { return "orders" }

// BeforeCreate stamps the audit columns with the context's actor.
func (m *orderModel) BeforeCreate(tx *gorm.DB) error {
	by := actor.FromContext(tx.Statement.Context)
	m.CreatedBy, m.UpdatedBy = by, by
	return nil
}

// BeforeUpdate stamps updated_by on every model update, map updates included.
func (m *orderModel) BeforeUpdate(tx *gorm.DB) error {
	tx.Statement.SetColumn("UpdatedBy", actor.FromContext(tx.Statement.Context))
	return nil
}

// orderItemModel maps the order_items table.
type orderItemModel struct {
	ID        int `gorm:"primaryKey"`
	OrderID   int
	SKU       string `gorm:"column:sku"`
	Quantity  int
	UnitPrice decimal.Decimal
}

func (orderItemModel) TableName() string { return "order_items" }

// OrderRepository implements order.Repository.
type OrderRepository struct {
	gorm *Gorm
}

// NewOrderRepository builds the repository.
func NewOrderRepository(g *Gorm) *OrderRepository {
	return &OrderRepository{gorm: g}
}

// itemsByID preloads an order's items in id order.
func itemsByID(db *gorm.DB) *gorm.DB { return db.Order("id") }

// Insert writes the order and its items in one Create (GORM inserts the items after the order and
// fills their ids); the context's actor becomes created_by / updated_by. A request id the customer
// already used hits requestIDIndex — after waiting for the transaction that wrote it, when that one
// is still open — and comes back as order.ErrDuplicateRequest.
func (r *OrderRepository) Insert(ctx context.Context, o *order.Order) error {
	m := toModel(o)
	if err := r.gorm.On(ctx).Create(&m).Error; err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == requestIDIndex {
			return fmt.Errorf("orders: insert: %w", order.ErrDuplicateRequest)
		}
		return fmt.Errorf("orders: insert: %w", err)
	}
	*o = toDomain(m)
	return nil
}

// FindByRequestID loads the customer's order created by the request with this id; the lookup is
// requestIDIndex.
func (r *OrderRepository) FindByRequestID(ctx context.Context, customerID, requestID string,
) (*order.Order, bool, error) {
	var models []orderModel
	err := r.gorm.On(ctx).Preload("Items", itemsByID).
		Where("customer_id = ? AND request_id = ?", customerID, requestID).
		Limit(1).Find(&models).Error
	if err != nil {
		return nil, false, fmt.Errorf("orders: find by request id: %w", err)
	}
	if len(models) == 0 {
		return nil, false, nil
	}
	o := toDomain(models[0])
	return &o, true, nil
}

// Get loads one order with its items; a missing or foreign order is 404. customerID "" reads any
// customer's order (operators).
func (r *OrderRepository) Get(ctx context.Context, id int, customerID string) (*order.Order, error) {
	q := r.gorm.On(ctx).Preload("Items", itemsByID)
	if customerID != "" {
		q = q.Where("customer_id = ?", customerID)
	}
	var m orderModel
	err := q.First(&m, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apperror.NotFound("Order", id)
	}
	if err != nil {
		return nil, fmt.Errorf("orders: get %d: %w", id, err)
	}
	o := toDomain(m)
	return &o, nil
}

// List pages one customer's orders newest first; both statements walk ix_orders_customer_id.
func (r *OrderRepository) List(ctx context.Context, customerID string, page paging.Request,
) ([]order.Order, int64, error) {
	var total int64
	err := r.gorm.On(ctx).Model(&orderModel{}).Where("customer_id = ?", customerID).Count(&total).Error
	if err != nil {
		return nil, 0, fmt.Errorf("orders: count: %w", err)
	}
	var models []orderModel
	err = r.gorm.On(ctx).Preload("Items", itemsByID).
		Where("customer_id = ?", customerID).
		Order("id DESC").Limit(page.Limit()).Offset(page.Offset()).
		Find(&models).Error
	if err != nil {
		return nil, 0, fmt.Errorf("orders: list: %w", err)
	}
	return toDomains(models), total, nil
}

// ListAll pages every customer's orders newest first by keyset: an index range scan on the
// primary key that costs the same on any page, and no count. The bound is only added past the
// first page, so it reaches the planner as a plain range condition, never "($1 = 0 OR id < $1)".
func (r *OrderRepository) ListAll(ctx context.Context, page paging.CursorRequest) ([]order.Order, error) {
	q := r.gorm.On(ctx).Preload("Items", itemsByID).Order("id DESC").Limit(page.Fetch())
	if page.After > 0 {
		q = q.Where("id < ?", page.After)
	}
	var models []orderModel
	if err := q.Find(&models).Error; err != nil {
		return nil, fmt.Errorf("orders: list all: %w", err)
	}
	return toDomains(models), nil
}

// UpdateStatus writes the status guarded by version: zero rows means somebody else wrote first.
func (r *OrderRepository) UpdateStatus(ctx context.Context, o *order.Order) error {
	// The plugin adds "WHERE version = ?" only for a non-zero version: an order whose version was
	// never loaded would be written with no concurrency check at all, so it is refused here.
	if o.Version <= 0 {
		return fmt.Errorf("orders: update status of %d without a loaded version", o.ID)
	}
	m := orderModel{ID: o.ID, Version: optimisticlock.Version{Int64: int64(o.Version), Valid: true}}
	returning := clause.Returning{Columns: []clause.Column{
		{Name: "updated_at"}, {Name: "updated_by"}, {Name: "version"},
	}}
	res := r.gorm.On(ctx).Model(&m).Clauses(returning).Updates(map[string]any{"status": string(o.Status)})
	if res.Error != nil {
		return fmt.Errorf("orders: update status of %d: %w", o.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return apperror.ConcurrencyConflict()
	}
	o.UpdatedAt, o.UpdatedBy, o.Version = m.UpdatedAt, m.UpdatedBy, int(m.Version.Int64)
	return nil
}

// cancelPendingSQL claims stale PENDING orders with SKIP LOCKED, so concurrent reapers never fight
// over a row. It is Raw SQL — the shape GORM's builder does not express well — and Raw bypasses the
// optimisticlock plugin, so it bumps version itself: a confirmation racing the reaper must lose.
const cancelPendingSQL = `
	UPDATE orders
	SET    status = ?, updated_at = ?, updated_by = ?, version = version + 1
	WHERE  id IN (
		SELECT id FROM orders
		WHERE  status = ? AND created_at < ?
		ORDER  BY id
		LIMIT  ?
		FOR UPDATE SKIP LOCKED
	)
	RETURNING id`

// CancelPendingOlderThan cancels up to limit stale PENDING orders and returns their ids (the
// caller drops them from the cache).
func (r *OrderRepository) CancelPendingOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]int, error) {
	db := r.gorm.On(ctx)
	var ids []int
	err := db.Raw(cancelPendingSQL, string(order.StatusCancelled), db.NowFunc(), actor.FromContext(ctx),
		string(order.StatusPending), cutoff, limit).Scan(&ids).Error
	if err != nil {
		return nil, fmt.Errorf("orders: reap pending: %w", err)
	}
	return ids, nil
}

func toModel(o *order.Order) orderModel {
	m := orderModel{
		ID:          o.ID,
		CustomerID:  o.CustomerID,
		Status:      string(o.Status),
		TotalAmount: o.TotalAmount,
		RequestID:   o.RequestID,
		Items:       make([]orderItemModel, len(o.Items)),
	}
	for i, it := range o.Items {
		m.Items[i] = orderItemModel{ID: it.ID, SKU: it.SKU, Quantity: it.Quantity, UnitPrice: it.UnitPrice}
	}
	return m
}

func toDomain(m orderModel) order.Order {
	o := order.Order{
		ID:          m.ID,
		CustomerID:  m.CustomerID,
		Status:      order.Status(m.Status),
		TotalAmount: m.TotalAmount,
		Items:       make([]order.Item, len(m.Items)),
		CreatedAt:   m.CreatedAt,
		CreatedBy:   m.CreatedBy,
		UpdatedAt:   m.UpdatedAt,
		UpdatedBy:   m.UpdatedBy,
		RequestID:   m.RequestID,
		Version:     int(m.Version.Int64),
	}
	for i, it := range m.Items {
		o.Items[i] = order.Item{ID: it.ID, SKU: it.SKU, Quantity: it.Quantity, UnitPrice: it.UnitPrice}
	}
	return o
}

func toDomains(models []orderModel) []order.Order {
	out := make([]order.Order, len(models))
	for i, m := range models {
		out[i] = toDomain(m)
	}
	return out
}

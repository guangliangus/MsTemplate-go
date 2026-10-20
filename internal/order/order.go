// Package order is the reference bounded context, end to end: the aggregate with its invariants,
// state machine and stable error codes; the ports it needs from the outside world (persistence,
// a transaction boundary, the inventory service); the application service that orchestrates
// them; and the integration event it publishes. The team conventions show here: an int identity,
// a status column instead of a physical delete, audit columns, money as a decimal.
//
// The package never imports a driver or an adapter: internal/postgres and internal/inventory
// implement its ports and internal/app wires them; internal/httpapi owns the HTTP shape. Replace
// it with your real contexts and keep the shape: one package per context, interfaces declared
// here where they are consumed, implementations beside it.
package order

import (
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/guangliangus/go-msvc-kit/apperror"
)

// Status is the order's lifecycle state; the status column is the soft-delete marker too.
type Status string

// Order statuses.
const (
	StatusPending   Status = "PENDING"
	StatusConfirmed Status = "CONFIRMED"
	StatusCancelled Status = "CANCELLED"
)

// Error codes this module publishes, beside the platform's. Part of the client contract: add,
// never rename.
const (
	CodeItemsRequired   = "ORDER_ITEMS_REQUIRED"
	CodeItemInvalid     = "ORDER_ITEM_INVALID"
	CodeItemUnavailable = "ORDER_ITEM_UNAVAILABLE"
	CodeNotPending      = "ORDER_NOT_PENDING"
	CodeNotCancellable  = "ORDER_NOT_CANCELLABLE"
	CodeTotalTooLarge   = "ORDER_TOTAL_TOO_LARGE"
	// CodeRequestIDReused is an Idempotency-Key the customer already used for a different basket.
	CodeRequestIDReused = "ORDER_REQUEST_ID_REUSED"
)

// MaxItems bounds the lines one order may carry. It is a rule of the order, not of the wire format:
// CheckLines enforces it with a 422 whichever caller built the lines, and the HTTP request type
// deliberately carries no copy of it (internal/httpapi.CreateOrderRequest).
const MaxItems = 100

// MaxAmount is the largest amount an order stores: total_amount is numeric(10,2). Refusing it here
// is a 422 the client can act on, instead of the database's numeric overflow surfacing as a 500.
var MaxAmount = decimal.RequireFromString("99999999.99")

// Line is what a customer asks for: a SKU and a quantity. The price is not the customer's to
// state — a client that sent its own unit price could buy anything for nothing — so Service.Create
// quotes every line with the inventory service and builds the Items from the answers.
type Line struct {
	SKU      string
	Quantity int
}

// CheckLines validates a request before anything is priced: no downstream call for an order
// that could never be accepted.
func CheckLines(customerID string, lines []Line) error {
	if strings.TrimSpace(customerID) == "" {
		return apperror.DomainRule(CodeItemInvalid, "An order needs a customer.")
	}
	if len(lines) == 0 {
		return apperror.DomainRule(CodeItemsRequired, "An order needs at least one item.")
	}
	if len(lines) > MaxItems {
		return apperror.DomainRule(CodeItemInvalid, "An order may carry at most "+strconv.Itoa(MaxItems)+" items.")
	}
	for _, l := range lines {
		if strings.TrimSpace(l.SKU) == "" {
			return apperror.DomainRule(CodeItemInvalid, "Every item needs a SKU.")
		}
		if l.Quantity < 1 {
			return apperror.DomainRule(CodeItemInvalid, "Item '"+l.SKU+"' needs a quantity of at least 1.")
		}
	}
	return nil
}

// Item is one priced line of an order.
type Item struct {
	ID        int
	SKU       string
	Quantity  int
	UnitPrice decimal.Decimal
}

// LineTotal is quantity times unit price.
func (i Item) LineTotal() decimal.Decimal {
	return i.UnitPrice.Mul(decimal.NewFromInt(int64(i.Quantity)))
}

// Order is the aggregate root.
type Order struct {
	ID          int
	CustomerID  string
	Status      Status
	TotalAmount decimal.Decimal
	Items       []Item
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
	UpdatedBy   string
	// RequestID is the client's id for the request that created the order (its Idempotency-Key),
	// unique per customer; "" when the request carried none.
	RequestID string
	// Version is the optimistic concurrency token (the orders.version column, bumped by every
	// update): an update that carries a stale one affects zero rows and surfaces as 409
	// CONCURRENCY_CONFLICT.
	Version int
}

// New validates the invariants and builds a PENDING order from priced items: the line rules
// (CheckLines) and then what only the prices can tell (newChecked).
func New(customerID string, items []Item) (*Order, error) {
	lines := make([]Line, len(items))
	for i, item := range items {
		lines[i] = Line{SKU: item.SKU, Quantity: item.Quantity}
	}
	if err := CheckLines(customerID, lines); err != nil {
		return nil, err
	}
	return newChecked(customerID, items)
}

// newChecked builds the order from items whose lines CheckLines has already accepted — the service
// runs the line rules once, before it prices anything — and checks what only a price can violate:
// no negative price, the total within MaxAmount.
func newChecked(customerID string, items []Item) (*Order, error) {
	total := decimal.Zero
	for _, item := range items {
		if item.UnitPrice.IsNegative() {
			return nil, apperror.DomainRule(CodeItemInvalid, "Item '"+item.SKU+"' cannot have a negative price.")
		}
		total = total.Add(item.LineTotal())
	}
	if total.Round(2).GreaterThan(MaxAmount) {
		return nil, apperror.DomainRule(CodeTotalTooLarge,
			"The order total exceeds "+MaxAmount.String()+"; split it into smaller orders.")
	}
	return &Order{
		CustomerID:  strings.TrimSpace(customerID),
		Status:      StatusPending,
		TotalAmount: total.Round(2),
		Items:       items,
	}, nil
}

// createdFrom reports whether the order was created from exactly these lines, in this order: what
// a repeated request must ask for to be answered with this order.
func (o *Order) createdFrom(lines []Line) bool {
	if len(o.Items) != len(lines) {
		return false
	}
	for i, l := range lines {
		if o.Items[i].SKU != l.SKU || o.Items[i].Quantity != l.Quantity {
			return false
		}
	}
	return true
}

// Confirm moves a PENDING order to CONFIRMED.
func (o *Order) Confirm() error {
	if o.Status != StatusPending {
		return apperror.Conflict(CodeNotPending,
			"Only a PENDING order can be confirmed; this one is "+string(o.Status)+".")
	}
	o.Status = StatusConfirmed
	return nil
}

// Cancel moves a PENDING order to CANCELLED (the soft delete).
func (o *Order) Cancel() error {
	if o.Status != StatusPending {
		return apperror.Conflict(CodeNotCancellable,
			"Only a PENDING order can be cancelled; this one is "+string(o.Status)+".")
	}
	o.Status = StatusCancelled
	return nil
}

// Codes lists this context's error codes, for the guards that keep the message catalogue and the
// API document complete.
func Codes() []string {
	return []string{CodeItemsRequired, CodeItemInvalid, CodeItemUnavailable, CodeNotPending, CodeNotCancellable,
		CodeTotalTooLarge, CodeRequestIDReused}
}

package order

import (
	"testing"

	"github.com/shopspring/decimal"

	"github.com/guangliangus/go-msvc-kit/apperror"
	"github.com/guangliangus/go-msvc-kit/messaging"
)

func price(s string) decimal.Decimal {
	d, _ := decimal.NewFromString(s)
	return d
}

func manyItems(n int) []Item {
	items := make([]Item, n)
	for i := range items {
		items[i] = Item{SKU: "A", Quantity: 1, UnitPrice: price("1")}
	}
	return items
}

// MaxItems is the order's rule, enforced here for every caller, not a limit of the HTTP shape.
func TestNewEnforcesMaxItemsInclusive(t *testing.T) {
	if _, err := New("1", manyItems(MaxItems)); err != nil {
		t.Fatalf("exactly MaxItems lines are allowed: %v", err)
	}
	if e, _ := apperror.As(CheckLines("1", make([]Line, MaxItems+1))); e == nil || e.Code != CodeItemInvalid {
		t.Fatal("one more line is refused before anything is priced")
	}
}

func TestNewComputesTotalAndStartsPending(t *testing.T) {
	o, err := New(" 42 ", []Item{{SKU: "A", Quantity: 2, UnitPrice: price("19.99")}, {SKU: "B", Quantity: 1, UnitPrice: price("0.015")}})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusPending || o.CustomerID != "42" {
		t.Fatalf("%+v", o)
	}
	if !o.TotalAmount.Equal(price("40.00")) {
		t.Fatalf("total %s", o.TotalAmount)
	}
}

func TestNewRefusesInvalidOrders(t *testing.T) {
	cases := map[string]struct {
		customer string
		items    []Item
		code     string
	}{
		"no customer": {"", []Item{{SKU: "A", Quantity: 1}}, CodeItemInvalid},
		"no items":    {"1", nil, CodeItemsRequired},
		"no sku":      {"1", []Item{{SKU: " ", Quantity: 1}}, CodeItemInvalid},
		"zero qty":    {"1", []Item{{SKU: "A", Quantity: 0}}, CodeItemInvalid},
		"too many":    {"1", manyItems(MaxItems + 1), CodeItemInvalid},
		"negative":    {"1", []Item{{SKU: "A", Quantity: 1, UnitPrice: price("-1")}}, CodeItemInvalid},
		"too large":   {"1", []Item{{SKU: "A", Quantity: 1000, UnitPrice: price("100000")}}, CodeTotalTooLarge},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(tc.customer, tc.items)
			e, ok := apperror.As(err)
			if !ok || e.Code != tc.code || e.Status != 422 {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestTransitions(t *testing.T) {
	o, _ := New("1", []Item{{SKU: "A", Quantity: 1, UnitPrice: price("1")}})
	if err := o.Confirm(); err != nil || o.Status != StatusConfirmed {
		t.Fatal(err)
	}
	if err := o.Confirm(); err == nil {
		t.Fatal("confirming twice is a conflict")
	}
	if e, _ := apperror.As(o.Cancel()); e == nil || e.Code != CodeNotCancellable || e.Status != 409 {
		t.Fatal("a confirmed order cannot be cancelled")
	}
	fresh, _ := New("1", []Item{{SKU: "A", Quantity: 1, UnitPrice: price("1")}})
	if err := fresh.Cancel(); err != nil || fresh.Status != StatusCancelled {
		t.Fatal(err)
	}
}

// The order id is the event's partition key: on Kafka an order's events stay on one partition, in
// publish order.
func TestCreatedEventIsKeyedByTheOrder(t *testing.T) {
	var event messaging.Keyed = CreatedEvent{OrderID: 42}
	if event.PartitionKey() != "42" {
		t.Fatalf("key = %q", event.PartitionKey())
	}
}

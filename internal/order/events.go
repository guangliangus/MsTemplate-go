package order

import (
	"strconv"

	"github.com/guangliangus/go-msvc-kit/money"
)

// EventCreated is the routing key of CreatedEvent: explicit and versioned, never derived from the
// Go type, so a rename cannot silently change it.
const EventCreated = "orders.order-created.v1"

// CreatedEvent is published when an order is accepted. It carries business keys only; consumers
// that need more read it through the API.
type CreatedEvent struct {
	OrderID     int          `json:"orderId"`
	CustomerID  string       `json:"customerId"`
	TotalAmount money.Amount `json:"totalAmount"`
}

// EventName implements messaging.Event.
func (CreatedEvent) EventName() string { return EventCreated }

// PartitionKey implements messaging.Keyed: the order id. On Kafka every event of one order lands
// on one partition and is consumed in publish order; RabbitMQ ignores it.
func (e CreatedEvent) PartitionKey() string { return strconv.Itoa(e.OrderID) }

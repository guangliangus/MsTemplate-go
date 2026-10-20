package consumers_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/consumers"
	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/internal/order/ordertest"
	"github.com/guangliangus/go-msvc-kit/apperror"
	"github.com/guangliangus/go-msvc-kit/messaging"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestHandleConfirmsTheOrderAndTolerantsRedelivery(t *testing.T) {
	svc, repo, _ := ordertest.NewService(t, ordertest.Inventory{UnitPrice: decimal.NewFromInt(1)})
	ctx := context.Background()
	o, _, err := svc.Create(ctx, "c1", "", []order.Line{{SKU: "A", Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	h := consumers.NewOrderCreatedHandler(svc, quiet)
	msg := messaging.Message[order.CreatedEvent]{
		Body:      order.CreatedEvent{OrderID: o.ID, CustomerID: o.CustomerID},
		EventName: order.EventCreated,
	}
	if err := h.Handle(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Stored(o.ID); got.Status != order.StatusConfirmed {
		t.Fatalf("status %s", got.Status)
	}
	// At-least-once delivery: the same message again is a no-op, never an error.
	msg.Attempt = 1
	if err := h.Handle(ctx, msg); err != nil {
		t.Fatalf("a redelivery must not fail: %v", err)
	}
}

// An order id this service never issued will not exist later either: the message must go
// straight to the dead-letter queue as non_retryable, not climb the retry ladder and fire the
// `exhausted` alert for something that is not an outage.
func TestHandleGivesUpOnAnUnknownOrder(t *testing.T) {
	svc, _, _ := ordertest.NewService(t, ordertest.Inventory{})
	h := consumers.NewOrderCreatedHandler(svc, quiet)
	err := h.Handle(context.Background(), messaging.Message[order.CreatedEvent]{Body: order.CreatedEvent{OrderID: 404}})
	if !errors.Is(err, messaging.ErrNonRetryable) {
		t.Fatalf("an unknown order is non-retryable: %v", err)
	}
	if apperror.StatusOf(err) != 404 {
		t.Fatalf("the cause is kept for the x-exception header: %v", err)
	}
}

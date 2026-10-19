package tasks_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/internal/order/ordertest"
	"example.com/msvcservice/internal/tasks"
	"github.com/guangliangus/go-msvc-kit/taskqueue"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestPendingOrdersReaperCancelsStalePendingOrders(t *testing.T) {
	svc, repo, _ := ordertest.NewService(ordertest.Inventory{UnitPrice: decimal.NewFromInt(1)})
	ctx := context.Background()
	ids := make([]int, 0, 2)
	for _, sku := range []string{"A", "B"} {
		o, err := svc.Create(ctx, "c1", []order.Line{{SKU: sku, Quantity: 1}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, o.ID)
	}
	confirmed, _ := svc.Create(ctx, "c1", []order.Line{{SKU: "C", Quantity: 1}})
	if err := svc.Confirm(ctx, confirmed.ID); err != nil {
		t.Fatal(err)
	}

	reaper := tasks.NewPendingOrdersReaper(svc, time.Minute, quiet)
	task := taskqueue.Task{QueueName: "pending-orders-reaper", Attempt: 1, DeliverOn: time.Now()}
	if res, err := reaper.Handle(ctx, task); err != nil || res != taskqueue.Done() {
		t.Fatalf("got %+v %v", res, err)
	}
	for _, id := range ids {
		if got, _ := repo.Stored(id); got.Status != order.StatusCancelled {
			t.Fatalf("order %d: %s", id, got.Status)
		}
	}
	if got, _ := repo.Stored(confirmed.ID); got.Status != order.StatusConfirmed {
		t.Fatal("only PENDING orders are reaped")
	}
	// The batch is idempotent: a retried run finds nothing left and is still done.
	if res, err := reaper.Handle(ctx, task); err != nil || res != taskqueue.Done() {
		t.Fatalf("second run: %+v %v", res, err)
	}
}

func TestHeartbeatIsDone(t *testing.T) {
	task := taskqueue.Task{QueueName: "heartbeat", Attempt: 1, DeliverOn: time.Now(), Misfired: true}
	if res, err := tasks.NewHeartbeat(quiet).Handle(context.Background(), task); err != nil || res != taskqueue.Done() {
		t.Fatalf("got %+v %v", res, err)
	}
}

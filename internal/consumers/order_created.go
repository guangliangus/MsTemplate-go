// Package consumers holds the service's event handlers. Each runs inside the inbox's process-once
// transaction (duplicates are skipped); returning an error sends the message down the retry
// ladder (30s, 2m, 10m) and then to the dead-letter queue.
package consumers

import (
	"context"
	"log/slog"

	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/pkg/messaging"
)

// OrderCreatedHandler confirms an order once its creation event comes back through the broker:
// the reference end-to-end loop (API → outbox → RabbitMQ → consumer → database), in-process.
type OrderCreatedHandler struct {
	orders *order.Service
	log    *slog.Logger
}

// NewOrderCreatedHandler builds the handler.
func NewOrderCreatedHandler(svc *order.Service, log *slog.Logger) *OrderCreatedHandler {
	return &OrderCreatedHandler{orders: svc, log: log}
}

// Handle confirms the order. The context carries the inbox transaction, so the status update and
// the inbox row commit together.
func (h *OrderCreatedHandler) Handle(ctx context.Context, msg messaging.Message[order.CreatedEvent]) error {
	h.log.InfoContext(ctx, "consuming order-created event",
		slog.Int("order_id", msg.Body.OrderID), slog.String("message_id", msg.MessageID.String()),
		slog.Int("attempt", msg.Attempt))
	return h.orders.Confirm(ctx, msg.Body.OrderID)
}

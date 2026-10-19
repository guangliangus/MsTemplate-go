package app

import (
	"testing"

	"github.com/guangliangus/go-msvc-kit/messaging"
)

// Each subscription goes to the bus its event lives on, and only there: a consumer on both buses
// would handle every event twice, a consumer on neither would never see it.
func TestSplitByBusHandsEachSubscriptionToOneBus(t *testing.T) {
	subs := []messaging.Subscription{
		{EventName: "orders.order-created.v1", Queue: "svc.orders.order-created.v1"},
		{EventName: "orders.order-cancelled.v1", Queue: "svc.orders.order-cancelled.v1"},
		{EventName: "payments.captured.v1", Queue: "svc.payments.captured.v1"},
	}
	onKafka := func(event string) bool { return event == "payments.captured.v1" }
	rabbit, kafka := splitByBus(subs, onKafka)
	if len(rabbit) != 2 || len(kafka) != 1 || kafka[0].EventName != "payments.captured.v1" {
		t.Fatalf("rabbit=%v kafka=%v", rabbit, kafka)
	}
	none := func(string) bool { return false }
	if rabbit, kafka := splitByBus(subs, none); len(rabbit) != 3 || len(kafka) != 0 {
		t.Fatalf("with Kafka off everything stays on RabbitMQ: rabbit=%d kafka=%d", len(rabbit), len(kafka))
	}
}

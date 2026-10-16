//go:build integration

package integration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"example.com/msvcservice/pkg/messaging"
	"example.com/msvcservice/pkg/rabbitmq"
	"example.com/msvcservice/pkg/testutil"
)

func brokerConfig(url string) rabbitmq.Config {
	return rabbitmq.Config{Enabled: true, URL: url, Exchange: "it.events", QueueType: "quorum", Prefetch: 10,
		ConsumerConcurrency: 2, RetryDelays: []time.Duration{time.Second, 2 * time.Second}, PublisherConfirms: true,
		PublishTimeout: 5 * time.Second, ReconnectDelay: time.Second, Heartbeat: 10 * time.Second}
}

type ping struct {
	N int `json:"n"`
}

func (ping) EventName() string { return "it.ping.v1" }

// Publish → broker → consumer host → inbox → handler, with the outbox dispatcher in the middle,
// plus a failing handler walking the retry ladder into the dead-letter queue.
func TestEndToEndThroughOutboxAndBroker(t *testing.T) {
	db := openDB(t)
	url := testutil.RabbitMQ(t)
	cfg := brokerConfig(url)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := rabbitmq.NewConnection(cfg, "it", quiet)
	go func() { _ = conn.Run(ctx) }()
	if err := conn.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}

	publisher := rabbitmq.NewPublisher(conn, cfg, quiet)
	dispatcher := messaging.NewOutboxDispatcher(db, publisher, messaging.OutboxConfig{PollInterval: 200 * time.Millisecond,
		BatchSize: 10, MaxAttempts: 3, Retention: time.Hour, CleanupInterval: time.Hour}, quiet)
	go func() { _ = dispatcher.Run(ctx) }()

	received := make(chan messaging.Message[ping], 4)
	var failures atomic.Int32
	inbox := messaging.NewInbox(db)
	subs := []messaging.Subscription{
		messaging.Subscribe("it.ping.v1", "it.ping", inbox, messaging.HandlerFunc[ping](func(_ context.Context, m messaging.Message[ping]) error {
			if m.Body.N < 0 {
				failures.Add(1)
				return errors.New("poison")
			}
			received <- m
			return nil
		})),
	}
	host := rabbitmq.NewConsumerHost(conn, cfg, publisher, quiet, subs...)
	go func() { _ = host.Run(ctx) }()
	time.Sleep(time.Second) // let the consumer declare its topology before publishing

	outbox := messaging.NewOutboxPublisher(db)
	if err := outbox.Publish(ctx, ping{N: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-received:
		if m.Body.N != 1 || m.EventName != "it.ping.v1" || m.Attempt != 0 || m.MessageID.String() == "" {
			t.Fatalf("%+v", m)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the event never arrived")
	}
	var status string
	if err := db.Pool.QueryRow(ctx, `SELECT status FROM outbox_messages ORDER BY id DESC LIMIT 1`).Scan(&status); err != nil || status != "PUBLISHED" {
		t.Fatalf("outbox row: %s %v", status, err)
	}

	// The poison message walks the ladder (1s, 2s) and lands in the DLQ with the failure marker.
	if err := outbox.Publish(ctx, ping{N: -1}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for failures.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if failures.Load() != 3 {
		t.Fatalf("expected the original plus two retries, got %d attempts", failures.Load())
	}
	ch, err := conn.Channel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	var dead int
	for time.Now().Before(deadline) {
		q, err := ch.QueueDeclarePassive(rabbitmq.DeadLetterQueue("it.ping"), true, false, false, false, map[string]any{"x-queue-type": "quorum"})
		if err != nil {
			t.Fatal(err)
		}
		if dead = q.Messages; dead == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if dead != 1 {
		t.Fatalf("expected one dead-lettered message, got %d", dead)
	}
}

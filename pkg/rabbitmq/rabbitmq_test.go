package rabbitmq

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"example.com/msvcservice/pkg/messaging"
)

func TestQueueNaming(t *testing.T) {
	if RetryQueue("svc.orders.order-created.v1", 2) != "svc.orders.order-created.v1.retry.2" {
		t.Fatal("retry queue name")
	}
	if DeadLetterQueue("q") != "q.dlq" {
		t.Fatal("dlq name")
	}
}

func TestConfigValidate(t *testing.T) {
	cfg := Config{Enabled: true, Exchange: "x", QueueType: "quorum"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("enabled without url must fail")
	}
	cfg.URL = "http://nope"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-amqp scheme must fail")
	}
	cfg.URL = "amqps://user:pass@broker:5671/"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.EffectiveRetryDelays()) != 3 || cfg.EffectiveRetryDelays()[0] != 30*time.Second {
		t.Fatal("default ladder")
	}
	cfg.Enabled = false
	cfg.URL = ""
	if err := cfg.Validate(); err != nil {
		t.Fatal("disabled needs no url")
	}
}

func TestHeaderHelpers(t *testing.T) {
	headers := amqp.Table{messaging.HeaderAttempt: int32(2), messaging.HeaderTraceParent: []byte("00-abc")}
	if attemptHeader(headers) != 2 || stringHeader(headers, messaging.HeaderTraceParent) != "00-abc" {
		t.Fatal("typed header reads")
	}
	if attemptHeader(amqp.Table{}) != 0 || stringHeader(nil, "x") != "" {
		t.Fatal("missing headers")
	}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestAPanickingHandlerBecomesAFailureNotACrash(t *testing.T) {
	h := &ConsumerHost{log: quiet}
	sub := messaging.Subscription{Queue: "q", Handle: func(context.Context, messaging.Delivery) (bool, error) {
		panic("nil map")
	}}
	dup, err := h.invoke(context.Background(), sub, messaging.Delivery{})
	if err == nil || dup {
		t.Fatalf("a panic is a failed handling: %v %v", dup, err)
	}
}

// With the broker away a publish ends within publish_timeout — waiting for the connection, and for
// the lock another publisher holds — instead of parking its caller indefinitely.
func TestPublishIsBoundedWhenTheBrokerIsAway(t *testing.T) {
	cfg := Config{Exchange: "x", PublishTimeout: 100 * time.Millisecond, PublisherConfirms: true}
	p := NewPublisher(NewConnection(cfg, "test", quiet), cfg, quiet)
	if p.Ready() {
		t.Fatal("a connection that never dialled is not ready")
	}
	start := time.Now()
	err := p.Publish(context.Background(), messaging.Envelope{EventName: "e"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("waiting for the connection: %v after %s", err, time.Since(start))
	}

	p.lock <- struct{}{} // another publish holds the channel
	start = time.Now()
	err = p.PublishToQueue(context.Background(), "q", amqp.Publishing{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("waiting for the lock: %v after %s", err, time.Since(start))
	}
}

func TestDrainCancelsHandlersOnlyAfterTheWindow(t *testing.T) {
	h := &ConsumerHost{log: quiet, cfg: Config{DrainTimeout: 50 * time.Millisecond}}
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	wg.Add(1)
	go func() { // a handler that only stops when cancelled
		defer wg.Done()
		<-ctx.Done()
	}()
	start := time.Now()
	h.drain(&wg, cancel, "q")
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("handlers were cancelled before the drain window: %s", waited)
	}

	quick := &sync.WaitGroup{}
	quick.Add(1)
	go quick.Done()
	cancelled := false
	h.drain(quick, func() { cancelled = true }, "q")
	if cancelled {
		t.Fatal("handlers that finish in time are never cancelled")
	}
}

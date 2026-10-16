package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"example.com/msvcservice/pkg/messaging"
)

// Publisher publishes to the topic exchange with publisher confirms, on one channel serialized
// behind a lock and recreated when found closed after a reconnect. It implements
// messaging.Transport and messaging.Readiness, which is how the outbox dispatcher reaches the broker.
//
// Every publish is bounded by publish_timeout as a whole — waiting for the lock, for a live
// connection and for the confirm — so a broker outage turns into a prompt error the caller can act
// on, never into a goroutine parked indefinitely while it holds something (a database transaction,
// a delivery, the lock itself).
type Publisher struct {
	conn     *Connection
	cfg      Config
	topology Topology
	log      *slog.Logger

	lock chan struct{} // a one-slot semaphore: unlike sync.Mutex, waiting for it honours ctx
	ch   *amqp.Channel
}

// NewPublisher builds the publisher.
func NewPublisher(conn *Connection, cfg Config, log *slog.Logger) *Publisher {
	return &Publisher{conn: conn, cfg: cfg, topology: NewTopology(cfg), log: log, lock: make(chan struct{}, 1)}
}

// Ready reports whether the broker connection is up; the outbox dispatcher checks it before
// claiming rows.
func (p *Publisher) Ready() bool {
	return p.conn.Connected()
}

// Publish sends an envelope to the exchange with the event name as routing key.
func (p *Publisher) Publish(ctx context.Context, env messaging.Envelope) error {
	ctx, span := messaging.Tracer().Start(ctx, env.EventName+" publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.destination.name", p.cfg.Exchange),
			attribute.String("messaging.message.id", env.MessageID.String())))
	defer span.End()

	headers := amqp.Table{
		messaging.HeaderEventName:  env.EventName,
		messaging.HeaderAttempt:    int32(env.Attempt), //nolint:gosec // small counter
		messaging.HeaderOccurredAt: env.OccurredAt.UTC().Format(time.RFC3339Nano),
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if tp := carrier.Get(messaging.HeaderTraceParent); tp != "" {
		headers[messaging.HeaderTraceParent] = tp
	} else if env.TraceParent != "" {
		headers[messaging.HeaderTraceParent] = env.TraceParent
	}

	msg := amqp.Publishing{
		DeliveryMode:  amqp.Persistent,
		ContentType:   messaging.ContentTypeJSON,
		MessageId:     env.MessageID.String(),
		CorrelationId: env.CorrelationID,
		Timestamp:     env.OccurredAt,
		Type:          env.EventName,
		Headers:       headers,
		Body:          env.Payload,
	}
	if err := p.publish(ctx, p.cfg.Exchange, env.EventName, msg); err != nil {
		return err
	}
	messaging.RecordPublished(ctx, env.EventName)
	return nil
}

// PublishToQueue sends directly to a queue through the default exchange (retry and dead-letter
// routing).
func (p *Publisher) PublishToQueue(ctx context.Context, queue string, msg amqp.Publishing) error {
	return p.publish(ctx, "", queue, msg)
}

func (p *Publisher) publish(ctx context.Context, exchange, key string, msg amqp.Publishing) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.PublishTimeout)
	defer cancel()
	select {
	case p.lock <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("rabbitmq: publish to %s/%s: waiting for the channel: %w", exchange, key, ctx.Err())
	}
	defer func() { <-p.lock }()

	ch, err := p.channel(ctx)
	if err != nil {
		return err
	}
	if !p.cfg.PublisherConfirms {
		if err := ch.PublishWithContext(ctx, exchange, key, false, false, msg); err != nil {
			p.resetChannel()
			return fmt.Errorf("rabbitmq: publish to %s/%s: %w", exchange, key, err)
		}
		return nil
	}

	confirmation, err := ch.PublishWithDeferredConfirmWithContext(ctx, exchange, key, false, false, msg)
	if err != nil {
		p.resetChannel()
		return fmt.Errorf("rabbitmq: publish to %s/%s: %w", exchange, key, err)
	}
	acked, err := confirmation.WaitContext(ctx)
	if err != nil {
		p.resetChannel()
		return fmt.Errorf("rabbitmq: confirm for %s/%s: %w", exchange, key, err)
	}
	if !acked {
		return fmt.Errorf("rabbitmq: broker nacked publish to %s/%s", exchange, key)
	}
	return nil
}

func (p *Publisher) channel(ctx context.Context) (*amqp.Channel, error) {
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	p.resetChannel()
	ch, err := p.conn.Channel(ctx)
	if err != nil {
		return nil, err
	}
	if p.cfg.PublisherConfirms {
		if err := ch.Confirm(false); err != nil {
			_ = ch.Close()
			return nil, fmt.Errorf("rabbitmq: enable confirms: %w", err)
		}
	}
	if err := p.topology.DeclareExchange(ch); err != nil {
		_ = ch.Close()
		return nil, err
	}
	p.ch = ch
	return ch, nil
}

func (p *Publisher) resetChannel() {
	if p.ch != nil {
		if err := p.ch.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
			p.log.Debug("rabbitmq publisher channel close failed", slog.Any("error", err))
		}
		p.ch = nil
	}
}

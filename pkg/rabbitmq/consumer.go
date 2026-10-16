package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"example.com/msvcservice/pkg/messaging"
)

const (
	routingTimeout     = 10 * time.Second
	routingRetryPause  = time.Second
	maxExceptionHeader = 500
)

var errChannelClosed = errors.New("rabbitmq: consumer channel closed")

// ConsumerHost runs every subscription: one channel per queue, manual acks, bounded concurrency,
// W3C trace extraction, delayed retry through the TTL queues and terminal dead-lettering. A lost
// channel or connection is re-established and consumption resumes. A handler that panics is
// recovered and treated as a failed handling, so one poison message cannot crash every replica in
// turn. On shutdown it cancels the consumers first (no new deliveries), lets in-flight handlers
// finish for up to drain_timeout, and only then cancels them; those have their messages requeued.
type ConsumerHost struct {
	conn      *Connection
	cfg       Config
	topology  Topology
	publisher *Publisher
	subs      []messaging.Subscription
	log       *slog.Logger
	inFlight  atomic.Int32
}

// NewConsumerHost builds the host for the subscriptions.
func NewConsumerHost(conn *Connection, cfg Config, publisher *Publisher, log *slog.Logger,
	subs ...messaging.Subscription) *ConsumerHost {
	return &ConsumerHost{conn: conn, cfg: cfg, topology: NewTopology(cfg), publisher: publisher, subs: subs, log: log}
}

// Run consumes until ctx is cancelled; it returns at once when there is nothing to consume.
func (h *ConsumerHost) Run(ctx context.Context) error {
	if len(h.subs) == 0 {
		return nil
	}
	var wg sync.WaitGroup
	for _, sub := range h.subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.runSubscription(ctx, sub)
		}()
	}
	wg.Wait()
	h.log.Info("consumer host stopped", slog.Int("in_flight", int(h.inFlight.Load())))
	return nil
}

func (h *ConsumerHost) runSubscription(ctx context.Context, sub messaging.Subscription) {
	for ctx.Err() == nil {
		err := h.consume(ctx, sub)
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("consumer stopped; re-establishing", slog.String("queue", sub.Queue),
			slog.Any("error", err), slog.Duration("after", h.cfg.ReconnectDelay))
		wait(ctx, h.cfg.ReconnectDelay)
	}
}

// consume serves one channel's lifetime: it returns when the channel closes or ctx ends.
func (h *ConsumerHost) consume(ctx context.Context, sub messaging.Subscription) error {
	ch, err := h.conn.Channel(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()

	if err := h.topology.DeclareConsumerQueues(ch, sub.Queue, sub.EventName); err != nil {
		return err
	}
	if err := ch.Qos(h.cfg.Prefetch, 0, false); err != nil {
		return fmt.Errorf("rabbitmq: qos: %w", err)
	}
	tag := sub.Queue + "." + uuid.NewString()
	deliveries, err := ch.Consume(sub.Queue, tag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("rabbitmq: consume %s: %w", sub.Queue, err)
	}
	closed := ch.NotifyClose(make(chan *amqp.Error, 1))
	h.log.Info("consuming", slog.String("queue", sub.Queue), slog.String("event", sub.EventName),
		slog.Int("prefetch", h.cfg.Prefetch), slog.Int("concurrency", h.cfg.ConsumerConcurrency))

	// Handlers run on their own context: shutdown stops the deliveries at once but cancels the
	// handlers only after the drain window, so work that is about to commit gets to commit.
	handlerCtx, cancelHandlers := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelHandlers()
	slots := make(chan struct{}, h.cfg.ConsumerConcurrency)
	var wg sync.WaitGroup
	acker := &channelAcker{ch: ch}
	for {
		select {
		case <-ctx.Done():
			if err := ch.Cancel(tag, false); err != nil {
				h.log.Debug("consumer cancel failed during shutdown", slog.Any("error", err))
			}
			h.drain(&wg, cancelHandlers, sub.Queue)
			return nil
		case amqpErr := <-closed:
			wg.Wait()
			if amqpErr != nil {
				return fmt.Errorf("%w: %s", errChannelClosed, amqpErr.Error())
			}
			return errChannelClosed
		case d, ok := <-deliveries:
			if !ok {
				wg.Wait()
				return errChannelClosed
			}
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				// Stopping while every slot is busy: hand this delivery straight back.
				acker.nack(d.DeliveryTag)
				continue
			}
			wg.Add(1)
			go func() {
				defer func() {
					<-slots
					wg.Done()
				}()
				h.handle(handlerCtx, sub, acker, d)
			}()
		}
	}
}

// drain waits for in-flight handlers up to drain_timeout, then cancels them and waits for them to
// requeue their messages.
func (h *ConsumerHost) drain(wg *sync.WaitGroup, cancelHandlers context.CancelFunc, queue string) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(h.cfg.DrainTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	h.log.Warn("consumer drain timed out; cancelling in-flight handlers, their messages are requeued",
		slog.String("queue", queue), slog.Duration("drain_timeout", h.cfg.DrainTimeout))
	cancelHandlers()
	<-done
}

func (h *ConsumerHost) handle(ctx context.Context, sub messaging.Subscription, acker *channelAcker, d amqp.Delivery) {
	h.inFlight.Add(1)
	defer h.inFlight.Add(-1)
	start := time.Now()
	outcome := messaging.OutcomeOK
	defer func() {
		messaging.RecordConsumed(context.WithoutCancel(ctx), sub.Queue, outcome, time.Since(start).Seconds())
	}()

	attempt := attemptHeader(d.Headers)
	if tp := stringHeader(d.Headers, messaging.HeaderTraceParent); tp != "" {
		ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{messaging.HeaderTraceParent: tp})
	}
	ctx, span := messaging.Tracer().Start(ctx, sub.EventName+" consume",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.destination.name", sub.Queue),
			attribute.String("messaging.message.id", d.MessageId),
			attribute.Int("messaging.rabbitmq.delivery_attempt", attempt)))
	defer span.End()

	messageID, err := uuid.Parse(d.MessageId)
	if err != nil {
		// Without an id the inbox cannot deduplicate; a fresh one keeps the message processable.
		messageID = uuid.New()
		h.log.WarnContext(ctx, "message has no usable message id; deduplication disabled for it",
			slog.String("queue", sub.Queue), slog.String("message_id", d.MessageId))
	}
	delivery := messaging.Delivery{
		Envelope: messaging.Envelope{
			MessageID:     messageID,
			EventName:     sub.EventName,
			Payload:       d.Body,
			OccurredAt:    d.Timestamp,
			CorrelationID: d.CorrelationId,
			Attempt:       attempt,
		},
		Queue: sub.Queue,
	}

	duplicate, handleErr := h.invoke(ctx, sub, delivery)
	switch {
	case handleErr == nil:
		if duplicate {
			outcome = messaging.OutcomeDuplicate
		}
		acker.ack(d.DeliveryTag)
	case ctx.Err() != nil:
		// Shutting down mid-handler: hand the message back for redelivery.
		outcome = messaging.OutcomeRequeued
		acker.nack(d.DeliveryTag)
	default:
		span.SetStatus(codes.Error, handleErr.Error())
		outcome = h.routeFailure(ctx, sub, acker, d, attempt, handleErr)
	}
}

// invoke runs the subscription's handler, turning a panic into an error so the message takes the
// failure path (retry ladder, then the DLQ) instead of taking the process down with it.
func (h *ConsumerHost) invoke(ctx context.Context, sub messaging.Subscription, d messaging.Delivery,
) (duplicate bool, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			h.log.ErrorContext(ctx, "message handler panicked",
				slog.String("queue", sub.Queue), slog.String("message_id", d.MessageID.String()),
				slog.Any("panic", rec), slog.String("stack", string(debug.Stack())))
			duplicate, err = false, fmt.Errorf("rabbitmq: handler panicked: %v", rec)
		}
	}()
	return sub.Handle(ctx, d)
}

// routeFailure copies the message to the next retry queue or to the dead-letter queue, then acks
// the original. Copy first, ack after: if the copy fails the original stays unacked and comes back.
func (h *ConsumerHost) routeFailure(ctx context.Context, sub messaging.Subscription, acker *channelAcker,
	d amqp.Delivery, attempt int, handleErr error) string {
	delays := h.cfg.EffectiveRetryDelays()
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}

	nonRetryable := errors.Is(handleErr, messaging.ErrNonRetryable)
	var target, outcome string
	if attempt < len(delays) && !nonRetryable {
		target = RetryQueue(sub.Queue, attempt+1)
		headers[messaging.HeaderAttempt] = int32(attempt + 1) //nolint:gosec // small counter
		outcome = messaging.OutcomeRetry
		h.log.WarnContext(ctx, "message handling failed; scheduling retry",
			slog.String("queue", sub.Queue), slog.String("message_id", d.MessageId),
			slog.Int("attempt", attempt+1), slog.String("retry_queue", target),
			slog.Duration("delay", delays[attempt]), slog.Any("error", handleErr))
	} else {
		target = DeadLetterQueue(sub.Queue)
		headers[messaging.HeaderException] = truncate(handleErr.Error(), maxExceptionHeader)
		outcome = messaging.OutcomeDeadLetter
		reason := messaging.DeadLetterExhausted
		if nonRetryable {
			reason = messaging.DeadLetterNonRetryable
		}
		messaging.RecordDeadLettered(ctx, sub.Queue, reason)
		h.log.ErrorContext(ctx, "message dead-lettered",
			slog.String("queue", sub.Queue), slog.String("message_id", d.MessageId), slog.String("reason", reason),
			slog.Int("attempts", attempt), slog.String("dlq", target), slog.Any("error", handleErr))
	}

	routeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), routingTimeout)
	defer cancel()
	copyMsg := amqp.Publishing{
		DeliveryMode:  amqp.Persistent,
		ContentType:   d.ContentType,
		MessageId:     d.MessageId,
		CorrelationId: d.CorrelationId,
		Timestamp:     d.Timestamp,
		Type:          d.Type,
		Headers:       headers,
		Body:          d.Body,
	}
	if err := h.publisher.PublishToQueue(routeCtx, target, copyMsg); err != nil {
		// Never leave the delivery unacked forever: requeue and let redelivery retry the routing.
		// The pause prevents a hot loop while the publisher is unhealthy.
		h.log.ErrorContext(ctx, "routing the failed message failed; requeueing",
			slog.String("queue", sub.Queue), slog.String("message_id", d.MessageId), slog.Any("error", err))
		wait(routeCtx, routingRetryPause)
		acker.nack(d.DeliveryTag)
		return messaging.OutcomeRequeued
	}
	acker.ack(d.DeliveryTag)
	return outcome
}

// channelAcker serializes acks on one channel across the handler goroutines.
type channelAcker struct {
	mu sync.Mutex
	ch *amqp.Channel
}

func (a *channelAcker) ack(tag uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.ch.Ack(tag, false)
}

func (a *channelAcker) nack(tag uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.ch.Nack(tag, false, true)
}

func attemptHeader(headers amqp.Table) int {
	switch v := headers[messaging.HeaderAttempt].(type) {
	case int32:
		return int(v)
	case int64:
		return int(v)
	case int:
		return v
	case int16:
		return int(v)
	case int8:
		return int(v)
	default:
		return 0
	}
}

func stringHeader(headers amqp.Table, name string) string {
	switch v := headers[name].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

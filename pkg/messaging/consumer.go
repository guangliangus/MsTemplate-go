package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Message is a consumed event with the delivery metadata a handler may want.
type Message[T any] struct {
	Body          T
	MessageID     uuid.UUID
	EventName     string
	CorrelationID string
	// Attempt is 0 on first delivery and grows with every pass through the retry ladder.
	Attempt int
	Queue   string
}

// Handler is the business handler for one event type. Returning an error sends the message down
// the delayed-retry ladder and finally to the dead-letter queue; honour ctx so a graceful stop can
// drain quickly (a handler cancelled by shutdown has its message requeued).
type Handler[T any] interface {
	Handle(ctx context.Context, msg Message[T]) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc[T any] func(ctx context.Context, msg Message[T]) error

// Handle calls f.
func (f HandlerFunc[T]) Handle(ctx context.Context, msg Message[T]) error { return f(ctx, msg) }

// Delivery is a raw message as the broker host hands it to a subscription.
type Delivery struct {
	Envelope
	Queue string
}

// Subscription binds a queue to a handler. Handle reports whether the delivery was a duplicate
// the inbox skipped.
type Subscription struct {
	EventName string
	Queue     string
	Handle    func(ctx context.Context, d Delivery) (duplicate bool, err error)
}

// Subscribe builds a typed subscription: the payload is decoded into T and the handler runs
// inside the inbox's process-once transaction (inbox may be nil for a handler with no database
// side effects, which then runs at-least-once with no deduplication).
func Subscribe[T any](eventName, queue string, inbox *Inbox, h Handler[T]) Subscription {
	return Subscription{
		EventName: eventName,
		Queue:     queue,
		Handle: func(ctx context.Context, d Delivery) (bool, error) {
			var body T
			if err := json.Unmarshal(d.Payload, &body); err != nil {
				// A body that does not decode today will not decode in ten minutes: straight to the
				// DLQ as non_retryable, not down the ladder into the "exhausted" alert.
				return false, NonRetryable(fmt.Errorf("messaging: decode %s: %w", d.EventName, err))
			}
			msg := Message[T]{
				Body:          body,
				MessageID:     d.MessageID,
				EventName:     d.EventName,
				CorrelationID: d.CorrelationID,
				Attempt:       d.Attempt,
				Queue:         d.Queue,
			}
			if inbox == nil {
				return false, h.Handle(ctx, msg)
			}
			processed, err := inbox.ProcessOnce(ctx, d.MessageID, queue, func(ctx context.Context) error {
				return h.Handle(ctx, msg)
			})
			return !processed, err
		},
	}
}

// ErrNonRetryable marks a failure that retrying cannot fix: a payload the handler cannot parse, a
// business rule that will refuse the message forever. A handler wraps such an error with
// NonRetryable and the host sends the message straight to the dead-letter queue instead of down
// the retry ladder, counted under reason="non_retryable", so that an alert on exhausted retries
// (the broker or a dependency being down) is not drowned by routine rejections.
var ErrNonRetryable = errors.New("messaging: non-retryable failure")

type nonRetryableError struct{ err error }

func (e *nonRetryableError) Error() string { return e.err.Error() }

// Unwrap exposes both the cause and the marker to errors.Is / errors.As.
func (e *nonRetryableError) Unwrap() []error { return []error{e.err, ErrNonRetryable} }

// NonRetryable marks err as not worth retrying; nil stays nil.
func NonRetryable(err error) error {
	if err == nil {
		return nil
	}
	return &nonRetryableError{err: err}
}

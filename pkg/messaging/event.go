// Package messaging is the broker-agnostic half of integration events: the Event contract, the
// envelope that travels on the wire, the transactional outbox that makes "write the row and
// publish the event" one commit, the inbox that makes consuming exactly-once-in-effect, and the
// typed subscription a handler plugs into. pkg/rabbitmq is the broker half.
//
// Wire format shared by every service on the platform's broker: the body is the event as camelCase JSON (with messageId
// and occurredAt added), the AMQP properties carry message id, correlation id and content type,
// and the headers carry x-event-name, x-attempt, x-occurred-at and the W3C traceparent.
package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Event is an integration event: a fact about this service's domain other services may act on.
// EventName is the routing key, explicit and versioned by convention ("orders.order-created.v1");
// renaming the Go type must never change it, which is why it is not derived from the type.
type Event interface {
	EventName() string
}

// Header and property names on the wire.
const (
	HeaderEventName   = "x-event-name"
	HeaderAttempt     = "x-attempt"
	HeaderTraceParent = "traceparent"
	HeaderException   = "x-exception"
	HeaderOccurredAt  = "x-occurred-at"
	ContentTypeJSON   = "application/json"
)

// Envelope is one message as it travels: the serialized event plus the metadata every hop needs.
type Envelope struct {
	MessageID     uuid.UUID
	EventName     string
	Payload       []byte
	OccurredAt    time.Time
	CorrelationID string
	TraceParent   string
	Attempt       int
}

// Transport delivers envelopes to the broker; pkg/rabbitmq implements it.
type Transport interface {
	Publish(ctx context.Context, env Envelope) error
}

// Publisher is what application code calls. The outbox implementation only writes a row on the
// caller's transaction; the dispatcher publishes it later.
type Publisher interface {
	Publish(ctx context.Context, event Event) error
}

const maxEventNameLength = 255

// ValidateEventName refuses names that cannot be a routing key.
func ValidateEventName(name string) error {
	switch {
	case name == "":
		return errors.New("messaging: event name is empty")
	case len(name) > maxEventNameLength:
		return fmt.Errorf("messaging: event name %q exceeds %d bytes", name, maxEventNameLength)
	case strings.ContainsAny(name, " \t\r\n#*"):
		return fmt.Errorf("messaging: event name %q contains whitespace or a topic wildcard", name)
	default:
		return nil
	}
}

// NewEnvelope serializes the event and captures the current trace context so an asynchronously
// dispatched event is still findable from the request that produced it.
func NewEnvelope(ctx context.Context, event Event) (Envelope, error) {
	name := event.EventName()
	if err := ValidateEventName(name); err != nil {
		return Envelope{}, err
	}
	id := uuid.New()
	occurred := time.Now().UTC()
	payload, err := encodePayload(event, id, occurred)
	if err != nil {
		return Envelope{}, fmt.Errorf("messaging: encode %s: %w", name, err)
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	correlation := ""
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		correlation = sc.TraceID().String()
	}
	if correlation == "" {
		correlation = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	return Envelope{
		MessageID:     id,
		EventName:     name,
		Payload:       payload,
		OccurredAt:    occurred,
		CorrelationID: correlation,
		TraceParent:   carrier.Get(HeaderTraceParent),
	}, nil
}

// encodePayload marshals the event and splices messageId and occurredAt into the object, the two
// members every event on the platform carries, so a consumer in any language reads the
// same body.
func encodePayload(event Event, id uuid.UUID, occurred time.Time) ([]byte, error) {
	body, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	body = bytes.TrimSpace(body)
	if len(body) < 2 || body[0] != '{' {
		return nil, errors.New("event must serialize to a JSON object")
	}
	meta, err := json.Marshal(struct {
		MessageID  uuid.UUID `json:"messageId"`
		OccurredAt time.Time `json:"occurredAt"`
	}{id, occurred})
	if err != nil {
		return nil, err
	}
	if string(body) == "{}" {
		return meta, nil
	}
	// meta is "{...}", body is "{...}": join as "{meta-members,body-members}".
	joined := make([]byte, 0, len(meta)+len(body))
	joined = append(joined, meta[:len(meta)-1]...)
	joined = append(joined, ',')
	joined = append(joined, body[1:]...)
	return joined, nil
}

package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type created struct {
	OrderID int    `json:"orderId"`
	Note    string `json:"note"`
}

func (created) EventName() string { return "orders.order-created.v1" }

type empty struct{}

func (empty) EventName() string { return "orders.ping.v1" }

func TestNewEnvelopeSplicesMetadataIntoTheBody(t *testing.T) {
	env, err := NewEnvelope(context.Background(), created{OrderID: 7, Note: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(env.Payload, &body); err != nil {
		t.Fatalf("payload is not JSON: %s", env.Payload)
	}
	if body["messageId"] != env.MessageID.String() || body["orderId"].(float64) != 7 || body["note"] != "x" {
		t.Fatalf("body %v", body)
	}
	if _, ok := body["occurredAt"]; !ok {
		t.Fatal("occurredAt missing")
	}
	if env.CorrelationID == "" || env.EventName != "orders.order-created.v1" {
		t.Fatalf("envelope %+v", env)
	}

	emptyEnv, err := NewEnvelope(context.Background(), empty{})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(emptyEnv.Payload, &body); err != nil || body["messageId"] == nil {
		t.Fatalf("an empty event still carries metadata: %s", emptyEnv.Payload)
	}
}

func TestNewEnvelopeCapturesTheTraceContext(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	env, err := NewEnvelope(ctx, created{})
	if err != nil {
		t.Fatal(err)
	}
	if env.TraceParent != "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01" {
		t.Fatalf("traceparent %q", env.TraceParent)
	}
	if env.CorrelationID != traceID.String() {
		t.Fatalf("correlation id is the trace id: %q", env.CorrelationID)
	}
}

type badName struct{}

func (badName) EventName() string { return "has space" }

func TestValidateEventName(t *testing.T) {
	for _, bad := range []string{"", "a b", "orders.#", "orders.*"} {
		if err := ValidateEventName(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if err := ValidateEventName("orders.order-created.v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEnvelope(context.Background(), badName{}); err == nil {
		t.Fatal("a bad name must fail at publish time")
	}
}

// A body that does not decode now never will: straight to the DLQ as non_retryable, not down the
// ladder into the "exhausted" alert that means a dependency stayed down.
func TestUndecodableBodiesAreNonRetryable(t *testing.T) {
	called := false
	sub := Subscribe("orders.order-created.v1", "q", nil, HandlerFunc[created](func(context.Context, Message[created]) error {
		called = true
		return nil
	}))
	_, err := sub.Handle(context.Background(), Delivery{Envelope: Envelope{Payload: []byte(`{"orderId":"seven"}`)}})
	if !errors.Is(err, ErrNonRetryable) || called {
		t.Fatalf("got %v (handler called: %v)", err, called)
	}
}

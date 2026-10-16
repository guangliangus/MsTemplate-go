package taskqueue

import (
	"bytes"
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Trace context travels inside the payload under the W3C keys (traceparent, tracestate): the
// task_queues table is shared with the platform's other services and gains no column for it, and a
// payload that is a JSON object carries two more members without any handler noticing. A payload
// that is not an object (an array, a scalar) travels untouched, and a member the producer already
// set is never overwritten.
var traceKeys = []string{"traceparent", "tracestate"}

func injectTrace(ctx context.Context, payload []byte) []byte {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return payload
	}
	fields, ok := objectFields(payload)
	if !ok {
		return payload
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	changed := false
	for _, key := range traceKeys {
		v := carrier.Get(key)
		if _, exists := fields[key]; exists || v == "" {
			continue
		}
		fields[key], _ = json.Marshal(v)
		changed = true
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return payload
	}
	return out
}

// extractTrace returns ctx continued from the producer's span when the payload carries one, so a
// task's span hangs under the request that enqueued it, like a consumed message under its publish.
func extractTrace(ctx context.Context, payload []byte) context.Context {
	fields, ok := objectFields(payload)
	if !ok {
		return ctx
	}
	carrier := propagation.MapCarrier{}
	for _, key := range traceKeys {
		var s string
		if raw, exists := fields[key]; exists && json.Unmarshal(raw, &s) == nil && s != "" {
			carrier.Set(key, s)
		}
	}
	if len(carrier) == 0 {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

func objectFields(payload []byte) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, false
	}
	return fields, true
}

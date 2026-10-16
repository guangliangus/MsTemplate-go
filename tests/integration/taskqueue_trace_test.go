//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"example.com/msvcservice/pkg/taskqueue"
)

// A task enqueued inside a trace carries the producer's traceparent in its payload, and the queue
// stats report both the claimable depth and how late its oldest task is.
func TestTaskQueueCarriesTraceContextAndReportsStats(t *testing.T) {
	db := openDB(t)
	store := taskqueue.NewStore(db)
	ctx := context.Background()

	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previous) })
	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	traced := trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	if _, err := store.Enqueue(traced, taskqueue.EnqueueRequest{QueueName: "traced", TaskID: "a", Payload: map[string]int{"n": 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(traced, taskqueue.EnqueueRequest{QueueName: "traced", TaskID: "b", Payload: []int{1, 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueBatch(traced, []taskqueue.EnqueueRequest{{QueueName: "traced", TaskID: "c"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := stats["traced"]; s.Depth != 3 || s.OldestAge <= 0 {
		t.Fatalf("stats before the claim: %+v", s)
	}

	popped, err := store.Pop(ctx, "traced", 10, "runner")
	if err != nil || len(popped) != 3 {
		t.Fatalf("pop: %v %d", err, len(popped))
	}
	byID := map[string]string{}
	for _, task := range popped {
		byID[task.TaskID] = string(task.Payload)
	}
	want := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	if !strings.Contains(byID["a"], `"traceparent": "`+want+`"`) || !strings.Contains(byID["a"], `"n": 1`) {
		t.Fatalf("object payload carries the producer's trace beside its own members: %s", byID["a"])
	}
	if byID["b"] != "[1, 2]" {
		t.Fatalf("a non-object payload travels untouched: %s", byID["b"])
	}
	if !strings.Contains(byID["c"], want) {
		t.Fatalf("the batch path injects too: %s", byID["c"])
	}
	if after, _ := store.Stats(ctx); after["traced"].Depth != 0 {
		t.Fatalf("claimed rows are not claimable: %+v", after["traced"])
	}
}

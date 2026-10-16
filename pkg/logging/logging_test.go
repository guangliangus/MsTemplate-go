package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestSensitiveAttributesAreMasked(t *testing.T) {
	var buf bytes.Buffer
	log := New(Config{Format: "json"}, &buf)
	log.Info("login", slog.String("password", "hunter2"), slog.String("user", "alice"),
		slog.Group("auth", slog.String("Token", "abc"), slog.Int("attempt", 1)))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["password"] != "***" || line["user"] != "alice" {
		t.Fatalf("top-level masking wrong: %v", line)
	}
	group := line["auth"].(map[string]any)
	if group["Token"] != "***" || group["attempt"].(float64) != 1 {
		t.Fatalf("group masking wrong: %v", group)
	}
}

func TestTraceIDsAreStamped(t *testing.T) {
	var buf bytes.Buffer
	log := New(Config{Format: "json"}, &buf)
	traceID, _ := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	spanID, _ := trace.SpanIDFromHex("b7ad6b7169203331")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	log.InfoContext(ctx, "hello")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["trace_id"] != traceID.String() || line["span_id"] != spanID.String() {
		t.Fatalf("trace correlation missing: %v", line)
	}
}

func TestParseLevel(t *testing.T) {
	if ParseLevel("DEBUG") != slog.LevelDebug || ParseLevel("warning") != slog.LevelWarn || ParseLevel("nope") != slog.LevelInfo {
		t.Fatal("level parsing")
	}
}

package logging

import (
	"context"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler stamps trace_id and span_id from the context's span onto each record, as the bare
// hex ids: the same values the X-Trace-ID header and the traceId problem-details member carry, so
// one value off a support ticket greps the logs and opens the trace.
type traceHandler struct {
	slog.Handler
}

// NewTraceHandler wraps h with trace correlation.
func NewTraceHandler(h slog.Handler) slog.Handler {
	return traceHandler{Handler: h}
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}

// maskingHandler replaces the values of sensitive attribute keys with "***", at any group depth.
type maskingHandler struct {
	slog.Handler
	keys map[string]struct{}
}

// NewMaskingHandler wraps h so that attributes named in keys (case-insensitive) are never written.
func NewMaskingHandler(h slog.Handler, keys []string) slog.Handler {
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		set[strings.ToLower(k)] = struct{}{}
	}
	return maskingHandler{Handler: h, keys: set}
}

func (h maskingHandler) Handle(ctx context.Context, r slog.Record) error {
	masked := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		masked.AddAttrs(h.mask(a))
		return true
	})
	return h.Handler.Handle(ctx, masked)
}

func (h maskingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = h.mask(a)
	}
	return maskingHandler{Handler: h.Handler.WithAttrs(out), keys: h.keys}
}

func (h maskingHandler) WithGroup(name string) slog.Handler {
	return maskingHandler{Handler: h.Handler.WithGroup(name), keys: h.keys}
}

func (h maskingHandler) mask(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		out := make([]any, 0, len(group))
		for _, g := range group {
			out = append(out, h.mask(g))
		}
		return slog.Group(a.Key, out...)
	}
	if _, sensitive := h.keys[strings.ToLower(a.Key)]; sensitive {
		return slog.String(a.Key, "***")
	}
	return a
}

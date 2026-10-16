package middleware

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"example.com/msvcservice/pkg/httpx"
)

// TraceHeaderName carries the bare 32-hex trace id back to the client on every traced response:
// the same value as the traceId in an error body and the trace_id in the logs, so one value off a
// support ticket opens all three.
const TraceHeaderName = "X-Trace-ID"

// TraceHeader sets X-Trace-ID before the handler writes. It only sets a header, so it can neither
// swallow an error nor change a status.
func TraceHeader() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id := httpx.TraceID(r.Context()); id != "" {
				w.Header().Set(TraceHeaderName, id)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RouteSpan names the server span after the matched route pattern ("GET /api/v1/orders/{id}")
// and tags the request-duration metric with http.route. It must sit inside the otelhttp handler
// and runs its work after the handler returns, which is when chi knows the pattern.
func RouteSpan() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			rc := chi.RouteContext(r.Context())
			if rc == nil {
				return
			}
			pattern := rc.RoutePattern()
			if pattern == "" {
				return
			}
			span := trace.SpanFromContext(r.Context())
			span.SetName(r.Method + " " + pattern)
			span.SetAttributes(semconv.HTTPRoute(pattern))
			if labeler, ok := otelhttp.LabelerFromContext(r.Context()); ok {
				labeler.Add(semconv.HTTPRoute(pattern))
			}
		})
	}
}

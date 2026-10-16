package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"example.com/msvcservice/pkg/actor"
	"example.com/msvcservice/pkg/httpx"
	"example.com/msvcservice/pkg/reqmeta"
)

// RequestLogger writes one line per request with the status the client actually received. It
// belongs OUTSIDE the error-to-problem conversion and the recoverer: inside them it would see the
// in-flight failure and record a validation 400 as a 500, and an SLO dashboard built on the log
// would read our own 4xx as our own outage. Paths with a listed prefix (probes, metrics) are not
// logged.
//
// Beside the status the line carries the errorCode of the problem the client received, when there
// was one, and the same pair feeds msvc_http_problems_total, so an alert can single out one code
// (UPSTREAM_SERVICE_UNAVAILABLE) where the status alone (502) could not. The inbound X-Request-ID
// is logged as request_id: it is the client's handle, the trace id is ours, and support needs both.
func RequestLogger(log *slog.Logger, skipPrefixes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, p := range skipPrefixes {
				if strings.HasPrefix(r.URL.Path, p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			start := time.Now()
			// The actor and the problem are established further down the chain; the recorders let
			// this outer frame read them once the request is done.
			ctx := httpx.WithProblemRecorder(actor.WithRecorder(r.Context()))
			r = r.WithContext(ctx)
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			route := ""
			if rc := chi.RouteContext(ctx); rc != nil {
				route = rc.RoutePattern()
			}
			level := slog.LevelInfo
			if ww.Status() >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("route", route),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.String("client_ip", ClientIP(r)),
				slog.String("actor", actor.FromContext(ctx)),
			}
			if id := reqmeta.Clean(r.Header.Get(reqmeta.HeaderRequestID)); id != "" {
				attrs = append(attrs, slog.String("request_id", id))
			}
			if code := httpx.RecordedProblem(ctx); code != "" {
				attrs = append(attrs, slog.String("error_code", code))
				recordProblem(ctx, route, ww.Status(), code)
			}
			log.LogAttrs(ctx, level, "http request", attrs...)
		})
	}
}

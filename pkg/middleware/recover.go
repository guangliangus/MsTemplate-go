package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/httpx"
)

// Recoverer turns a panic into a 500 problem and a log line with the stack, so one bad request
// never takes the listener down. http.ErrAbortHandler is re-raised, as net/http expects.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(rec)
				}
				log.ErrorContext(r.Context(), "panic while handling request",
					slog.Any("panic", rec),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())))
				httpx.ErrorWriterFrom(r.Context()).WriteError(w, r, apperror.Internal(fmt.Errorf("panic: %v", rec)))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/cors"
)

// RequestTimeout puts a deadline on every request's context. Handlers that propagate the context
// (database, Redis, downstream calls) fail with context.DeadlineExceeded, which the error writer
// answers with 504 REQUEST_TIMEOUT. It complements the downstream client timeouts: together they
// keep a slow request from holding resources forever.
func RequestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// MaxBodyBytes caps request bodies; reads past the limit fail with *http.MaxBytesError (413).
func MaxBodyBytes(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && n > 0 {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORSConfig is bound from "http.cors".
type CORSConfig struct {
	// Origins allowed to call this API from a browser; empty disables CORS entirely.
	Origins []string `koanf:"origins"`
	// AllowedMethods defaults to the usual verbs.
	AllowedMethods []string `koanf:"allowed_methods"`
	// AllowedHeaders defaults to Authorization, Content-Type and the platform's custom headers.
	AllowedHeaders []string `koanf:"allowed_headers"`
	// MaxAge is the preflight cache lifetime in seconds.
	MaxAge int `koanf:"max_age" validate:"min=0"`
}

// CORS applies the configured policy; with no origins it is a no-op.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	if len(cfg.Origins) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	methods := cfg.AllowedMethods
	if len(methods) == 0 {
		methods = []string{
			http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
		}
	}
	headers := cfg.AllowedHeaders
	if len(headers) == 0 {
		headers = []string{"Authorization", "Content-Type", "Idempotency-Key", "X-Request-ID"}
	}
	return cors.Handler(cors.Options{
		AllowedOrigins:   cfg.Origins,
		AllowedMethods:   methods,
		AllowedHeaders:   headers,
		ExposedHeaders:   []string{TraceHeaderName, "Location", "Idempotency-Replayed", "Retry-After"},
		AllowCredentials: true,
		MaxAge:           cfg.MaxAge,
	})
}

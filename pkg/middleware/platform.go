package middleware

import (
	"net/http"
	"strings"

	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/httpx"
	"example.com/msvcservice/pkg/reqmeta"
)

// PlatformHeaders captures the platform's request headers (X-Request-ID, X-Language, ...) into the
// context, so the access log and every outbound call can carry them. It sits early in the chain,
// before anything that calls a downstream.
func PlatformHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(reqmeta.Capture(r.Context(), r.Header)))
		})
	}
}

// RequireHeaders refuses requests missing any of the named headers with 400 MISSING_HEADERS, the
// missing names listed under errors. The platform's consumer apps always send X-Platform,
// X-Device-ID, X-Request-ID and X-Language; a route that depends on them says so here instead of
// failing later with a less useful error. With no names it is a no-op.
func RequireHeaders(names ...string) func(http.Handler) http.Handler {
	if len(names) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var missing []string
			for _, name := range names {
				if strings.TrimSpace(r.Header.Get(name)) == "" {
					missing = append(missing, name)
				}
			}
			if len(missing) > 0 {
				httpx.Error(w, r, apperror.MissingHeaders(missing))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

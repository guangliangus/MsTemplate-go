// Package middleware is the host's HTTP middleware set. The order it is applied in is load-bearing;
// see internal/httpapi/router.go for the one the template ships.
package middleware

import (
	"net/http"

	"example.com/msvcservice/pkg/httpx"
)

// ErrorWriter attaches the host's problem-details writer to every request context, so handlers
// and later middlewares all write errors the same way.
func ErrorWriter(ew *httpx.ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(httpx.WithErrorWriter(r.Context(), ew)))
		})
	}
}

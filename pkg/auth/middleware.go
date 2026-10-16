package auth

import (
	"log/slog"
	"net/http"
	"strings"

	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/httpx"
)

// Authenticate establishes the principal: a bearer token is validated (an invalid one is a 401
// right here, so a bad token never reaches a handler as "anonymous"); with no token, the
// development headers may fabricate one; otherwise the request continues anonymous and the
// Require* middlewares decide. dev may be nil.
func Authenticate(v *Validator, dev *DevHeaders, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, present := bearer(r)
			if present {
				p, err := v.Parse(raw)
				if err != nil {
					log.InfoContext(r.Context(), "token refused", slog.Any("error", err))
					httpx.Error(w, r, apperror.Unauthorized("The access token is missing, invalid or expired."))
					return
				}
				next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
				return
			}
			if dev.Active() {
				if p := dev.Principal(r); p != nil {
					next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", true // present but unusable: refuse, do not fall through to anonymous
	}
	return strings.TrimSpace(token), true
}

// Require refuses anonymous requests with a 401.
func Require() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := FromContext(r.Context()); !ok {
				httpx.Error(w, r, apperror.Unauthorized("Authentication is required."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRealm passes only a principal of the named plane: 401 when anonymous, 403 for a valid
// token of another plane or of no plane at all (a missing realm is what a malformed or foreign
// token looks like, and a decision based on absence would be fail-open).
func RequireRealm(realm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := FromContext(r.Context())
			if !ok {
				httpx.Error(w, r, apperror.Unauthorized("Authentication is required."))
				return
			}
			if p.Realm != realm {
				httpx.Error(w, r, apperror.Forbidden("This endpoint serves the "+realm+" plane."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireScope passes only a token carrying the scope (403 otherwise, 401 when anonymous).
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := FromContext(r.Context())
			if !ok {
				httpx.Error(w, r, apperror.Unauthorized("Authentication is required."))
				return
			}
			if !p.HasScope(scope) {
				httpx.Error(w, r, apperror.Forbidden("This endpoint requires the '"+scope+"' scope."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

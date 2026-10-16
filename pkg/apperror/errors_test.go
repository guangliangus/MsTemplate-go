package apperror

import (
	"errors"
	"net/http"
	"testing"
)

func TestTypedErrorsCarryStatusAndCode(t *testing.T) {
	cases := []struct {
		name   string
		err    *Error
		status int
		code   string
	}{
		{"not found", NotFound("Order", 42), http.StatusNotFound, CodeNotFound},
		{"domain rule", DomainRule("ORDER_X", "nope"), http.StatusUnprocessableEntity, "ORDER_X"},
		{"conflict", Conflict("ORDER_Y", "nope"), http.StatusConflict, "ORDER_Y"},
		{"concurrency", ConcurrencyConflict(), http.StatusConflict, CodeConcurrencyConflict},
		{"forbidden", Forbidden("no"), http.StatusForbidden, CodeForbidden},
		{"unauthorized", Unauthorized("who"), http.StatusUnauthorized, CodeUnauthorized},
		{"rate limited", RateLimited("slow", 0), http.StatusTooManyRequests, CodeRateLimitExceeded},
		{"timeout", Timeout("late"), http.StatusGatewayTimeout, CodeRequestTimeout},
		{"upstream", Upstream("inventory", "down", nil), http.StatusBadGateway, CodeUpstreamUnavailable},
		{"not configured", NotConfigured("service_clients.x"), http.StatusInternalServerError, CodeNotConfigured},
		{"validation", Validation(nil), http.StatusBadRequest, CodeValidationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Status != tc.status || tc.err.Code != tc.code {
				t.Fatalf("got %d %s, want %d %s", tc.err.Status, tc.err.Code, tc.status, tc.code)
			}
		})
	}
}

func TestAsAndStatusOfThroughWrapping(t *testing.T) {
	cause := errors.New("boom")
	wrapped := errors.Join(errors.New("context"), NotFound("Order", 1).WithCause(cause))

	e, ok := As(wrapped)
	if !ok || e.Code != CodeNotFound {
		t.Fatalf("As did not find the typed error: %v", wrapped)
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("the cause must stay reachable through Unwrap")
	}
	if StatusOf(errors.New("plain")) != http.StatusInternalServerError {
		t.Fatal("untyped errors are 500")
	}
	if StatusOf(wrapped) != http.StatusNotFound {
		t.Fatal("typed errors keep their status through wrapping")
	}
}

func TestNotFoundMessageNamesTheResource(t *testing.T) {
	if got := NotFound("Order", 42).Message; got != "Order '42' was not found." {
		t.Fatalf("unexpected message %q", got)
	}
}

func TestCodeForStatus(t *testing.T) {
	if CodeForStatus(404) != CodeNotFound || CodeForStatus(405) != CodeMethodNotAllowed {
		t.Fatal("router statuses map to their codes")
	}
	if CodeForStatus(418) != CodeInternalError {
		t.Fatal("unknown statuses fall back to INTERNAL_ERROR")
	}
}

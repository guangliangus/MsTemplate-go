// Package apperror is the platform's expected-error vocabulary: typed errors the HTTP layer maps to
// RFC 9457 problem details, and the machine-readable codes every service on the platform shares.
//
// An *Error is an error a handler expects and the caller can act on; its Message is safe to show
// to callers. Anything else that reaches the HTTP layer is a 500 with a generic detail and a log
// line keyed by the trace id. Status codes group by what the client should do: 400 fix and resend,
// 401 re-authenticate, 403 stop trying, 404 it does not exist, 409 state conflict, 422 a business
// rule refused it, 429 slow down, 502 the upstream failed, 504 we timed out.
package apperror

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// StatusClientClosedRequest is the nginx-style status for a request whose client went away.
const StatusClientClosedRequest = 499

// Error is an expected application error with a stable code and an HTTP status.
type Error struct {
	// Code is the UPPER_SNAKE_CASE error code (see codes.go).
	Code string
	// Status is the HTTP status the API layer answers with.
	Status int
	// Message is the detail sentence; it must be safe to show to callers.
	Message string
	// RetryAfter, when positive, becomes the Retry-After header (429 and 503).
	RetryAfter time.Duration
	// Fields carries per-field validation messages for CodeValidationFailed.
	Fields map[string][]string

	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return e.Code + ": " + e.Message + ": " + e.cause.Error()
	}
	return e.Code + ": " + e.Message
}

// Unwrap exposes the cause to errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.cause }

// WithCause attaches the underlying error (for logs; never shown to callers) and returns e.
func (e *Error) WithCause(cause error) *Error {
	e.cause = cause
	return e
}

// New builds an error with an explicit code, status and caller-safe message.
func New(code string, status int, message string) *Error {
	return &Error{Code: code, Status: status, Message: message}
}

// BadRequest is a malformed request (400).
func BadRequest(message string) *Error {
	return New(CodeBadRequest, http.StatusBadRequest, message)
}

// Validation is a request that failed field validation (400); fields maps a field to its messages.
func Validation(fields map[string][]string) *Error {
	e := New(CodeValidationFailed, http.StatusBadRequest, "One or more validation errors occurred.")
	e.Fields = fields
	return e
}

// Unauthorized is "who are you" (401).
func Unauthorized(message string) *Error {
	return New(CodeUnauthorized, http.StatusUnauthorized, message)
}

// Forbidden is an authenticated caller that may not do this (403). Not for "who are you" (401).
func Forbidden(message string) *Error {
	return New(CodeForbidden, http.StatusForbidden, message)
}

// NotFound is a missing resource (404): "Order '42' was not found."
func NotFound(resource string, key any) *Error {
	return New(CodeNotFound, http.StatusNotFound, fmt.Sprintf("%s '%v' was not found.", resource, key))
}

// Conflict is a state conflict (409) with a service-specific code.
func Conflict(code, message string) *Error {
	return New(code, http.StatusConflict, message)
}

// ConcurrencyConflict is a row modified by another request since it was read (409).
func ConcurrencyConflict() *Error {
	return New(CodeConcurrencyConflict, http.StatusConflict,
		"The resource was modified by another request; re-read it and retry.")
}

// DomainRule is a violated business invariant (422), kept distinct from request-shape errors (400).
func DomainRule(code, message string) *Error {
	return New(code, http.StatusUnprocessableEntity, message)
}

// RateLimited is an exhausted budget (429) carrying the wait the client should observe.
func RateLimited(message string, retryAfter time.Duration) *Error {
	e := New(CodeRateLimitExceeded, http.StatusTooManyRequests, message)
	e.RetryAfter = retryAfter
	return e
}

// Timeout is the server-side request deadline (504).
func Timeout(message string) *Error {
	return New(CodeRequestTimeout, http.StatusGatewayTimeout, message)
}

// Upstream is a downstream that failed or timed out (502). It is the upstream's fault, not the
// caller's, and a client that sees it may retry — unlike a 4xx from that same upstream, which means
// WE sent something wrong and belongs in our logs as a 500, not blamed onward as a 502.
func Upstream(upstream, message string, cause error) *Error {
	return New(CodeUpstreamUnavailable, http.StatusBadGateway, upstream+": "+message).WithCause(cause)
}

// Unavailable is this service refusing work it cannot do safely right now (503).
func Unavailable(code, message string, retryAfter time.Duration) *Error {
	e := New(code, http.StatusServiceUnavailable, message)
	e.RetryAfter = retryAfter
	return e
}

// NotConfigured names a configuration section this deployment is missing (500, CodeNotConfigured).
func NotConfigured(section string) *Error {
	return New(CodeNotConfigured, http.StatusInternalServerError,
		"This deployment is missing the '"+section+"' configuration section.")
}

// Internal wraps an unexpected failure (500) with a generic detail; the cause goes to the log.
func Internal(cause error) *Error {
	return New(CodeInternalError, http.StatusInternalServerError,
		"The request could not be completed.").WithCause(cause)
}

// As extracts the *Error from an error chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// StatusOf returns the HTTP status an error maps to, 500 for anything untyped.
func StatusOf(err error) int {
	if e, ok := As(err); ok {
		return e.Status
	}
	return http.StatusInternalServerError
}

// MissingHeaders is a request without headers its route requires (400); each name is listed under
// Fields as "required", the same shape as a validation failure.
func MissingHeaders(names []string) *Error {
	e := New(CodeMissingHeaders, http.StatusBadRequest, "The request is missing required headers.")
	e.Fields = make(map[string][]string, len(names))
	for _, name := range names {
		e.Fields[name] = []string{"required"}
	}
	return e
}

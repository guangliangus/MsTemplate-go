package apperror

// Platform-wide machine-readable error codes, emitted as the errorCode member of every problem
// details body. They are part of the client contract: add, never rename. UPPER_SNAKE_CASE matches
// the codes the platform's identity service and its other services publish, so one vocabulary reaches
// every client. A service adds its own codes beside these in its own package (see
// internal/order for the reference).
const (
	// CodeBadRequest is a malformed request: unparsable JSON, a wrong type, a bad path parameter.
	CodeBadRequest = "BAD_REQUEST"
	// CodeValidationFailed is a well-formed request that failed field validation; see Error.Fields.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeMethodNotAllowed is the router's 405.
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	// CodeUnsupportedMediaType is a body in a content type the endpoint does not read.
	CodeUnsupportedMediaType = "UNSUPPORTED_MEDIA_TYPE"
	// CodePayloadTooLarge is a body over the configured limit.
	CodePayloadTooLarge = "PAYLOAD_TOO_LARGE"
	// CodeMissingHeaders is a request without the platform headers its route requires (X-Platform,
	// X-Request-ID, ...); the missing names are listed under errors.
	CodeMissingHeaders = "MISSING_HEADERS"

	// CodeUnauthorized is "who are you": no token, an expired one, a bad signature.
	CodeUnauthorized = "UNAUTHORIZED"
	// CodeForbidden is "not you": a valid token that may not do this.
	CodeForbidden = "FORBIDDEN"

	// CodeNotFound is a resource that does not exist (or is soft-deleted).
	CodeNotFound = "NOT_FOUND"
	// CodeConflict is a state conflict the client can resolve by re-reading.
	CodeConflict = "CONFLICT"
	// CodeConcurrencyConflict is a row modified by another request since it was read (xmin token).
	CodeConcurrencyConflict = "CONCURRENCY_CONFLICT"
	// CodeUniqueViolation is a unique constraint that refused the write (SQLSTATE 23505).
	CodeUniqueViolation = "UNIQUE_VIOLATION"

	// CodeRateLimitExceeded is a budget the caller exhausted; the response carries Retry-After.
	CodeRateLimitExceeded = "RATE_LIMIT_EXCEEDED"
	// CodeRequestTimeout is the server-side request deadline (504).
	CodeRequestTimeout = "REQUEST_TIMEOUT"
	// CodeClientClosedRequest is a client that went away before the response was written (499).
	CodeClientClosedRequest = "CLIENT_CLOSED_REQUEST"

	// CodeIdempotencyKeyInvalid is an Idempotency-Key header outside 1..128 characters.
	CodeIdempotencyKeyInvalid = "IDEMPOTENCY_KEY_INVALID"
	// CodeIdempotencyInFlight is a duplicate arriving while the original is still being processed.
	CodeIdempotencyInFlight = "IDEMPOTENCY_IN_FLIGHT"
	// CodeIdempotencyStoreUnavailable is the fail-closed 503 when the idempotency store is down.
	CodeIdempotencyStoreUnavailable = "IDEMPOTENCY_STORE_UNAVAILABLE"

	// CodeNotConfigured is a configuration section this deployment does not carry: a 500 like any
	// other server fault, but with its own code, because "the request could not be completed" sends
	// an operator to read code when the answer is a missing secret. The section's name travels in
	// the detail; no value ever does.
	CodeNotConfigured = "NOT_CONFIGURED"
	// CodeUpstreamUnavailable is a downstream service that failed or timed out (502).
	CodeUpstreamUnavailable = "UPSTREAM_SERVICE_UNAVAILABLE"
	// CodeServiceUnavailable is this service refusing work it cannot do safely right now (503).
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
	// CodeInternalError is everything that was not expected.
	CodeInternalError = "INTERNAL_ERROR"
)

// CodeForStatus returns the code a framework-produced response of this status carries, so that a
// router 404, a 405 or an empty-bodied 401 answered by a middleware all speak the same vocabulary.
func CodeForStatus(status int) string {
	switch status {
	case 400:
		return CodeBadRequest
	case 401:
		return CodeUnauthorized
	case 403:
		return CodeForbidden
	case 404:
		return CodeNotFound
	case 405:
		return CodeMethodNotAllowed
	case 409:
		return CodeConflict
	case 413:
		return CodePayloadTooLarge
	case 415:
		return CodeUnsupportedMediaType
	case 429:
		return CodeRateLimitExceeded
	case 499:
		return CodeClientClosedRequest
	case 502:
		return CodeUpstreamUnavailable
	case 503:
		return CodeServiceUnavailable
	case 504:
		return CodeRequestTimeout
	default:
		return CodeInternalError
	}
}

// Codes lists the whole platform vocabulary, for the guards that must cover every code: the
// localized message catalogue, the OpenAPI document. A new code is added here as well as above.
func Codes() []string {
	return []string{
		CodeBadRequest, CodeValidationFailed, CodeMethodNotAllowed, CodeUnsupportedMediaType,
		CodePayloadTooLarge, CodeMissingHeaders,
		CodeUnauthorized, CodeForbidden,
		CodeNotFound, CodeConflict, CodeConcurrencyConflict, CodeUniqueViolation,
		CodeRateLimitExceeded, CodeRequestTimeout, CodeClientClosedRequest,
		CodeIdempotencyKeyInvalid, CodeIdempotencyInFlight, CodeIdempotencyStoreUnavailable,
		CodeNotConfigured, CodeUpstreamUnavailable, CodeServiceUnavailable, CodeInternalError,
	}
}

package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"

	"example.com/msvcservice/pkg/auth"
	"example.com/msvcservice/pkg/resilience"
)

// retryTransport retries idempotent requests on connection failures and on 429/502/503/504,
// with the package's backoff. A request with a body is only retried when it can be replayed
// (GetBody set, which NewRequest does).
type retryTransport struct {
	next http.RoundTripper
	cfg  resilience.RetryConfig
	name string
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !retryable(req) {
		return t.next.RoundTrip(req)
	}
	var resp *http.Response
	name := "httpclient." + t.name
	err := resilience.Retry(req.Context(), name, t.cfg, isTransientExchange, func(_ context.Context) error {
		attempt, err := rewind(req)
		if err != nil {
			return err
		}
		resp, err = t.next.RoundTrip(attempt) //nolint:bodyclose // the caller closes the final response
		if err != nil {
			return err
		}
		if retryableStatus(resp.StatusCode) {
			drain(resp)
			return &statusRetry{status: resp.StatusCode}
		}
		return nil
	})
	var sr *statusRetry
	if errors.As(err, &sr) {
		// Attempts exhausted on a retryable status: hand the last response back as-is.
		return resp, nil
	}
	return resp, err
}

type statusRetry struct{ status int }

func (s *statusRetry) Error() string { return "retryable status " + http.StatusText(s.status) }

func retryable(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		// idempotent by definition
	default:
		if req.Header.Get("Idempotency-Key") == "" {
			return false
		}
	}
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isTransientExchange(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var sr *statusRetry
	if errors.As(err, &sr) {
		return true
	}
	if resilience.IsBreakerOpen(err) {
		return false
	}
	return true // connection refused, reset, EOF, DNS: all worth one more try
}

func rewind(req *http.Request) (*http.Request, error) {
	if req.GetBody == nil {
		return req, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Body = body
	return clone, nil
}

func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func bearerFromContext(ctx context.Context) string {
	return auth.BearerToken(ctx)
}

// Package httpclient builds the named, resilient HTTP clients a service uses to call its
// downstreams by Kubernetes service name. Each client is one transport chain:
//
//	breaker → retry (idempotent requests only) → platform headers + caller token → OpenTelemetry → net/http
//
// with one overall timeout per call. A downstream 5xx or timeout surfaces as 502
// UPSTREAM_SERVICE_UNAVAILABLE (the upstream's fault); a downstream 4xx is a *StatusError, which
// the error writer answers with a 500, because it means WE sent something wrong.
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"

	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/reqmeta"
	"example.com/msvcservice/pkg/resilience"
)

// Credential strategies.
const (
	// CredentialsNone sends no Authorization header: the downstream sits on the trusted network.
	CredentialsNone = "none"
	// CredentialsForwardCaller copies the caller's bearer token onto the outgoing request, for a
	// downstream that must decide as the user. Not for background work, which has no caller.
	CredentialsForwardCaller = "forward_caller"
)

// Config is one entry of the "service_clients" map, keyed by client name.
type Config struct {
	// BaseURL must end with '/': url resolution drops the last path segment of a base without
	// one, and the symptom is "the downstream 404s but the URL looks right".
	BaseURL string `koanf:"base_url"`
	// Timeout is the budget for the whole call, retries included.
	Timeout time.Duration `koanf:"timeout" validate:"min=0"`
	// Retry applies to idempotent requests (GET/HEAD/OPTIONS/PUT/DELETE, or any method carrying an
	// Idempotency-Key header) on connection errors and 429/502/503/504.
	Retry resilience.RetryConfig `koanf:"retry"`
	// Breaker opens after consecutive failures; on by default for a single downstream.
	Breaker resilience.BreakerConfig `koanf:"breaker"`
	// Credentials is none or forward_caller.
	Credentials string `koanf:"credentials" validate:"omitempty,oneof=none forward_caller"`
	// Headers are sent on every request (an API key from a Secret, for instance).
	Headers map[string]string `koanf:"headers"`
	// PropagateHeaders are copied from the inbound request (captured by middleware.PlatformHeaders)
	// onto every outbound call; empty means reqmeta.DefaultPropagated (X-Request-ID, X-Language).
	// X-Request-ID is always sent: without an inbound one the trace id stands in, so a downstream
	// that requires it (the platform's services do) is satisfied and the call stays correlated.
	PropagateHeaders []string `koanf:"propagate_headers"`
	// MaxConnsPerHost bounds the connection pool.
	MaxConnsPerHost int `koanf:"max_conns_per_host" validate:"min=0"`
}

// Validate enforces the trailing slash.
func (c *Config) Validate() error {
	if c.BaseURL == "" {
		return nil // a capability section: validated when the client is built, see New
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("base_url %q is not an absolute URL", c.BaseURL)
	}
	if !strings.HasSuffix(u.Path, "/") {
		return fmt.Errorf("base_url %q must end with '/'", c.BaseURL)
	}
	return nil
}

// Option tunes a client beyond its configuration section.
type Option func(*options)

type options struct {
	serviceName string
}

// WithServiceName sends X-Service-Name on every call: the platform's services require it on
// service-to-service routes, and it names the caller in the callee's logs. The composition root
// passes the service's own name.
func WithServiceName(name string) Option {
	return func(o *options) { o.serviceName = name }
}

// Client is a named downstream client.
type Client struct {
	name          string
	base          *url.URL
	http          *http.Client
	log           *slog.Logger
	notConfigured bool
}

// New builds the client. A missing base URL does not fail startup: the client is built in a
// "not configured" state and every call answers 500 NOT_CONFIGURED naming the section, so a
// capability nobody wired breaks only itself.
func New(name string, cfg Config, log *slog.Logger, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if cfg.BaseURL == "" {
		log.Warn("service client has no base_url; calls will fail with NOT_CONFIGURED", slog.String("client", name))
		return &Client{name: name, log: log, notConfigured: true}, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("httpclient %s: %w", name, err)
	}
	base, _ := url.Parse(cfg.BaseURL)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = cfg.MaxConnsPerHost
	transport.MaxIdleConnsPerHost = max(cfg.MaxConnsPerHost, 10)

	var rt http.RoundTripper = otelhttp.NewTransport(transport)
	propagate := cfg.PropagateHeaders
	if len(propagate) == 0 {
		propagate = reqmeta.DefaultPropagated
	}
	rt = &headerTransport{next: rt, headers: cfg.Headers, forwardCaller: cfg.Credentials == CredentialsForwardCaller,
		serviceName: o.serviceName, propagate: propagate}
	rt = &retryTransport{next: rt, cfg: cfg.Retry, name: name}
	//nolint:bodyclose // responses are returned to the caller, who closes them
	breaker := resilience.NewBreaker[*http.Response]("httpclient."+name, cfg.Breaker, log,
		resilience.IgnoreErrors(isCallerCancellation))
	rt = &breakerTransport{next: rt, cb: breaker}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{
		name: name,
		base: base,
		http: &http.Client{Transport: rt, Timeout: timeout},
		log:  log,
	}, nil
}

// Name is the client's configured name.
func (c *Client) Name() string { return c.name }

// NewRequest builds a request against the base URL. path is joined to the base path, query is
// optional, and a non-nil body is sent as JSON with a GetBody so retries can replay it.
func (c *Client) NewRequest(ctx context.Context, method, path string, query url.Values, body any,
) (*http.Request, error) {
	if c.notConfigured {
		return nil, apperror.NotConfigured("service_clients." + c.name)
	}
	target := c.base.JoinPath(path)
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}
	var reader io.Reader
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("httpclient %s: encode body: %w", c.name, err)
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("httpclient %s: build request: %w", c.name, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil }
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// Do sends the request. Transport failures, timeouts and an open breaker become 502
// UPSTREAM_SERVICE_UNAVAILABLE; the caller owns the response body otherwise.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.wrapTransportError(req, err)
	}
	return resp, nil
}

func (c *Client) wrapTransportError(req *http.Request, err error) error {
	switch {
	case req.Context().Err() != nil:
		// The caller's own deadline or cancellation, not the upstream's fault: let the error writer
		// answer 504 / 499 from the context error.
		return req.Context().Err()
	case resilience.IsBreakerOpen(err):
		return apperror.Upstream(c.name, "the circuit breaker is open.", err)
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return apperror.Upstream(c.name, "the call timed out.", err)
		}
		return apperror.Upstream(c.name, "the service is unreachable.", err)
	}
}

// StatusError is a downstream 4xx: the request we sent was wrong. It maps to a 500 (never a 502)
// with the body kept for the log.
type StatusError struct {
	Client string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("httpclient %s: downstream answered %d: %s", e.Client, e.Status, e.Body)
}

// DoJSON sends a JSON request and decodes a 2xx JSON response into out (which may be nil).
// 5xx becomes 502 UPSTREAM_SERVICE_UNAVAILABLE, 4xx becomes *StatusError.
func (c *Client) DoJSON(ctx context.Context, method, path string, query url.Values, in, out any) error {
	req, err := c.NewRequest(ctx, method, path, query, in)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	const maxErrorBody = 2048
	switch {
	case resp.StatusCode >= http.StatusInternalServerError:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return apperror.Upstream(c.name, fmt.Sprintf("the service answered %d.", resp.StatusCode),
			&StatusError{Client: c.name, Status: resp.StatusCode, Body: string(snippet)})
	case resp.StatusCode >= http.StatusBadRequest:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &StatusError{Client: c.name, Status: resp.StatusCode, Body: string(snippet)}
	case out == nil || resp.StatusCode == http.StatusNoContent:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	default:
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return apperror.Upstream(c.name, "the response could not be decoded.", err)
		}
		return nil
	}
}

// breakerTransport counts transport errors and 5xx as failures; a 5xx is still returned to the
// caller as a response. A call the caller itself cancelled (the client went away) is not counted
// at all; the client timeout and the caller's deadline still are, because a downstream too slow to
// answer within them is exactly what the breaker is for.
type breakerTransport struct {
	next http.RoundTripper
	cb   *gobreaker.CircuitBreaker[*http.Response]
}

var errServerFailure = errors.New("upstream 5xx")

// callerCancelled wraps a transport error caused by the caller's cancellation, so the breaker can
// leave it out of its counts.
type callerCancelled struct{ err error }

func (e *callerCancelled) Error() string { return e.err.Error() }
func (e *callerCancelled) Unwrap() error { return e.err }

func isCallerCancellation(err error) bool {
	var cc *callerCancelled
	return errors.As(err, &cc)
}

func (t *breakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := resilience.Execute(t.cb, func() (*http.Response, error) {
		resp, err := t.next.RoundTrip(req)
		if err != nil {
			// req's context carries the client timeout as a deadline, so only a cancellation that
			// came from the caller reads as context.Canceled here.
			if errors.Is(req.Context().Err(), context.Canceled) {
				return nil, &callerCancelled{err: err}
			}
			return nil, err
		}
		if resp.StatusCode >= http.StatusInternalServerError {
			return resp, errServerFailure
		}
		return resp, nil
	})
	var cc *callerCancelled
	switch {
	case errors.Is(err, errServerFailure):
		return resp, nil
	case errors.As(err, &cc):
		return resp, cc.err
	default:
		return resp, err
	}
}

// headerTransport adds the static headers, the platform headers carried over from the inbound
// request, the caller name and, when enabled, the caller's bearer token. A header the caller set
// on the request itself always wins.
type headerTransport struct {
	next          http.RoundTripper
	headers       map[string]string
	forwardCaller bool
	serviceName   string
	propagate     []string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	ctx := req.Context()
	for _, name := range t.propagate {
		if req.Header.Get(name) != "" {
			continue
		}
		if v := reqmeta.Value(ctx, name); v != "" {
			req.Header.Set(name, v)
		}
	}
	if req.Header.Get(reqmeta.HeaderRequestID) == "" {
		req.Header.Set(reqmeta.HeaderRequestID, requestID(ctx))
	}
	if t.serviceName != "" && req.Header.Get(reqmeta.HeaderServiceName) == "" {
		req.Header.Set(reqmeta.HeaderServiceName, t.serviceName)
	}
	if t.forwardCaller {
		if token := bearerFromContext(ctx); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	return t.next.RoundTrip(req)
}

// requestID is the id an outbound call carries when the inbound request brought none: the trace
// id, so the downstream's log line and ours meet on one value, or a fresh uuid outside a trace.
func requestID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

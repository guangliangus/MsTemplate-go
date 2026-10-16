package httpclient

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"example.com/msvcservice/pkg/reqmeta"
	"example.com/msvcservice/pkg/resilience"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestOutboundCallsCarryThePlatformHeaders(t *testing.T) {
	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	cfg := Config{BaseURL: upstream.URL + "/", Headers: map[string]string{"X-Api-Key": "k"}}
	c, err := New("users", cfg, quiet, WithServiceName("orders"))
	if err != nil {
		t.Fatal(err)
	}

	// With an inbound request: X-Request-ID and X-Language are carried over, device headers are not.
	inbound := http.Header{}
	inbound.Set("X-Request-ID", "req-7")
	inbound.Set("X-Language", "zh-TW")
	inbound.Set("X-Device-ID", "dev-1")
	ctx := reqmeta.Capture(context.Background(), inbound)
	if err := c.DoJSON(ctx, http.MethodGet, "me", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"X-Request-Id": "req-7", "X-Language": "zh-TW", "X-Service-Name": "orders", "X-Api-Key": "k"}
	for k, v := range want {
		if got := seen.Get(k); got != v {
			t.Fatalf("%s = %q, want %q", k, got, v)
		}
	}
	if seen.Get("X-Device-ID") != "" {
		t.Fatal("device headers are not propagated by default")
	}

	// Without an inbound request (a background job): X-Request-ID is still sent, as a generated id.
	if err := c.DoJSON(context.Background(), http.MethodGet, "me", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if id := seen.Get("X-Request-ID"); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("generated request id %q", id)
	}

	// A header set on the request by the caller wins over propagation.
	req, _ := c.NewRequest(ctx, http.MethodGet, "me", nil, nil)
	req.Header.Set("X-Language", "ja")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seen.Get("X-Language") != "ja" {
		t.Fatalf("caller's header overridden: %q", seen.Get("X-Language"))
	}
}

// A burst of callers giving up (client disconnects) says nothing about the downstream: the breaker
// must stay closed, while real failures still open it.
func TestCallerCancellationsDoNotOpenTheBreaker(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	cfg := Config{BaseURL: upstream.URL + "/", Timeout: 5 * time.Second,
		Retry: resilience.RetryConfig{MaxAttempts: 1},
		Breaker: resilience.BreakerConfig{Enabled: true, FailureThreshold: 2, OpenDuration: time.Minute,
			HalfOpenMaxRequests: 1}}
	c, err := New("down", cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		go func() { time.Sleep(10 * time.Millisecond); cancel() }()
		_ = c.DoJSON(ctx, http.MethodGet, "slow", nil, nil, nil)
		cancel()
	}
	if err := c.DoJSON(context.Background(), http.MethodGet, "fast", nil, nil, nil); err != nil {
		t.Fatalf("the breaker opened on caller cancellations: %v", err)
	}
}

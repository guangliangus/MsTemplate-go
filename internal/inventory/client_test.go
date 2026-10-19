package inventory_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/inventory"
	"github.com/guangliangus/go-msvc-kit/apperror"
	"github.com/guangliangus/go-msvc-kit/httpclient"
	"github.com/guangliangus/go-msvc-kit/resilience"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// stub is an inventory service that answers every request with one status and body and remembers
// the last request line it was asked.
type stub struct {
	mu    sync.Mutex
	asked string
}

func (s *stub) lastAsked() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

// newClient points the adapter at a stub answering status and body; retries are off so a 5xx is
// one exchange, and the breaker is off so one test cannot trip the next.
func newClient(t *testing.T, status int, body string) (*inventory.Client, *stub) {
	t.Helper()
	s := &stub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.asked = r.URL.RequestURI()
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c, err := httpclient.New("inventory", httpclient.Config{
		BaseURL: srv.URL + "/",
		Timeout: 2 * time.Second,
		Retry:   resilience.RetryConfig{MaxAttempts: 1},
	}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return inventory.New(c), s
}

func TestQuoteReturnsThePriceAndAsksForTheQuantity(t *testing.T) {
	client, s := newClient(t, http.StatusOK, `{"available":true,"unitPrice":19.99}`)
	q, err := client.Quote(context.Background(), "SKU-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Available || !q.UnitPrice.Equal(decimal.RequireFromString("19.99")) {
		t.Fatalf("%+v", q)
	}
	if got := s.lastAsked(); got != "/inventory/SKU-1/availability?quantity=2" {
		t.Fatalf("asked %s", got)
	}
}

func TestQuoteUnavailableAnswers(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"unknown SKU (404)":  {http.StatusNotFound, `{"title":"Not found."}`},
		"sold out":           {http.StatusOK, `{"available":false}`},
		"sold out, no price": {http.StatusOK, `{"available":false,"unitPrice":null}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := newClient(t, tc.status, tc.body)
			q, err := client.Quote(context.Background(), "SKU-1", 1)
			if err != nil || q.Available || !q.UnitPrice.IsZero() {
				t.Fatalf("got %+v %v", q, err)
			}
		})
	}
}

// What the adapter maps: an available SKU without a price and a 5xx are the downstream's fault
// (502 UPSTREAM_SERVICE_UNAVAILABLE); any other 4xx means we sent a bad request (a 500, with the
// status error kept for the log).
func TestQuoteMapsFaults(t *testing.T) {
	cases := map[string]struct {
		status     int
		body       string
		wantStatus int
		wantCode   string
	}{
		"available without unitPrice": {http.StatusOK, `{"available":true}`, 502, apperror.CodeUpstreamUnavailable},
		"available with null price":   {http.StatusOK, `{"available":true,"unitPrice":null}`, 502, apperror.CodeUpstreamUnavailable},
		"5xx":                         {http.StatusInternalServerError, `oops`, 502, apperror.CodeUpstreamUnavailable},
		"not JSON":                    {http.StatusOK, `<html>`, 502, apperror.CodeUpstreamUnavailable},
		"other 4xx":                   {http.StatusBadRequest, `{"title":"bad"}`, 500, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client, _ := newClient(t, tc.status, tc.body)
			_, err := client.Quote(context.Background(), "SKU-1", 1)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := apperror.StatusOf(err); got != tc.wantStatus {
				t.Fatalf("status %d, want %d: %v", got, tc.wantStatus, err)
			}
			if e, ok := apperror.As(err); ok && e.Code != tc.wantCode {
				t.Fatalf("code %s, want %s", e.Code, tc.wantCode)
			}
			var status *httpclient.StatusError
			if tc.status >= http.StatusBadRequest && (!errors.As(err, &status) || status.Status != tc.status) {
				t.Fatalf("the downstream status is kept for the log: %v", err)
			}
		})
	}
}

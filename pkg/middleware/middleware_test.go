package middleware

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"example.com/msvcservice/pkg/auth"
	"example.com/msvcservice/pkg/httpx"
	"example.com/msvcservice/pkg/redisx"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func problemOf(t *testing.T, rec *httptest.ResponseRecorder) httpx.Problem {
	t.Helper()
	var p httpx.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem body: %s", rec.Body.String())
	}
	return p
}

func TestClientIPResolver(t *testing.T) {
	var seen string
	capture := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = ClientIP(r) })

	cases := []struct {
		name   string
		cfg    ForwardedConfig
		remote string
		xff    string
		want   string
	}{
		{"disabled ignores XFF", ForwardedConfig{}, "10.0.0.1:4000", "1.2.3.4", "10.0.0.1"},
		{"rightmost entry wins", ForwardedConfig{Enabled: true}, "10.0.0.1:4000", "9.9.9.9, 1.2.3.4", "1.2.3.4"},
		{"trusted proxies are skipped", ForwardedConfig{Enabled: true, TrustedProxies: []string{"10.0.0.0/8"}},
			"10.0.0.1:4000", "1.2.3.4, 10.1.1.1", "1.2.3.4"},
		{"garbage falls back to remote", ForwardedConfig{Enabled: true}, "10.0.0.1:4000", "not-an-ip", "10.0.0.1"},
		{"no address at all", ForwardedConfig{}, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			ClientIPResolver(tc.cfg)(capture).ServeHTTP(httptest.NewRecorder(), req)
			if seen != tc.want {
				t.Fatalf("got %q want %q", seen, tc.want)
			}
		})
	}
}

func TestRecovererWritesProblem(t *testing.T) {
	h := Recoverer(quiet)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if p := problemOf(t, rec); rec.Code != 500 || p.ErrorCode != "INTERNAL_ERROR" {
		t.Fatalf("%d %+v", rec.Code, p)
	}
}

func TestRequestTimeoutBecomes504(t *testing.T) {
	h := RequestTimeout(20 * time.Millisecond)(httpx.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
		<-r.Context().Done()
		return r.Context().Err()
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if p := problemOf(t, rec); rec.Code != 504 || p.ErrorCode != "REQUEST_TIMEOUT" {
		t.Fatalf("%d %+v", rec.Code, p)
	}
}

func TestMaxBodyBytesBecomes413(t *testing.T) {
	h := MaxBodyBytes(4)(httpx.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
		_, err := io.ReadAll(r.Body)
		return err
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("too long")))
	if rec.Code != 413 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestRateLimiterPolicies(t *testing.T) {
	cfg := RateLimitConfig{Enabled: true, Global: "global", Policies: map[string]RateLimitPolicy{
		"global": {Requests: 2, Window: time.Minute, PartitionBy: PartitionCaller},
		"writes": {Requests: 1, Window: time.Minute, PartitionBy: PartitionIP},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rl := NewRateLimiter(cfg, quiet)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	t.Run("caller partition", func(t *testing.T) {
		h := rl.Global()(ok)
		call := func(sub string) int {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{Subject: sub}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Code
		}
		first, second := call("a"), call("a")
		if first != 204 || second != 204 {
			t.Fatal("two requests are within budget")
		}
		if call("a") != 429 {
			t.Fatal("the third is refused")
		}
		if call("b") != 204 {
			t.Fatal("another caller has its own budget")
		}
	})

	t.Run("unattributable requests are not counted", func(t *testing.T) {
		h := rl.Policy("writes")(ok)
		for range 3 {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = ""
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 204 {
				t.Fatal("a request with no address must not share a bucket")
			}
		}
	})

	t.Run("429 is a problem with Retry-After", func(t *testing.T) {
		h := rl.Policy("writes")(ok)
		for i := range 2 {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.RemoteAddr = "5.6.7.8:1"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if i == 1 {
				p := problemOf(t, rec)
				if rec.Code != 429 || p.ErrorCode != "RATE_LIMIT_EXCEEDED" || rec.Header().Get("Retry-After") == "" {
					t.Fatalf("%d %+v %v", rec.Code, p, rec.Header())
				}
			}
		}
	})
}

func newStore(t *testing.T) (*redisx.IdempotencyStore, *miniredis.Miniredis) {
	t.Helper()
	srv := miniredis.RunT(t)
	client, err := redisx.Open(redisx.Config{Addr: srv.Addr(), KeyPrefix: "t:", DialTimeout: time.Second,
		ReadTimeout: time.Second, WriteTimeout: time.Second, PoolSize: 2}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return redisx.NewIdempotencyStore(client), srv
}

func idempotencyConfig() IdempotencyConfig {
	return IdempotencyConfig{Enabled: true, InFlightTTL: time.Minute, Retention: time.Hour, MaxBodyBytes: 1024}
}

func TestIdempotencyReplaysSuccess(t *testing.T) {
	store, _ := newStore(t)
	calls := 0
	h := Idempotency(idempotencyConfig(), store, quiet)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Location", "/orders/1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader("{}"))
		req.Header.Set("Idempotency-Key", "k1")
		req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{Subject: "1"}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first, second := do(), do()
	if calls != 1 {
		t.Fatalf("handler ran %d times", calls)
	}
	if second.Code != 201 || second.Body.String() != first.Body.String() || second.Header().Get("Location") != "/orders/1" {
		t.Fatalf("replay differs: %d %s", second.Code, second.Body.String())
	}
	if second.Header().Get(ReplayedHeader) != "true" || first.Header().Get(ReplayedHeader) != "" {
		t.Fatal("only the replay is marked")
	}
}

func TestIdempotencyErrorsAreNotStoredAndKeysAreCallerScoped(t *testing.T) {
	store, _ := newStore(t)
	calls := 0
	h := Idempotency(idempotencyConfig(), store, quiet)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	do := func(sub string) int {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.Header.Set("Idempotency-Key", "same")
		req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{Subject: sub}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if do("a") != 400 || do("a") != 200 {
		t.Fatal("an error response must not be stored; the retry executes")
	}
	if do("b") != 200 || calls != 3 {
		t.Fatalf("another caller with the same key is a different request (calls=%d)", calls)
	}
}

func TestIdempotencyInFlightAndStoreDown(t *testing.T) {
	store, srv := newStore(t)
	release := make(chan struct{})
	h := Idempotency(idempotencyConfig(), store, quiet)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.Header.Set("Idempotency-Key", "k")
		return req
	}
	done := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, request())
		done <- rec.Code
	}()
	time.Sleep(50 * time.Millisecond)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request())
	if p := problemOf(t, rec); rec.Code != 409 || p.ErrorCode != "IDEMPOTENCY_IN_FLIGHT" {
		t.Fatalf("concurrent duplicate: %d %+v", rec.Code, p)
	}
	close(release)
	if <-done != 200 {
		t.Fatal("the original completes normally")
	}

	srv.Close()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request())
	if p := problemOf(t, rec); rec.Code != 503 || p.ErrorCode != "IDEMPOTENCY_STORE_UNAVAILABLE" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("store down must fail closed: %d %+v", rec.Code, p)
	}
}

func TestIdempotencyIgnoresRequestsWithoutKey(t *testing.T) {
	store, _ := newStore(t)
	calls := 0
	h := Idempotency(idempotencyConfig(), store, quiet)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	}
	if calls != 2 {
		t.Fatal("no key, no protection")
	}
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Idempotency-Key", strings.Repeat("k", 129))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("an over-long key is a 400, got %d", rec.Code)
	}
	_ = context.Background()
}

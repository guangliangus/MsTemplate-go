package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestValidateKeepsTheBudgetsAndListenersApart(t *testing.T) {
	ok := Config{Addr: ":8080", AdminAddr: ":8081", ShutdownTimeout: 25 * time.Second, DrainTimeout: 15 * time.Second,
		WriteTimeout: 60 * time.Second, RequestTimeout: 30 * time.Second}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"drain eats the whole budget": func(c *Config) { c.DrainTimeout = c.ShutdownTimeout },
		"one listener for both":       func(c *Config) { c.AdminAddr = c.Addr },
		"504 cut off":                 func(c *Config) { c.WriteTimeout = c.RequestTimeout },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A request still running when the drain budget ends is cut, so the components that stop after
// the HTTP server keep their share of the shutdown budget.
func TestDrainIsBoundedByItsShare(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	hold := make(chan struct{})
	defer close(hold)
	srv := New(Config{Addr: "127.0.0.1:0", DrainTimeout: 100 * time.Millisecond},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}), quiet)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	for srv.Addr() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := http.Get("http://" + srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "drain") || time.Since(start) > 2*time.Second {
			t.Fatalf("drain should give up after its share: %v after %s", err, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server waited past its drain budget")
	}
}

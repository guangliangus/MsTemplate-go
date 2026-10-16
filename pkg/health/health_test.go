package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func get(h http.Handler) (int, Report) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var rep Report
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	return rec.Code, rep
}

func TestProbes(t *testing.T) {
	r := New(time.Second)
	if code, _ := get(r.Startup()); code != 503 {
		t.Fatal("startup is 503 before MarkStarted")
	}
	r.MarkStarted()
	if code, _ := get(r.Startup()); code != 200 {
		t.Fatal("startup is 200 after MarkStarted")
	}
	if code, _ := get(r.Live()); code != 200 {
		t.Fatal("live is always 200")
	}

	healthy := true
	r.AddReadiness("db", func(context.Context) error {
		if healthy {
			return nil
		}
		return errors.New("down")
	})
	r.AddReadiness("slow", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	code, rep := get(r.Ready())
	if code != 503 || rep.Entries["slow"].Status != StatusUnhealthy || rep.Entries["db"].Status != StatusHealthy {
		t.Fatalf("timeouts are unhealthy: %d %+v", code, rep)
	}
}

func TestInformationalChecksDegradeWithoutFailingReadiness(t *testing.T) {
	r := New(time.Second)
	r.AddReadiness("db", func(context.Context) error { return nil })
	r.AddInformational("cache", func(context.Context) error { return errors.New("down") })
	code, rep := get(r.Ready())
	if code != 200 || rep.Status != StatusDegraded {
		t.Fatalf("a shared dependency's outage keeps the pod ready: %d %+v", code, rep)
	}
	if e := rep.Entries["cache"]; e.Status != StatusUnhealthy || e.Gating || e.Description != "down" {
		t.Fatalf("the failure is still reported: %+v", e)
	}
	r.AddReadiness("db", func(context.Context) error { return errors.New("gone") })
	if code, rep := get(r.Ready()); code != 503 || rep.Status != StatusUnhealthy {
		t.Fatalf("a gating failure wins over a degraded one: %d %+v", code, rep)
	}
}

func TestReadyAggregatesAndDrains(t *testing.T) {
	r := New(time.Second)
	r.AddReadiness("a", func(context.Context) error { return nil })
	if code, rep := get(r.Ready()); code != 200 || rep.Status != StatusHealthy {
		t.Fatalf("%d %+v", code, rep)
	}
	r.MarkDraining()
	code, rep := get(r.Ready())
	if code != 503 || rep.Entries["draining"].Status != StatusUnhealthy {
		t.Fatalf("draining must fail readiness: %d %+v", code, rep)
	}
}

func TestWatchKeepsTheOutcomesCurrentWithoutProbes(t *testing.T) {
	r := New(time.Second)
	r.AddInformational("broker", func(context.Context) error { return errors.New("down") })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := r.Watch(ctx, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	v, ok := checkOutcomes().Load("broker")
	if !ok || v.(outcome).up != 0 || v.(outcome).gating {
		t.Fatalf("the failing informational check is recorded: %+v", v)
	}
}

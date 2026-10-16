// Package health serves the three Kubernetes probes with one JSON shape:
//
//	/health/startup  — 200 once the host finished wiring (MarkStarted), 503 before
//	/health/live     — 200 while the process answers; it never looks at a dependency, so a
//	                   broken database restarts nothing
//	/health/ready    — runs every dependency check and 503s when a GATING check fails or the
//	                   host is draining, which takes the pod out of the Service
//
// Readiness answers "should THIS pod receive traffic", not "is everything fine". A dependency
// every replica shares (Redis, the broker) fails for all pods at once: gating on it removes every
// endpoint together, turns a degraded service into the ingress's bare 503 and stalls the rollout
// that would fix it. Such dependencies are registered as informational: the report shows them
// (status Degraded, still 200) and the alerting watches them, but the pod keeps serving.
//
// Every check's latest outcome is exported as msvc_health_check_up{check,gating} (1 or 0). Watch
// keeps it current between probes, so an informational dependency's outage still pages someone
// even though it no longer takes the pods out of the Service.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Status is the outcome of a check or of the whole report.
type Status string

const (
	// StatusHealthy means the check passed.
	StatusHealthy Status = "Healthy"
	// StatusDegraded means only informational checks failed; the pod stays ready.
	StatusDegraded Status = "Degraded"
	// StatusUnhealthy means the check failed or timed out.
	StatusUnhealthy Status = "Unhealthy"
)

// CheckFunc probes one dependency; a non-nil error is unhealthy.
type CheckFunc func(ctx context.Context) error

type check struct {
	fn     CheckFunc
	gating bool
}

// Registry holds the readiness checks and the host's lifecycle flags.
type Registry struct {
	mu       sync.RWMutex
	checks   map[string]check
	timeout  time.Duration
	started  atomic.Bool
	draining atomic.Bool
}

// New creates a registry whose checks are bounded by timeout each. Keep it below the readiness
// probe's timeoutSeconds, or the kubelet gives up on a check that would have passed.
func New(timeout time.Duration) *Registry {
	return &Registry{checks: map[string]check{}, timeout: timeout}
}

// AddReadiness registers a GATING check: when it fails /health/ready answers 503 and the pod
// leaves the Service. Only a dependency without which this pod cannot give a correct answer at all
// belongs here.
func (r *Registry) AddReadiness(name string, fn CheckFunc) {
	r.add(name, check{fn: fn, gating: true})
}

// AddInformational registers a check /health/ready reports but never fails on: a shared
// dependency whose outage the callers degrade around (a cache that falls back to the database, a
// broker behind the outbox). Its failure shows as Degraded with a 200.
func (r *Registry) AddInformational(name string, fn CheckFunc) {
	r.add(name, check{fn: fn, gating: false})
}

func (r *Registry) add(name string, c check) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[name] = c
}

// MarkStarted flips /health/startup to 200.
func (r *Registry) MarkStarted() { r.started.Store(true) }

// MarkDraining flips /health/ready to 503 for the rest of the process's life, so the endpoint
// controller stops routing new connections while in-flight work drains.
func (r *Registry) MarkDraining() { r.draining.Store(true) }

// Startup answers the startup probe.
func (r *Registry) Startup() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if r.started.Load() {
			writeReport(w, http.StatusOK, Report{Status: StatusHealthy, Entries: map[string]Entry{}})
			return
		}
		writeReport(w, http.StatusServiceUnavailable, Report{Status: StatusUnhealthy, Entries: map[string]Entry{
			"startup": {Status: StatusUnhealthy, Description: "the host is still starting"},
		}})
	})
}

// Live answers the liveness probe: the process is up. Nothing external is consulted.
func (r *Registry) Live() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeReport(w, http.StatusOK, Report{Status: StatusHealthy, Entries: map[string]Entry{}})
	})
}

// Ready answers the readiness probe by running every check concurrently: 503 only when a gating
// check failed or the host is draining.
func (r *Registry) Ready() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rep := r.Run(req.Context())
		status := http.StatusOK
		if rep.Status == StatusUnhealthy {
			status = http.StatusServiceUnavailable
		}
		writeReport(w, status, rep)
	})
}

// Report is the readiness outcome.
type Report struct {
	Status        Status           `json:"status"`
	TotalDuration string           `json:"totalDuration"`
	Entries       map[string]Entry `json:"entries"`
}

// Entry is one check's outcome. Gating says whether its failure takes the pod out of the Service.
type Entry struct {
	Status      Status `json:"status"`
	Gating      bool   `json:"gating"`
	Duration    string `json:"duration"`
	Description string `json:"description,omitempty"`
}

// Run executes the checks and builds the report: Unhealthy when a gating check failed or the host
// is draining, Degraded when only informational checks failed, Healthy otherwise.
func (r *Registry) Run(ctx context.Context) Report {
	start := time.Now()
	r.mu.RLock()
	checks := make(map[string]check, len(r.checks))
	for name, c := range r.checks {
		checks[name] = c
	}
	r.mu.RUnlock()

	type outcome struct {
		name string
		e    Entry
	}
	results := make(chan outcome, len(checks))
	for name, c := range checks {
		go func() {
			results <- outcome{name: name, e: r.runOne(ctx, c)}
		}()
	}

	rep := Report{Status: StatusHealthy, Entries: make(map[string]Entry, len(checks)+1)}
	for range checks {
		o := <-results
		rep.Entries[o.name] = o.e
		switch {
		case o.e.Status == StatusHealthy:
		case o.e.Gating:
			rep.Status = StatusUnhealthy
		case rep.Status == StatusHealthy:
			rep.Status = StatusDegraded
		default:
			// already Degraded or Unhealthy: an informational failure changes nothing
		}
	}
	recordOutcomes(rep.Entries)
	if r.draining.Load() {
		rep.Status = StatusUnhealthy
		rep.Entries["draining"] = Entry{
			Status: StatusUnhealthy, Gating: true, Duration: "0s", Description: "the host is shutting down",
		}
	}
	rep.TotalDuration = time.Since(start).String()
	return rep
}

// Watch runs the checks every interval until ctx ends, keeping msvc_health_check_up current where
// nothing probes (a local run) and between probes. Run it as a lifecycle component.
func (r *Registry) Watch(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		r.Run(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Registry) runOne(ctx context.Context, c check) Entry {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	start := time.Now()
	err := c.fn(ctx)
	e := Entry{Status: StatusHealthy, Gating: c.gating, Duration: time.Since(start).String()}
	if err != nil {
		e.Status = StatusUnhealthy
		e.Description = err.Error()
	}
	return e
}

func writeReport(w http.ResponseWriter, status int, rep Report) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rep)
}

// checkOutcomes holds the latest outcome per check for the gauge callback: the checks run on the
// probe and Watch cadence, never on the scrape path.
var checkOutcomes = sync.OnceValue(func() *sync.Map {
	outcomes := &sync.Map{}
	_, _ = otel.Meter("msvc/health").Int64ObservableGauge("msvc.health.check.up",
		metric.WithDescription("Latest outcome of each health check (1 healthy, 0 failing), by check and whether it "+
			"gates readiness. A failing non-gating check means the service runs degraded while every pod stays ready."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			outcomes.Range(func(key, value any) bool {
				name, last := key.(string), value.(outcome)
				o.Observe(last.up, metric.WithAttributes(
					attribute.String("check", name), attribute.String("gating", strconv.FormatBool(last.gating))))
				return true
			})
			return nil
		}))
	return outcomes
})

type outcome struct {
	up     int64
	gating bool
}

func recordOutcomes(entries map[string]Entry) {
	outcomes := checkOutcomes()
	for name, e := range entries {
		up := int64(0)
		if e.Status == StatusHealthy {
			up = 1
		}
		outcomes.Store(name, outcome{up: up, gating: e.Gating})
	}
}

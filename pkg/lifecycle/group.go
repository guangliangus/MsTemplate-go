// Package lifecycle runs the host's long-lived components (HTTP server, consumers, dispatchers,
// task runner) as one group and stops them in reverse order of registration: the HTTP server first,
// so readiness fails and the endpoint controller stops routing, then the workers, each given the
// rest of the shutdown budget to drain. Pair the budget with the pod's terminationGracePeriodSeconds.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Runner is a component that runs until its context is cancelled. Returning before that, with a
// nil error, is a normal early exit (a consumer host with nothing to consume); returning an error
// stops the whole group.
type Runner interface {
	Run(ctx context.Context) error
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context) error

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context) error { return f(ctx) }

// Group is an ordered set of runners.
type Group struct {
	log             *slog.Logger
	shutdownTimeout time.Duration
	items           []item
}

type item struct {
	name   string
	runner Runner
}

// NewGroup creates a group whose ordered shutdown is bounded by shutdownTimeout in total.
func NewGroup(log *slog.Logger, shutdownTimeout time.Duration) *Group {
	return &Group{log: log, shutdownTimeout: shutdownTimeout}
}

// Add registers a runner; registration order is start order and reverse stop order.
func (g *Group) Add(name string, r Runner) {
	g.items = append(g.items, item{name: name, runner: r})
}

type running struct {
	name   string
	cancel context.CancelFunc
	done   chan error
}

type failure struct {
	name string
	err  error
}

// Run starts every runner, waits for ctx to be cancelled or a runner to fail, then stops the
// runners in reverse order. It returns the failure that ended the group, or nil on a clean stop.
func (g *Group) Run(ctx context.Context) error {
	base := context.WithoutCancel(ctx)
	failed := make(chan failure, len(g.items))
	started := make([]running, 0, len(g.items))

	for _, it := range g.items {
		rctx, cancel := context.WithCancel(base)
		done := make(chan error, 1)
		started = append(started, running{name: it.name, cancel: cancel, done: done})
		go g.runOne(rctx, it, done, failed)
	}

	var cause error
	select {
	case <-ctx.Done():
		g.log.Info("shutdown requested")
	case f := <-failed:
		cause = fmt.Errorf("%s: %w", f.name, f.err)
		g.log.Error("component failed; stopping the host", slog.String("component", f.name), slog.Any("error", f.err))
	}

	g.stopAll(started)
	return cause
}

func (g *Group) runOne(ctx context.Context, it item, done chan<- error, failed chan<- failure) {
	err := it.runner.Run(ctx)
	done <- err
	switch {
	case err == nil:
		if ctx.Err() == nil {
			g.log.Info("component exited early", slog.String("component", it.name))
		}
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		// stopped on request
	default:
		failed <- failure{name: it.name, err: err}
	}
}

func (g *Group) stopAll(started []running) {
	deadline := time.Now().Add(g.shutdownTimeout)
	for i := len(started) - 1; i >= 0; i-- {
		r := started[i]
		r.cancel()
		remaining := max(time.Until(deadline), 0)
		select {
		case err := <-r.done:
			if err != nil && !errors.Is(err, context.Canceled) {
				g.log.Warn("component stopped with error", slog.String("component", r.name), slog.Any("error", err))
			} else {
				g.log.Info("component stopped", slog.String("component", r.name))
			}
		case <-time.After(remaining):
			g.log.Error("component did not stop within the shutdown budget; abandoning it",
				slog.String("component", r.name))
		}
	}
}

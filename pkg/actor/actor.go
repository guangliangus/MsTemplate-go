// Package actor identifies who is performing the current unit of work, for audit columns and
// logs. HTTP requests resolve it from the token (sub or client_id), message consumers and
// background work fall back to System.
package actor

import (
	"context"
	"sync"
)

// System is the actor recorded when no caller is attached to the context.
const System = "system"

type contextKey struct{}

type recorderKey struct{}

// recorder lets an outer frame (the request logger) see an actor an inner frame (authentication)
// established later on a derived context, which plain context values cannot do.
type recorder struct {
	mu   sync.Mutex
	name string
}

func (r *recorder) set(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.name = name
}

func (r *recorder) get() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.name
}

// WithRecorder attaches a slot that later WithName calls on derived contexts fill, so a middleware
// that runs outside authentication can still log the caller.
func WithRecorder(ctx context.Context) context.Context {
	return context.WithValue(ctx, recorderKey{}, &recorder{})
}

// WithName returns a context carrying the actor name (and records it for outer frames).
func WithName(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	if rec, ok := ctx.Value(recorderKey{}).(*recorder); ok {
		rec.set(name)
	}
	return context.WithValue(ctx, contextKey{}, name)
}

// FromContext returns the actor name, or System when none is attached.
func FromContext(ctx context.Context) string {
	if name, ok := ctx.Value(contextKey{}).(string); ok && name != "" {
		return name
	}
	if rec, ok := ctx.Value(recorderKey{}).(*recorder); ok {
		if name := rec.get(); name != "" {
			return name
		}
	}
	return System
}

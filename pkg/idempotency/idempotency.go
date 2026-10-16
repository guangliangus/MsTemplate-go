// Package idempotency is the storage contract behind the Idempotency-Key middleware: the types the
// middleware reads and writes, and the Store interface pkg/redisx implements. It lives apart from
// both so neither imports the other.
package idempotency

import (
	"context"
	"time"
)

// StoredResponse is a captured successful response, replayed verbatim for duplicates.
type StoredResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    []byte            `json:"body"`
}

// BeginStatus is the outcome of claiming a key.
type BeginStatus int

const (
	// Claimed means this request owns the key and must execute.
	Claimed BeginStatus = iota
	// InFlight means another request holds the key and has not finished.
	InFlight
	// Completed means a response is stored for replay.
	Completed
)

// BeginResult is the claim outcome, with the stored response when Completed.
type BeginResult struct {
	Status   BeginStatus
	Response *StoredResponse
}

// Store is what the middleware needs. Any error from a method is a store failure by contract:
// the middleware fails closed on it.
type Store interface {
	// TryBegin atomically claims the key for inFlightTTL or reports its current state.
	TryBegin(ctx context.Context, key string, inFlightTTL time.Duration) (BeginResult, error)
	// Complete stores the response for retention.
	Complete(ctx context.Context, key string, resp StoredResponse, retention time.Duration) error
	// Abandon releases a claim so the client may retry.
	Abandon(ctx context.Context, key string) error
}

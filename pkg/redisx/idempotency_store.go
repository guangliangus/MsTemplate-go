package redisx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"example.com/msvcservice/pkg/idempotency"
)

// inFlightMarker is the value a claimed-but-unfinished key holds.
const inFlightMarker = `{"inFlight":true}`

type storedValue struct {
	InFlight bool                        `json:"inFlight,omitempty"`
	Response *idempotency.StoredResponse `json:"response,omitempty"`
}

// IdempotencyStore keeps claims and response snapshots in Redis. The claim is one SET NX GET
// round trip (Redis >= 7.0), so two concurrent duplicates cannot both win.
type IdempotencyStore struct {
	client *Client
}

// NewIdempotencyStore builds the store on the client.
func NewIdempotencyStore(client *Client) *IdempotencyStore {
	return &IdempotencyStore{client: client}
}

// TryBegin claims the key or reports its state.
func (s *IdempotencyStore) TryBegin(ctx context.Context, key string, inFlightTTL time.Duration,
) (idempotency.BeginResult, error) {
	k := s.client.Key("idem", key)
	args := redis.SetArgs{Mode: "NX", TTL: inFlightTTL, Get: true}
	previous, err := s.client.SetArgs(ctx, k, inFlightMarker, args).Result()
	if errors.Is(err, redis.Nil) {
		return idempotency.BeginResult{Status: idempotency.Claimed}, nil
	}
	if err != nil {
		return idempotency.BeginResult{}, fmt.Errorf("redisx: idempotency claim: %w", err)
	}
	var value storedValue
	if err := json.Unmarshal([]byte(previous), &value); err != nil {
		return idempotency.BeginResult{}, fmt.Errorf("redisx: idempotency value is corrupt: %w", err)
	}
	if value.InFlight || value.Response == nil {
		return idempotency.BeginResult{Status: idempotency.InFlight}, nil
	}
	return idempotency.BeginResult{Status: idempotency.Completed, Response: value.Response}, nil
}

// Complete stores the response for retention.
func (s *IdempotencyStore) Complete(ctx context.Context, key string, resp idempotency.StoredResponse,
	retention time.Duration) error {
	body, err := json.Marshal(storedValue{Response: &resp})
	if err != nil {
		return fmt.Errorf("redisx: encode idempotent response: %w", err)
	}
	if err := s.client.Set(ctx, s.client.Key("idem", key), body, retention).Err(); err != nil {
		return fmt.Errorf("redisx: store idempotent response: %w", err)
	}
	return nil
}

// Abandon releases the claim.
func (s *IdempotencyStore) Abandon(ctx context.Context, key string) error {
	if err := s.client.Del(ctx, s.client.Key("idem", key)).Err(); err != nil {
		return fmt.Errorf("redisx: release idempotency claim: %w", err)
	}
	return nil
}

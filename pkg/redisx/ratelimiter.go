package redisx

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Decision is the outcome of a business rate-limit check.
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// One script, one round trip: INCR and PEXPIRE as separate commands can be split by a dropped
// connection, leaving a key that never expires and a subject that is refused forever. The TTL is
// deliberately not renewed on later hits, or a politely retrying client would push its own reset
// time further away with every attempt.
var fixedWindowScript = redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then
  redis.call("PEXPIRE", KEYS[1], ARGV[1])
end
local ttl = redis.call("PTTL", KEYS[1])
return {n, ttl}`)

// FixedWindowAllow counts one hit for subject in a fixed window shared by every replica (the
// in-process HTTP limiter's budget multiplies with the replica count; this one does not).
// Callers decide what a store failure means; the platform default is fail-open: the endpoint is
// still authenticated, validated and audited, and refusing everything during a Redis blip would
// make the limiter cause the outage it exists to prevent.
func (c *Client) FixedWindowAllow(ctx context.Context, subject string, limit int, window time.Duration,
) (Decision, error) {
	key := c.Key("ratelimit", subject)
	values, err := fixedWindowScript.Run(ctx, c, []string{key}, window.Milliseconds()).Int64Slice()
	if err != nil {
		return Decision{Allowed: true}, fmt.Errorf("redisx: rate limit %s: %w", subject, err)
	}
	if len(values) != 2 {
		return Decision{Allowed: true}, fmt.Errorf("redisx: rate limit %s: unexpected script reply", subject)
	}
	count, ttlMillis := int(values[0]), values[1]
	retryAfter := time.Duration(max(ttlMillis, 0)) * time.Millisecond
	if count > limit {
		return Decision{Allowed: false, Remaining: 0, RetryAfter: retryAfter}, nil
	}
	return Decision{Allowed: true, Remaining: limit - count, RetryAfter: retryAfter}, nil
}

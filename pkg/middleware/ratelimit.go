package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/httprate"

	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/auth"
	"example.com/msvcservice/pkg/httpx"
)

// Partition strategies for a rate-limit policy.
const (
	// PartitionNone is one shared bucket.
	PartitionNone = "none"
	// PartitionIP is one bucket per client address.
	PartitionIP = "ip"
	// PartitionCaller is one bucket per authenticated caller, falling back to the address.
	PartitionCaller = "caller"
)

// RateLimitPolicy is one named sliding-window limit.
type RateLimitPolicy struct {
	// Requests allowed per Window.
	Requests int `koanf:"requests" validate:"min=1"`
	// Window length.
	Window time.Duration `koanf:"window" validate:"min=1ms"`
	// PartitionBy is none, ip or caller.
	PartitionBy string `koanf:"partition_by" validate:"omitempty,oneof=none ip caller"`
}

// RateLimitConfig is bound from "http.rate_limiting".
//
// In-process limits are PER POD: the effective global budget is the configured value times the
// replica count, and grows when the HPA scales out. True global rate limiting belongs to the
// gateway. Anonymous floods never reach these limits either (authentication 401s first), which is
// also the gateway's job.
type RateLimitConfig struct {
	Enabled bool `koanf:"enabled"`
	// Global names the policy applied to every route, or "" for none.
	Global string `koanf:"global"`
	// Policies by name; routes opt into one with RateLimiter.Policy(name).
	Policies map[string]RateLimitPolicy `koanf:"policies" validate:"dive"`
}

// Validate checks that Global names a policy.
func (c *RateLimitConfig) Validate() error {
	if c.Global != "" {
		if _, ok := c.Policies[c.Global]; !ok {
			return fmt.Errorf("rate_limiting.global names unknown policy %q", c.Global)
		}
	}
	return nil
}

// RateLimiter builds middlewares from the configured policies.
type RateLimiter struct {
	cfg RateLimitConfig
	log *slog.Logger
}

// NewRateLimiter creates the factory.
func NewRateLimiter(cfg RateLimitConfig, log *slog.Logger) *RateLimiter {
	return &RateLimiter{cfg: cfg, log: log}
}

// Global returns the global policy's middleware, or a pass-through.
func (rl *RateLimiter) Global() func(http.Handler) http.Handler {
	if !rl.cfg.Enabled || rl.cfg.Global == "" {
		return passThrough
	}
	return rl.Policy(rl.cfg.Global)
}

// Policy returns the middleware for a named policy. An unknown name is a startup bug, so it
// panics rather than silently leaving the route unprotected.
func (rl *RateLimiter) Policy(name string) func(http.Handler) http.Handler {
	policy, ok := rl.cfg.Policies[name]
	if !ok {
		panic(fmt.Sprintf("middleware: rate limit policy %q is not configured", name))
	}
	if !rl.cfg.Enabled {
		return passThrough
	}
	limiter := httprate.NewRateLimiter(policy.Requests, policy.Window,
		httprate.WithKeyFuncs(func(r *http.Request) (string, error) {
			return partitionKey(r, policy.PartitionBy), nil
		}),
		httprate.WithLimitHandler(func(w http.ResponseWriter, r *http.Request) {
			retryAfter := policy.Window
			if s := w.Header().Get("Retry-After"); s != "" {
				if secs, err := strconv.Atoi(s); err == nil {
					retryAfter = time.Duration(secs) * time.Second
				}
			}
			httpx.Error(w, r, apperror.RateLimited(
				"Too many requests under the '"+name+"' policy; retry after the indicated delay.", retryAfter))
		}),
	)
	return func(next http.Handler) http.Handler {
		limited := limiter.Handler(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A request the host cannot attribute to anyone is not counted at all, rather than into
			// one bucket every such request shares: a budget that cannot name its subject is not a
			// budget, and a shared bucket lets the first callers lock everyone else out.
			if partitionKey(r, policy.PartitionBy) == "" {
				next.ServeHTTP(w, r)
				return
			}
			limited.ServeHTTP(w, r)
		})
	}
}

func partitionKey(r *http.Request, strategy string) string {
	switch strategy {
	case PartitionNone:
		return "shared"
	case PartitionIP:
		if ip := ClientIP(r); ip != "" {
			return "ip:" + ip
		}
		return ""
	default: // caller
		if p, ok := auth.FromContext(r.Context()); ok && p.Actor() != "" {
			return "caller:" + p.Actor()
		}
		if ip := ClientIP(r); ip != "" {
			return "ip:" + ip
		}
		return ""
	}
}

func passThrough(next http.Handler) http.Handler { return next }

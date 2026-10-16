package middleware

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/auth"
	"example.com/msvcservice/pkg/httpx"
	"example.com/msvcservice/pkg/idempotency"
)

// Idempotency-Key header names.
const (
	// DefaultIdempotencyHeader is the request header carrying the key.
	DefaultIdempotencyHeader = "Idempotency-Key"
	// ReplayedHeader marks a response served from the store.
	ReplayedHeader = "Idempotency-Replayed"
)

const (
	maxIdempotencyKeyLength     = 128
	storeUnavailableRetryAfter  = 5 * time.Second
	headersWorthReplaying       = 3
	defaultIdempotencyMaxBody   = 256 * 1024
	defaultIdempotencyInFlight  = time.Minute
	defaultIdempotencyRetention = 24 * time.Hour
)

// IdempotencyConfig is bound from "http.idempotency".
type IdempotencyConfig struct {
	Enabled bool `koanf:"enabled"`
	// Header carrying the key; default Idempotency-Key.
	Header string `koanf:"header"`
	// Methods the middleware protects; default POST and PATCH.
	Methods []string `koanf:"methods"`
	// InFlightTTL bounds how long a claim without a stored response blocks duplicates (a crashed
	// pod must not pin a key forever).
	InFlightTTL time.Duration `koanf:"in_flight_ttl" validate:"min=1s"`
	// Retention is how long a stored response replays.
	Retention time.Duration `koanf:"retention" validate:"min=1m"`
	// MaxBodyBytes caps the response size that is stored; larger responses are not replayed.
	MaxBodyBytes int `koanf:"max_body_bytes" validate:"min=1"`
}

func (c IdempotencyConfig) header() string {
	if c.Header == "" {
		return DefaultIdempotencyHeader
	}
	return c.Header
}

func (c IdempotencyConfig) methods() []string {
	if len(c.Methods) == 0 {
		return []string{http.MethodPost, http.MethodPatch}
	}
	return c.Methods
}

// Idempotency replays stored responses for duplicate unsafe requests that carry the idempotency
// header — the HTTP counterpart of the messaging inbox. The first request per key executes; its
// successful (<400) response (status, content type, Location, body) is stored and replayed
// verbatim for duplicates, marked with Idempotency-Replayed. Concurrent duplicates get 409 while
// the original is in flight. Error responses are never stored, so the client may retry with the
// same key. The key is scoped per caller, method and path; the body is NOT fingerprinted.
//
// It must run after authentication (keys are caller-scoped) and after rate limiting (a replay
// still counts against the caller's budget).
//
// Failure policy: when the store is unreachable the request is refused with 503 rather than run
// unprotected. There is nothing underneath an idempotency key — with the store down nobody knows
// whether this key already ran — so failing open would not degrade the guarantee but delete it,
// silently, for exactly the writes a caller marked must-not-duplicate.
func Idempotency(cfg IdempotencyConfig, store idempotency.Store, log *slog.Logger) func(http.Handler) http.Handler {
	header := cfg.header()
	methods := cfg.methods()
	return func(next http.Handler) http.Handler {
		if !cfg.Enabled || store == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !slices.Contains(methods, r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			raw := r.Header.Get(header)
			if raw == "" {
				next.ServeHTTP(w, r)
				return
			}
			if len(raw) > maxIdempotencyKeyLength {
				httpx.Error(w, r, apperror.New(apperror.CodeIdempotencyKeyInvalid, http.StatusBadRequest,
					"The "+header+" header must be 1-128 characters long."))
				return
			}
			key := idempotencyKey(r, raw)

			begin, err := store.TryBegin(r.Context(), key, cfg.InFlightTTL)
			if err != nil {
				if r.Context().Err() != nil {
					return
				}
				log.ErrorContext(r.Context(),
					"idempotency store unavailable; refusing the request rather than executing it unprotected",
					slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("error", err))
				httpx.Error(w, r, apperror.Unavailable(apperror.CodeIdempotencyStoreUnavailable,
					"The idempotency store is unavailable; retry with the same key.", storeUnavailableRetryAfter))
				return
			}
			switch begin.Status {
			case idempotency.Completed:
				replay(w, begin.Response)
			case idempotency.InFlight:
				httpx.Error(w, r, apperror.New(apperror.CodeIdempotencyInFlight, http.StatusConflict,
					"A request with the same idempotency key is still being processed."))
			default:
				executeAndCapture(w, r, next, store, key, cfg, log)
			}
		})
	}
}

func idempotencyKey(r *http.Request, raw string) string {
	caller := "anon:" + ClientIP(r)
	if p, ok := auth.FromContext(r.Context()); ok {
		caller = p.Actor()
	}
	return strings.Join([]string{caller, r.Method, r.URL.Path, raw}, "|")
}

func replay(w http.ResponseWriter, resp *idempotency.StoredResponse) {
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set(ReplayedHeader, "true")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// captureWriter buffers the response so a successful one can be stored after the handler ran.
type captureWriter struct {
	http.ResponseWriter
	status  int
	body    bytes.Buffer
	limit   int
	tooBig  bool
	written bool
}

func (c *captureWriter) WriteHeader(status int) {
	if !c.written {
		c.status = status
		c.written = true
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.written {
		c.WriteHeader(http.StatusOK)
	}
	if !c.tooBig {
		if c.body.Len()+len(b) > c.limit {
			c.tooBig = true
			c.body.Reset()
		} else {
			c.body.Write(b)
		}
	}
	return c.ResponseWriter.Write(b)
}

func executeAndCapture(w http.ResponseWriter, r *http.Request, next http.Handler, store idempotency.Store,
	key string, cfg IdempotencyConfig, log *slog.Logger) {
	cw := &captureWriter{ResponseWriter: w, status: http.StatusOK, limit: cfg.MaxBodyBytes}
	completed := false
	defer func() {
		if completed {
			return
		}
		// A panic or an error status: free the claim so the client can retry with the same key.
		if err := store.Abandon(context.WithoutCancel(r.Context()), key); err != nil {
			log.WarnContext(r.Context(), "idempotency claim could not be released", slog.Any("error", err))
		}
	}()

	next.ServeHTTP(cw, r)

	if cw.status >= http.StatusBadRequest || cw.tooBig {
		return
	}
	headers := make(map[string]string, headersWorthReplaying)
	for _, h := range []string{"Content-Type", "Location", TraceHeaderName} {
		if v := cw.Header().Get(h); v != "" {
			headers[h] = v
		}
	}
	stored := idempotency.StoredResponse{Status: cw.status, Headers: headers, Body: cw.body.Bytes()}
	if err := store.Complete(context.WithoutCancel(r.Context()), key, stored, cfg.Retention); err != nil {
		// The response already went out; the claim is released below so a retry re-executes rather
		// than waiting out the in-flight TTL.
		log.WarnContext(r.Context(), "idempotent response could not be stored", slog.Any("error", err))
		return
	}
	completed = true
}

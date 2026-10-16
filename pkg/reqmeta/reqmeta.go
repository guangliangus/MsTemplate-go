// Package reqmeta carries the platform's request headers from the inbound request to the places
// that need them later: the access log and every outbound call to another service. The platform's
// services expect X-Request-ID, X-Language and X-Service-Name on service-to-service calls and
// refuse requests without them, so a service built from the template passes them on instead of
// making every handler remember to.
package reqmeta

import (
	"context"
	"net/http"
	"strings"
)

// Platform header names.
const (
	HeaderRequestID   = "X-Request-ID"
	HeaderLanguage    = "X-Language"
	HeaderServiceName = "X-Service-Name"
	HeaderPlatform    = "X-Platform"
	HeaderDeviceID    = "X-Device-ID"
	HeaderAppVersion  = "X-App-Version"
	HeaderCurrency    = "X-Currency"
	HeaderRegion      = "X-Region"
)

// Known lists the headers Capture keeps; anything else on the request is ignored.
var Known = []string{
	HeaderRequestID, HeaderLanguage, HeaderServiceName, HeaderPlatform,
	HeaderDeviceID, HeaderAppVersion, HeaderCurrency, HeaderRegion,
}

// DefaultPropagated are the headers an outbound call carries from the inbound request unless the
// client is configured otherwise. The device headers are deliberately not in it: a downstream that
// needs them asks for them explicitly.
var DefaultPropagated = []string{HeaderRequestID, HeaderLanguage}

// MaxValueLength bounds one header value; longer values are dropped, never truncated, because a
// truncated id correlates with nothing.
const MaxValueLength = 128

type ctxKey struct{}

// Capture reads the known headers off h and returns a context carrying the ones present.
func Capture(ctx context.Context, h http.Header) context.Context {
	values := map[string]string{}
	for _, name := range Known {
		if v := Clean(h.Get(name)); v != "" {
			values[http.CanonicalHeaderKey(name)] = v
		}
	}
	if len(values) == 0 {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, values)
}

// Value returns the captured header, or "".
func Value(ctx context.Context, name string) string {
	values, _ := ctx.Value(ctxKey{}).(map[string]string)
	return values[http.CanonicalHeaderKey(name)]
}

// RequestID is the captured X-Request-ID, or "".
func RequestID(ctx context.Context) string { return Value(ctx, HeaderRequestID) }

// Clean returns v trimmed, or "" when it is empty, longer than MaxValueLength or carries a control
// character: a value that would break a log line or a header is not worth keeping.
func Clean(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > MaxValueLength {
		return ""
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return v
}

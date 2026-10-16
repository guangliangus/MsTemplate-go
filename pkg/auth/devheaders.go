package auth

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// Header names the development scheme reads.
const (
	// HeaderDevSubject is required; it becomes sub.
	HeaderDevSubject = "X-Dev-Subject"
	// HeaderDevRealm is optional; CONSUMER or BACKOFFICE. Absent means absent: every realm policy
	// refuses, exactly as for a real token with no realm. Forgetting it answers 403, not an
	// accidental consumer identity.
	HeaderDevRealm = "X-Dev-Realm"
	// HeaderDevScopes is optional, space separated.
	HeaderDevScopes = "X-Dev-Scopes"
	// HeaderDevClientID is optional; how a machine caller is imitated.
	HeaderDevClientID = "X-Dev-Client-Id"
)

// DevelopmentEnvironment is the only environment name the development scheme answers in.
const DevelopmentEnvironment = "development"

// DevHeaders fabricates a principal out of request headers so `go run` plus curl reaches a
// protected endpoint with no user service running anywhere. It validates nothing, so it fails
// towards refusing twice over: no subject header means no principal, and the environment is
// re-checked here rather than only where it is registered, so a dev_headers section that leaks
// into a staging config map buys an attacker nothing.
type DevHeaders struct {
	enabled     bool
	environment string
	log         *slog.Logger
	warnOnce    sync.Once
}

// NewDevHeaders builds the scheme for the running environment.
func NewDevHeaders(cfg DevHeadersConfig, environment string, log *slog.Logger) *DevHeaders {
	return &DevHeaders{enabled: cfg.Enabled, environment: strings.ToLower(environment), log: log}
}

// Active reports whether header identities are accepted: enabled AND the development environment.
func (d *DevHeaders) Active() bool {
	if d == nil || !d.enabled {
		return false
	}
	if d.environment != DevelopmentEnvironment {
		d.warnOnce.Do(func() {
			d.log.Warn("auth.dev_headers.enabled is set outside the development environment and is ignored; "+
				"remove it from this environment's configuration",
				slog.String("environment", d.environment))
		})
		return false
	}
	return true
}

// Principal reads the headers; nil when X-Dev-Subject is absent.
func (d *DevHeaders) Principal(r *http.Request) *Principal {
	subject := strings.TrimSpace(r.Header.Get(HeaderDevSubject))
	if subject == "" {
		return nil
	}
	return &Principal{
		Subject:  subject,
		Realm:    strings.TrimSpace(r.Header.Get(HeaderDevRealm)),
		Scope:    strings.Join(strings.Fields(r.Header.Get(HeaderDevScopes)), " "),
		ClientID: strings.TrimSpace(r.Header.Get(HeaderDevClientID)),
		Scheme:   SchemeDevHeader,
	}
}

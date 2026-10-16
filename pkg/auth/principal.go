package auth

import (
	"context"
	"strings"

	"example.com/msvcservice/pkg/actor"
)

// Principal is the authenticated caller.
type Principal struct {
	// Subject is the account id (ClaimSubject).
	Subject string
	// Realm is the identity plane, or "" for a token that carries none.
	Realm string
	// SessionID is the device session, or "".
	SessionID string
	// ClientID is a machine caller's id, or "".
	ClientID string
	// Scope is the space-separated scope list, or "".
	Scope string
	// Scheme says how the principal was established (SchemeBearer or SchemeDevHeader).
	Scheme string
	// RawToken is the bearer token as received, for forwarding to downstream services.
	RawToken string
}

// Scopes splits the scope claim.
func (p *Principal) Scopes() []string {
	return strings.Fields(p.Scope)
}

// HasScope reports whether the token carries the scope.
func (p *Principal) HasScope(scope string) bool {
	for _, s := range p.Scopes() {
		if s == scope {
			return true
		}
	}
	return false
}

// Actor is the audit identity: the subject, else the client id.
func (p *Principal) Actor() string {
	if p.Subject != "" {
		return p.Subject
	}
	return p.ClientID
}

type principalKey struct{}

// WithPrincipal attaches the principal and the audit actor to the context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	ctx = context.WithValue(ctx, principalKey{}, p)
	return actor.WithName(ctx, p.Actor())
}

// FromContext returns the request's principal, if the request was authenticated.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok && p != nil
}

// BearerToken returns the caller's raw bearer token for forwarding, or "" when there is none.
func BearerToken(ctx context.Context) string {
	if p, ok := FromContext(ctx); ok {
		return p.RawToken
	}
	return ""
}

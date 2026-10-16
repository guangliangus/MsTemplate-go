// Package auth validates the HS256 access tokens the platform's user service (user-svc) signs and
// decides which identity plane a request belongs to.
//
// Both planes (consumers and back-office operators) share one issuer and one key, and number their
// accounts independently: consumer 5 and back-office 5 are two people sharing an integer. The
// realm claim is the only signal telling them apart, so every protected route names the plane it
// serves with RequireRealm — a bare "authenticated" check would accept an operator's token on a
// consumer route and then read its sub as a consumer id. A token with no realm satisfies no plane.
//
// The signing key is shared: whoever holds it can mint a token any service accepts. It comes from
// the environment (a Kubernetes Secret), never from a checked-in file; the development overlay
// carries the throwaway key user-svc ships for local runs.
package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// Claim names as they travel on the wire; the issuer and every validator must agree on them.
const (
	// ClaimSubject is the account id within the numbering of the plane ClaimRealm names.
	ClaimSubject = "sub"
	// ClaimRealm says which plane's numbering the subject belongs to.
	ClaimRealm = "realm"
	// ClaimSessionID is the device session the token belongs to; absent for session-less tokens.
	ClaimSessionID = "sid"
	// ClaimScope is the space-separated scope list, as OAuth2 delivers it.
	ClaimScope = "scope"
	// ClaimClientID is a machine caller's client id; kept for the audit actor and a future issuer.
	ClaimClientID = "client_id"
)

// The two identity planes the user service issues tokens for.
const (
	// RealmConsumer is end users of the consumer app.
	RealmConsumer = "CONSUMER"
	// RealmBackOffice is operators of the back office.
	RealmBackOffice = "BACKOFFICE"
)

// Schemes name how a principal was established.
const (
	SchemeBearer    = "bearer"
	SchemeDevHeader = "dev-header"
)

// MinSigningKeyBytes is the floor for the HS256 key: a key shorter than the digest it produces is
// weaker than the algorithm's name promises, and the library accepts it without a word.
const MinSigningKeyBytes = 32

// Config is bound from the "auth" section.
type Config struct {
	// Issuer is the value user-svc writes into iss; any other issuer is refused before the signature
	// is even checked.
	Issuer string `koanf:"issuer" validate:"required"`
	// SigningKey is base64 of the HS256 key, at least 32 bytes. Required: the service refuses to
	// boot without it.
	SigningKey string `koanf:"signing_key" validate:"required"`
	// AdditionalSigningKeys are further base64 keys a token may be signed with, for a key rotation:
	// list the new key here before user-svc starts signing with it, swap the two once it does, and
	// drop the old one after the longest token lifetime has passed. No service has to switch at the
	// same moment as the issuer. Empty outside a rotation.
	AdditionalSigningKeys []string `koanf:"additional_signing_keys"`
	// Audience is optional and empty by default: user-svc tokens carry no aud — they prove who the
	// caller is, not which API they meant to reach — so a service that set one would refuse every
	// token. The knob stays for an audience-bound B2B issuer.
	Audience string `koanf:"audience"`
	// ClockSkew tolerates drift between the issuer and this host when checking exp and nbf.
	// 30 seconds matches user-svc's own validator.
	ClockSkew time.Duration `koanf:"clock_skew" validate:"min=0,max=5m"`
	// DevHeaders enables header-fabricated identities for local development (see DevHeaders).
	DevHeaders DevHeadersConfig `koanf:"dev_headers"`
}

// DevHeadersConfig is bound from "auth.dev_headers".
type DevHeadersConfig struct {
	// Enabled accepts X-Dev-Subject / X-Dev-Realm / X-Dev-Scopes instead of a token. Ignored outside
	// the development environment: the handler refuses there even when this is true, because a
	// switch that turns authentication off must not be reachable from a config map.
	Enabled bool `koanf:"enabled"`
}

// Validate applies the key length floor to every key.
func (c *Config) Validate() error {
	_, err := c.verificationKeys()
	return err
}

// verificationKeys decodes the signing key and the additional ones, in that order.
func (c *Config) verificationKeys() ([][]byte, error) {
	keys := make([][]byte, 0, 1+len(c.AdditionalSigningKeys))
	key, err := DecodeSigningKey(c.SigningKey)
	if err != nil {
		return nil, err
	}
	keys = append(keys, key)
	for i, encoded := range c.AdditionalSigningKeys {
		key, err := DecodeSigningKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("additional_signing_keys[%d]: %w", i, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// DecodeSigningKey decodes the base64 key and applies the floor.
func DecodeSigningKey(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("signing_key is required")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("signing_key is not valid base64: %w", err)
	}
	if len(key) < MinSigningKeyBytes {
		return nil, fmt.Errorf("signing_key must decode to at least %d bytes, got %d", MinSigningKeyBytes, len(key))
	}
	return key, nil
}

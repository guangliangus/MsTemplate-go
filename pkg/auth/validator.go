package auth

import (
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the token payload by its wire names.
type Claims struct {
	Realm     string `json:"realm,omitempty"`
	SessionID string `json:"sid,omitempty"`
	Scope     string `json:"scope,omitempty"`
	ClientID  string `json:"client_id,omitempty"`
	jwt.RegisteredClaims
}

// ErrInvalidToken is returned for any token the validator refuses; the reason is wrapped for logs
// but never shown to the caller.
var ErrInvalidToken = errors.New("invalid token")

// Validator checks tokens locally against the shared key — no network call per request. During a
// key rotation it accepts a token signed with any of the configured keys.
type Validator struct {
	key  any // []byte, or a jwt.VerificationKeySet while a rotation lists more than one key
	opts []jwt.ParserOption
}

// NewValidator builds a validator from the configuration.
func NewValidator(cfg Config) (*Validator, error) {
	keys, err := cfg.verificationKeys()
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	var key any = keys[0]
	if len(keys) > 1 {
		set := jwt.VerificationKeySet{}
		for _, k := range keys {
			set.Keys = append(set.Keys, k)
		}
		key = set
	}
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithLeeway(cfg.ClockSkew),
		jwt.WithExpirationRequired(),
	}
	if cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(cfg.Audience))
	}
	return &Validator{key: key, opts: opts}, nil
}

// Parse validates raw and returns its principal.
func (v *Validator) Parse(raw string) (*Principal, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(raw, claims, v.keyFunc, v.opts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if !token.Valid {
		return nil, ErrInvalidToken
	}
	if claims.Subject == "" && claims.ClientID == "" {
		return nil, fmt.Errorf("%w: neither sub nor client_id present", ErrInvalidToken)
	}
	return &Principal{
		Subject:   claims.Subject,
		Realm:     claims.Realm,
		SessionID: claims.SessionID,
		ClientID:  claims.ClientID,
		Scope:     claims.Scope,
		Scheme:    SchemeBearer,
		RawToken:  raw,
	}, nil
}

func (v *Validator) keyFunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, fmt.Errorf("unexpected signing method %q", token.Header["alg"])
	}
	return v.key, nil
}

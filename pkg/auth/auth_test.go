package auth

import (
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var testKey = base64.StdEncoding.EncodeToString([]byte("dev-only-access-token-key-32byte"))

func testConfig() Config {
	return Config{Issuer: "user-svc", SigningKey: testKey, ClockSkew: 30 * time.Second}
}

func mint(t *testing.T, key, issuer string, claims Claims, ttl time.Duration) string {
	t.Helper()
	claims.Issuer = issuer
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(ttl))
	claims.IssuedAt = jwt.NewNumericDate(time.Now())
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(raw)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestValidatorAcceptsUserSvcShapedTokens(t *testing.T) {
	v, err := NewValidator(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	claims := Claims{Realm: RealmConsumer, SessionID: "s1", Scope: "a b"}
	claims.Subject = "42"
	p, err := v.Parse(mint(t, testKey, "user-svc", claims, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "42" || p.Realm != RealmConsumer || !p.HasScope("b") || p.Scheme != SchemeBearer {
		t.Fatalf("principal %+v", p)
	}
}

func TestValidatorRefuses(t *testing.T) {
	v, _ := NewValidator(testConfig())
	other := base64.StdEncoding.EncodeToString([]byte("another-key-that-is-32-bytes-long!"))
	withSub := Claims{Realm: RealmConsumer}
	withSub.Subject = "1"

	cases := map[string]string{
		"wrong key":    mint(t, other, "user-svc", withSub, time.Hour),
		"wrong issuer": mint(t, testKey, "someone-else", withSub, time.Hour),
		"expired":      mint(t, testKey, "user-svc", withSub, -2*time.Minute),
		"no subject":   mint(t, testKey, "user-svc", Claims{Realm: RealmConsumer}, time.Hour),
		"garbage":      "not.a.token",
	}
	for name, token := range cases {
		if _, err := v.Parse(token); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewValidatorEnforcesKeyFloor(t *testing.T) {
	cfg := testConfig()
	cfg.SigningKey = base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := NewValidator(cfg); err == nil {
		t.Fatal("a 5-byte key must be refused")
	}
}

func protectedChain(v *Validator, dev *DevHeaders, realm string) http.Handler {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := FromContext(r.Context())
		w.Header().Set("X-Actor", p.Actor())
		w.WriteHeader(http.StatusOK)
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return Authenticate(v, dev, log)(RequireRealm(realm)(ok))
}

func TestMiddlewaresDecideByPlane(t *testing.T) {
	v, _ := NewValidator(testConfig())
	h := protectedChain(v, nil, RealmConsumer)
	consumer := Claims{Realm: RealmConsumer}
	consumer.Subject = "5"
	operator := Claims{Realm: RealmBackOffice}
	operator.Subject = "5"
	noRealm := Claims{}
	noRealm.Subject = "5"

	cases := []struct {
		name   string
		header string
		status int
	}{
		{"anonymous", "", 401},
		{"malformed scheme", "Basic abc", 401},
		{"bad token", "Bearer nope", 401},
		{"consumer", "Bearer " + mint(t, testKey, "user-svc", consumer, time.Hour), 200},
		{"operator on consumer route", "Bearer " + mint(t, testKey, "user-svc", operator, time.Hour), 403},
		{"no realm", "Bearer " + mint(t, testKey, "user-svc", noRealm, time.Hour), 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("got %d want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

func TestRequireScope(t *testing.T) {
	v, _ := NewValidator(testConfig())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Authenticate(v, nil, log)(RequireScope("orders.read")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	with := Claims{Realm: RealmBackOffice, Scope: "orders.read other"}
	with.Subject = "1"
	without := Claims{Realm: RealmBackOffice}
	without.Subject = "1"
	for token, want := range map[string]int{
		mint(t, testKey, "user-svc", with, time.Hour):    204,
		mint(t, testKey, "user-svc", without, time.Hour): 403,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("got %d want %d", rec.Code, want)
		}
	}
}

func TestDevHeadersOnlyInDevelopment(t *testing.T) {
	v, _ := NewValidator(testConfig())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(HeaderDevSubject, "9")
		r.Header.Set(HeaderDevRealm, RealmConsumer)
		return r
	}

	dev := NewDevHeaders(DevHeadersConfig{Enabled: true}, "development", log)
	rec := httptest.NewRecorder()
	protectedChain(v, dev, RealmConsumer).ServeHTTP(rec, req())
	if rec.Code != 200 || rec.Header().Get("X-Actor") != "9" {
		t.Fatalf("development: %d %s", rec.Code, rec.Body.String())
	}

	staging := NewDevHeaders(DevHeadersConfig{Enabled: true}, "staging", log)
	rec = httptest.NewRecorder()
	protectedChain(v, staging, RealmConsumer).ServeHTTP(rec, req())
	if rec.Code != 401 {
		t.Fatalf("outside development the headers must be ignored: %d", rec.Code)
	}

	missingRealm := httptest.NewRequest(http.MethodGet, "/", nil)
	missingRealm.Header.Set(HeaderDevSubject, "9")
	rec = httptest.NewRecorder()
	protectedChain(v, dev, RealmConsumer).ServeHTTP(rec, missingRealm)
	if rec.Code != 403 {
		t.Fatalf("a missing realm answers 403, not an accidental identity: %d", rec.Code)
	}
}

func TestValidatorAcceptsEveryKeyOfARotation(t *testing.T) {
	next := base64.StdEncoding.EncodeToString([]byte("the-next-key-user-svc-signs-with!"))
	cfg := testConfig()
	cfg.AdditionalSigningKeys = []string{next}
	v, err := NewValidator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	claims := Claims{Realm: RealmConsumer}
	claims.Subject = "7"
	for name, key := range map[string]string{"current": testKey, "next": next} {
		if _, err := v.Parse(mint(t, key, "user-svc", claims, time.Hour)); err != nil {
			t.Errorf("a token signed with the %s key is accepted: %v", name, err)
		}
	}
	stranger := base64.StdEncoding.EncodeToString([]byte("a-key-that-is-not-in-the-rotation"))
	if _, err := v.Parse(mint(t, stranger, "user-svc", claims, time.Hour)); err == nil {
		t.Fatal("a key outside the rotation is refused")
	}
	cfg.AdditionalSigningKeys = []string{"c2hvcnQ="}
	if err := cfg.Validate(); err == nil {
		t.Fatal("every additional key meets the length floor")
	}
}

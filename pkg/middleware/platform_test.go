package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/msvcservice/pkg/reqmeta"
)

func TestPlatformHeadersAndRequireHeaders(t *testing.T) {
	var captured string
	h := PlatformHeaders()(RequireHeaders("X-Platform", "X-Device-ID")(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			captured = reqmeta.Value(r.Context(), reqmeta.HeaderLanguage)
			w.WriteHeader(http.StatusNoContent)
		})))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Language", "ja")
	req.Header.Set("X-Platform", "ios")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	p := problemOf(t, rec)
	if rec.Code != http.StatusBadRequest || p.ErrorCode != "MISSING_HEADERS" || len(p.Errors["X-Device-ID"]) != 1 ||
		len(p.Errors["X-Platform"]) != 0 {
		t.Fatalf("missing headers are named: %d %+v", rec.Code, p)
	}

	req.Header.Set("X-Device-ID", "d-1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || captured != "ja" {
		t.Fatalf("complete request passes and the language is captured: %d %q", rec.Code, captured)
	}

	if RequireHeaders()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})) == nil {
		t.Fatal("no names is a no-op, not nil")
	}
}

func TestClusterInternalOnly(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := ClusterInternalOnly()(ok)
	cases := []struct {
		name   string
		remote string
		header string
		want   int
	}{
		{"private peer, no proxy", "10.1.2.3:5000", "", http.StatusNoContent},
		{"loopback", "127.0.0.1:5000", "", http.StatusNoContent},
		{"ipv6 link-local with zone", "[fe80::1%eth0]:5000", "", http.StatusNoContent},
		{"public peer", "203.0.113.9:5000", "", http.StatusForbidden},
		{"proxied request", "10.1.2.3:5000", "X-Forwarded-For", http.StatusForbidden},
		{"rfc 7239 forwarded", "10.1.2.3:5000", "Forwarded", http.StatusForbidden},
		{"unparsable peer fails closed", "nonsense", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/internal", nil)
			req.RemoteAddr = tc.remote
			if tc.header != "" {
				req.Header.Set(tc.header, "203.0.113.9")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusForbidden && problemOf(t, rec).ErrorCode != "FORBIDDEN" {
				t.Fatal("refusals speak the error contract")
			}
		})
	}
}

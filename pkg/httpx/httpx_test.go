package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/msvcservice/pkg/apperror"
)

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) Problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type %q", ct)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestErrorWriterMapsTypedErrors(t *testing.T) {
	ew := NewErrorWriter("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders/9", nil)
	rec := httptest.NewRecorder()
	ew.Write(rec, req, apperror.NotFound("Order", 9))

	p := decodeProblem(t, rec)
	if rec.Code != 404 || p.Status != 404 || p.ErrorCode != "NOT_FOUND" || p.Title != "Not Found" {
		t.Fatalf("unexpected problem %+v", p)
	}
	if p.Type != "urn:msvc:error:not-found" || p.Instance != "/api/v1/orders/9" {
		t.Fatalf("type/instance wrong: %+v", p)
	}
}

func TestErrorWriterHeadersAndFallbacks(t *testing.T) {
	ew := NewErrorWriter("https://errors.example.com/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	rec := httptest.NewRecorder()
	ew.Write(rec, req, apperror.RateLimited("slow down", 1500*time.Millisecond))
	if rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("Retry-After rounds up: %q", rec.Header().Get("Retry-After"))
	}

	rec = httptest.NewRecorder()
	ew.Write(rec, req, apperror.Unauthorized("no"))
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatal("401 must name the scheme")
	}

	rec = httptest.NewRecorder()
	ew.Write(rec, req, context.DeadlineExceeded)
	if p := decodeProblem(t, rec); p.Status != 504 || p.ErrorCode != "REQUEST_TIMEOUT" {
		t.Fatalf("deadline → 504: %+v", p)
	}

	rec = httptest.NewRecorder()
	ew.Write(rec, req, &http.MaxBytesError{Limit: 10})
	if p := decodeProblem(t, rec); p.Status != 413 || p.ErrorCode != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("too large → 413: %+v", p)
	}

	rec = httptest.NewRecorder()
	ew.Write(rec, req, errors.New("secret internals"))
	p := decodeProblem(t, rec)
	if p.Status != 500 || p.ErrorCode != "INTERNAL_ERROR" || strings.Contains(p.Detail, "secret") {
		t.Fatalf("untyped errors are generic 500s: %+v", p)
	}
	if p.Type != "https://errors.example.com/internal-error" {
		t.Fatalf("type base is configurable: %s", p.Type)
	}
}

func TestMappersRunFirstAndCustomizersLast(t *testing.T) {
	ew := NewErrorWriter("", nil)
	sentinel := errors.New("domain thing")
	ew.Mappers = append(ew.Mappers, func(_ *http.Request, err error) *apperror.Error {
		if errors.Is(err, sentinel) {
			return apperror.DomainRule("THING_BROKEN", "thing")
		}
		return nil
	})
	ew.Customizers = append(ew.Customizers, func(_ *http.Request, p *Problem) { p.Detail = "translated: " + p.ErrorCode })

	rec := httptest.NewRecorder()
	ew.Write(rec, httptest.NewRequest(http.MethodGet, "/", nil), sentinel)
	p := decodeProblem(t, rec)
	if p.Status != 422 || p.ErrorCode != "THING_BROKEN" || p.Detail != "translated: THING_BROKEN" {
		t.Fatalf("%+v", p)
	}
}

func TestHandlerFuncWritesErrors(t *testing.T) {
	h := HandlerFunc(func(http.ResponseWriter, *http.Request) error { return apperror.Forbidden("nope") })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 403 {
		t.Fatalf("status %d", rec.Code)
	}
}

type body struct {
	Name  string `json:"name" validate:"required,max=5"`
	Count int    `json:"count" validate:"min=1"`
}

func TestDecodeJSON(t *testing.T) {
	cases := []struct {
		name, payload, contentType, code string
		status                           int
		field                            string
	}{
		{"ok", `{"name":"ab","count":2}`, "application/json", "", 200, ""},
		{"malformed", `{"name":`, "application/json", "BAD_REQUEST", 400, ""},
		{"empty", ``, "application/json", "BAD_REQUEST", 400, ""},
		{"wrong type", `{"name":"ab","count":"two"}`, "application/json", "VALIDATION_FAILED", 400, "count"},
		{"validation", `{"name":"toolong","count":0}`, "application/json", "VALIDATION_FAILED", 400, "name"},
		{"media type", `{"name":"ab","count":2}`, "text/plain", "UNSUPPORTED_MEDIA_TYPE", 415, ""},
		{"two documents", `{"name":"ab","count":2}{}`, "application/json", "BAD_REQUEST", 400, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.payload))
			req.Header.Set("Content-Type", tc.contentType)
			var b body
			err := DecodeJSON(req, &b)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			e, ok := apperror.As(err)
			if !ok || e.Code != tc.code || e.Status != tc.status {
				t.Fatalf("got %v, want %s/%d", err, tc.code, tc.status)
			}
			if tc.field != "" {
				if _, present := e.Fields[tc.field]; !present {
					t.Fatalf("field %q missing from %v", tc.field, e.Fields)
				}
			}
		})
	}
}

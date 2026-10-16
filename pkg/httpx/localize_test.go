package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/msvcservice/pkg/apperror"
)

type fakeTranslator map[string]string

func (f fakeTranslator) Translate(_ *http.Request, code string) (string, bool) {
	msg, ok := f[code]
	return msg, ok
}

func TestLocalizeAndProblemRecorder(t *testing.T) {
	ew := NewErrorWriter("", nil)
	ew.Customizers = append(ew.Customizers, Localize(fakeTranslator{"NOT_FOUND": "找不到資料"}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(WithProblemRecorder(req.Context()))
	rec := httptest.NewRecorder()
	ew.Write(rec, req, apperror.NotFound("Order", 1))
	var p Problem
	decodeBody(t, rec, &p)
	if p.Title != "找不到資料" || p.Detail != "Order '1' was not found." || p.ErrorCode != "NOT_FOUND" {
		t.Fatalf("title localized, detail kept: %+v", p)
	}
	if got := RecordedProblem(req.Context()); got != "NOT_FOUND" {
		t.Fatalf("recorded %q", got)
	}

	// No catalogue entry: the status title stands.
	rec = httptest.NewRecorder()
	ew.Write(rec, req, errors.New("boom"))
	decodeBody(t, rec, &p)
	if p.Title != "Internal Server Error" || RecordedProblem(req.Context()) != "INTERNAL_ERROR" {
		t.Fatalf("%+v", p)
	}
	if RecordedProblem(httptest.NewRequest(http.MethodGet, "/", nil).Context()) != "" {
		t.Fatal("no recorder, nothing recorded")
	}
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
}

// Package httpx is the HTTP contract shared by every handler: errors become RFC 9457 problem
// details with the platform's errorCode and traceId members (never an envelope around success
// responses), JSON bodies are decoded and validated one way, and handlers return errors instead
// of writing them.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"example.com/msvcservice/pkg/apperror"
)

// DefaultTypeBaseURI prefixes the problem type URI; the lowercase kebab-case error code is appended
// (urn:msvc:error:not-found). A URN so nothing has to be hosted; point it at an https error
// catalogue if the team publishes one.
const DefaultTypeBaseURI = "urn:msvc:error:"

// Problem is an RFC 9457 body with the platform's extension members.
type Problem struct {
	Type      string              `json:"type"`
	Title     string              `json:"title"`
	Status    int                 `json:"status"`
	Detail    string              `json:"detail,omitempty"`
	Instance  string              `json:"instance,omitempty"`
	ErrorCode string              `json:"errorCode"`
	TraceID   string              `json:"traceId,omitempty"`
	Errors    map[string][]string `json:"errors,omitempty"`
}

// Mapper turns a service's own error types into the platform's *apperror.Error; nil means "not
// mine". Mappers run before the built-in mapping, first non-nil answer wins. A domain package that
// imports nothing from the platform can still have its errors mapped here.
type Mapper func(r *http.Request, err error) *apperror.Error

// Customizer is a last touch on every problem this host writes, after errorCode, title, type,
// traceId and instance are final — translating detail is the canonical use.
type Customizer func(r *http.Request, p *Problem)

// Translator localizes error codes for a request; pkg/i18n's Catalog implements it.
type Translator interface {
	// Translate returns the caller's-language message for an error code, false when there is none.
	Translate(r *http.Request, code string) (string, bool)
}

// Localize is the Customizer that puts the caller's language on the problem: the title becomes the
// catalogue's message for the errorCode when it has one (RFC 9457 makes title the per-type summary,
// which is what one message per code is), the detail stays the developer-facing sentence.
func Localize(t Translator) Customizer {
	return func(r *http.Request, p *Problem) {
		if msg, ok := t.Translate(r, p.ErrorCode); ok {
			p.Title = msg
		}
	}
}

type problemRecorder struct{ code string }

type recorderKey struct{}

// WithProblemRecorder lets an outer middleware learn which problem, if any, was written during the
// request: the access log wants the errorCode beside the status, and the problems counter is
// keyed by it. The recorder is filled by WriteError, so it sees what the client received.
func WithProblemRecorder(ctx context.Context) context.Context {
	return context.WithValue(ctx, recorderKey{}, &problemRecorder{})
}

// RecordedProblem returns the errorCode of the problem written during the request, or "".
func RecordedProblem(ctx context.Context) string {
	if rec, ok := ctx.Value(recorderKey{}).(*problemRecorder); ok {
		return rec.code
	}
	return ""
}

// ErrorWriter turns errors into problem details responses.
type ErrorWriter struct {
	TypeBaseURI string
	Log         *slog.Logger
	Mappers     []Mapper
	Customizers []Customizer
}

// NewErrorWriter builds a writer; an empty typeBaseURI means DefaultTypeBaseURI.
func NewErrorWriter(typeBaseURI string, log *slog.Logger) *ErrorWriter {
	if typeBaseURI == "" {
		typeBaseURI = DefaultTypeBaseURI
	}
	if log == nil {
		log = slog.Default()
	}
	return &ErrorWriter{TypeBaseURI: typeBaseURI, Log: log}
}

// Map resolves any error to the typed error the response is built from.
func (ew *ErrorWriter) Map(r *http.Request, err error) *apperror.Error {
	for _, m := range ew.Mappers {
		if e := m(r, err); e != nil {
			return e
		}
	}
	if e, ok := apperror.As(err); ok {
		return e
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return apperror.Timeout("The request did not complete within the server's time limit.").WithCause(err)
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		return apperror.New(apperror.CodeClientClosedRequest, apperror.StatusClientClosedRequest,
			"The client closed the request before a response was written.").WithCause(err)
	case errors.As(err, &tooLarge):
		return apperror.New(apperror.CodePayloadTooLarge, http.StatusRequestEntityTooLarge,
			"The request body exceeds "+strconv.FormatInt(tooLarge.Limit, 10)+" bytes.").WithCause(err)
	default:
		return apperror.Internal(err)
	}
}

// Write maps err and writes the problem. Server faults are logged with their cause here, once,
// keyed by the trace id the client also received.
func (ew *ErrorWriter) Write(w http.ResponseWriter, r *http.Request, err error) {
	e := ew.Map(r, err)
	if e.Status >= http.StatusInternalServerError {
		ew.Log.ErrorContext(r.Context(), "request failed",
			slog.String("errorCode", e.Code),
			slog.Int("status", e.Status),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Any("error", err))
	}
	ew.WriteError(w, r, e)
}

// WriteError writes a typed error as a problem.
func (ew *ErrorWriter) WriteError(w http.ResponseWriter, r *http.Request, e *apperror.Error) {
	p := Problem{
		Type:      TypeURI(ew.TypeBaseURI, e.Code),
		Title:     StatusTitle(e.Status),
		Status:    e.Status,
		Detail:    e.Message,
		Instance:  r.URL.Path,
		ErrorCode: e.Code,
		TraceID:   TraceID(r.Context()),
		Errors:    e.Fields,
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	for _, c := range ew.Customizers {
		c(r, &p)
	}
	if rec, ok := r.Context().Value(recorderKey{}).(*problemRecorder); ok {
		rec.code = e.Code
	}
	writeProblem(w, p)
}

// WriteStatus writes a framework-produced problem (a router 404, a 405) whose code derives from
// the status, so clients never have to branch on the status alone.
func (ew *ErrorWriter) WriteStatus(w http.ResponseWriter, r *http.Request, status int, detail string) {
	ew.WriteError(w, r, apperror.New(apperror.CodeForStatus(status), status, detail))
}

func writeProblem(w http.ResponseWriter, p Problem) {
	body, err := json.Marshal(p)
	if err != nil {
		http.Error(w, `{"errorCode":"INTERNAL_ERROR"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// TypeURI appends the kebab-case code to the base: ("urn:msvc:error:", "NOT_FOUND") ->
// "urn:msvc:error:not-found".
func TypeURI(base, code string) string {
	return base + strings.ToLower(strings.ReplaceAll(code, "_", "-"))
}

// StatusTitle is the default title per status; a Localize customizer replaces it with the caller's
// language's message for the errorCode.
func StatusTitle(status int) string {
	if status == apperror.StatusClientClosedRequest {
		return "Client Closed Request"
	}
	if title := http.StatusText(status); title != "" {
		return title
	}
	return "Error"
}

// TraceID is the bare 32-hex trace id of the context's span, or "" outside a trace.
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

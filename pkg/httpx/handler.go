package httpx

import (
	"context"
	"net/http"
)

// HandlerFunc is a handler that returns its failure instead of writing it; the error writer in
// the request context turns it into a problem. Return before writing any part of the response.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// ServeHTTP runs the handler and writes any error as a problem.
func (f HandlerFunc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := f(w, r); err != nil {
		Error(w, r, err)
	}
}

// Handle adapts a HandlerFunc for routers that want an http.HandlerFunc.
func Handle(f HandlerFunc) http.HandlerFunc {
	return f.ServeHTTP
}

type writerKey struct{}

// WithErrorWriter attaches the host's error writer to the context so every handler and middleware
// writes problems with the same type base, mappers and customizers.
func WithErrorWriter(ctx context.Context, ew *ErrorWriter) context.Context {
	return context.WithValue(ctx, writerKey{}, ew)
}

var fallbackWriter = NewErrorWriter("", nil)

// ErrorWriterFrom returns the context's error writer, or a default one.
func ErrorWriterFrom(ctx context.Context) *ErrorWriter {
	if ew, ok := ctx.Value(writerKey{}).(*ErrorWriter); ok {
		return ew
	}
	return fallbackWriter
}

// Error writes err as a problem using the request's error writer.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	ErrorWriterFrom(r.Context()).Write(w, r, err)
}

// ErrorStatus writes a framework-produced problem for a bare status.
func ErrorStatus(w http.ResponseWriter, r *http.Request, status int, detail string) {
	ErrorWriterFrom(r.Context()).WriteStatus(w, r, status, detail)
}

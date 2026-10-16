package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"example.com/msvcservice/pkg/apperror"
)

// DecodeJSON reads a JSON body into v and validates it. Malformed JSON is a 400 BAD_REQUEST, a
// wrong field type is a 400 VALIDATION_FAILED naming the field, a wrong content type a 415, and a
// body over the middleware's limit a 413. Unknown fields are ignored (tolerant reader: a client may send newer fields).
func DecodeJSON(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		media, _, err := mime.ParseMediaType(ct)
		if err != nil || media != "application/json" {
			return apperror.New(apperror.CodeUnsupportedMediaType, http.StatusUnsupportedMediaType,
				"The request body must be application/json.")
		}
	}

	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return decodeError(err)
	}
	if dec.More() {
		return apperror.BadRequest("The request body must contain a single JSON document.")
	}
	return Validate(v)
}

func decodeError(err error) error {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return err
	case errors.Is(err, io.EOF):
		return apperror.BadRequest("The request body is required.")
	case errors.As(err, &syntax):
		return apperror.BadRequest("The request body is not valid JSON (offset " +
			strconv.FormatInt(syntax.Offset, 10) + ").")
	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return apperror.Validation(map[string][]string{
			field: {fmt.Sprintf("must be a %s", typeErr.Type)},
		})
	default:
		return apperror.BadRequest("The request body could not be read.").WithCause(err)
	}
}

// WriteJSON writes v with the status; the body is marshalled first so a failure never leaves a
// half-written 200 behind.
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("httpx: marshal response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(body)
	return err
}

// Created writes a 201 with a Location header.
func Created(w http.ResponseWriter, location string, v any) error {
	w.Header().Set("Location", location)
	return WriteJSON(w, http.StatusCreated, v)
}

// NoContent writes a 204.
func NoContent(w http.ResponseWriter) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ParamInt reads a positive integer route parameter; anything else is a 400.
func ParamInt(r *http.Request, name string) (int, error) {
	raw := chi.URLParam(r, name)
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, apperror.BadRequest(fmt.Sprintf("The '%s' route parameter must be a positive integer.", name))
	}
	return n, nil
}

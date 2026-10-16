package postgres

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func pgErr(code string) error {
	return &pgconn.PgError{Code: code, Message: "x"}
}

func TestClassification(t *testing.T) {
	if !IsUniqueViolation(pgErr("23505")) || IsUniqueViolation(pgErr("23503")) {
		t.Fatal("unique violation")
	}
	if !IsSerializationFailure(pgErr("40001")) || !IsSerializationFailure(pgErr("40P01")) {
		t.Fatal("serialization")
	}
	for _, transient := range []error{pgErr("40001"), pgErr("08006"), pgErr("57P01"), pgErr("53300"),
		&net.OpError{Op: "read", Err: errors.New("reset")}} {
		if !IsTransient(transient) {
			t.Errorf("%v should be transient", transient)
		}
	}
	for _, permanent := range []error{nil, pgErr("23505"), pgErr("42P01"), context.Canceled, context.DeadlineExceeded,
		errors.New("plain")} {
		if IsTransient(permanent) {
			t.Errorf("%v should not be transient", permanent)
		}
	}
}

func TestErrorMapper(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if e := ErrorMapper(req, pgErr("23505")); e == nil || e.Status != 409 || e.Code != "UNIQUE_VIOLATION" {
		t.Fatalf("%+v", e)
	}
	if e := ErrorMapper(req, pgErr("40001")); e == nil || e.Code != "CONCURRENCY_CONFLICT" {
		t.Fatalf("%+v", e)
	}
	if e := ErrorMapper(req, pgErr("57014")); e == nil || e.Status != 504 {
		t.Fatalf("%+v", e)
	}
	if e := ErrorMapper(req, errors.New("other")); e != nil {
		t.Fatal("unrelated errors are not mapped")
	}
}

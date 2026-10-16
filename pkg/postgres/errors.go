package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"example.com/msvcservice/pkg/apperror"
)

// SQLSTATE codes the classification below keys on.
const (
	stateUniqueViolation      = "23505"
	stateSerializationFailure = "40001"
	stateDeadlockDetected     = "40P01"
	stateQueryCanceled        = "57014"
	stateAdminShutdown        = "57P01"
	stateCrashShutdown        = "57P02"
	stateCannotConnectNow     = "57P03"
	stateTooManyConnections   = "53300"
	stateLockNotAvailable     = "55P03"
	classConnectionException  = "08"
)

// SQLState returns the error's SQLSTATE, or "" for anything that is not a server error.
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// IsUniqueViolation reports a unique constraint refusal (23505).
func IsUniqueViolation(err error) bool {
	return SQLState(err) == stateUniqueViolation
}

// IsSerializationFailure reports a serialization failure or deadlock, which a retry of the whole
// transaction is expected to resolve.
func IsSerializationFailure(err error) bool {
	code := SQLState(err)
	return code == stateSerializationFailure || code == stateDeadlockDetected
}

// IsTransient reports a failure worth another attempt: the connection died, the server is
// starting or shutting down, the pool is exhausted, a lock was briefly unavailable. It is NOT a
// statement about whether the operation is safe to repeat — a unique violation (never transient)
// will never succeed on retry, and a bare INSERT that timed out may already have committed.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch code := SQLState(err); {
	case code == "":
		// below: no server error, inspect the transport
	case code == stateSerializationFailure, code == stateDeadlockDetected,
		code == stateAdminShutdown, code == stateCrashShutdown, code == stateCannotConnectNow,
		code == stateTooManyConnections, code == stateLockNotAvailable:
		return true
	case len(code) >= 2 && code[:2] == classConnectionException:
		return true
	default:
		return false
	}
	if pgconn.SafeToRetry(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// ErrorMapper translates database errors for the HTTP layer: a unique violation is a 409
// UNIQUE_VIOLATION, a serialization failure that survived the retries is a 409
// CONCURRENCY_CONFLICT, and a query cancelled by statement_timeout is a 504.
func ErrorMapper(_ *http.Request, err error) *apperror.Error {
	switch SQLState(err) {
	case stateUniqueViolation:
		return apperror.New(apperror.CodeUniqueViolation, http.StatusConflict,
			"A record with the same unique value already exists.").WithCause(err)
	case stateSerializationFailure, stateDeadlockDetected:
		return apperror.ConcurrencyConflict().WithCause(err)
	case stateQueryCanceled:
		return apperror.Timeout("A database statement exceeded its time limit.").WithCause(err)
	default:
		return nil
	}
}

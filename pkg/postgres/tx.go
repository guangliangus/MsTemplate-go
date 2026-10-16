package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the subset of pgxpool.Pool and pgx.Tx every repository uses, so one method body
// runs on the pool or inside the caller's transaction depending on the context.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{}

// txState is what a transaction carries on the context: the transaction itself and the work queued
// for after its commit. A pgx.Tx is never shared between goroutines, so neither is this.
type txState struct {
	tx          pgx.Tx
	afterCommit []func(ctx context.Context)
}

func stateFromContext(ctx context.Context) (*txState, bool) {
	st, ok := ctx.Value(txKey{}).(*txState)
	return st, ok && st != nil && st.tx != nil
}

// TxFromContext returns the transaction carried by ctx, if any.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	if st, ok := stateFromContext(ctx); ok {
		return st.tx, true
	}
	return nil, false
}

// AfterCommit runs fn once the transaction carried by ctx has committed — never when it rolls
// back — or at once when ctx carries no transaction. It is for side effects outside the database
// that must not run ahead of the commit: a cache entry invalidated before the commit lets a
// concurrent reader load the old row and put it straight back for a whole TTL. fn gets a context
// that a cancellation after the commit does not reach.
func (db *DB) AfterCommit(ctx context.Context, fn func(ctx context.Context)) {
	if st, ok := stateFromContext(ctx); ok {
		st.afterCommit = append(st.afterCommit, fn)
		return
	}
	fn(ctx)
}

// Q returns the transaction on ctx when there is one, else the pool. Every repository method
// starts with this, which is what makes WithTx compose across packages.
func (db *DB) Q(ctx context.Context) Querier {
	if tx, ok := TxFromContext(ctx); ok {
		return tx
	}
	return db.Pool
}

// WithTx runs fn inside a transaction carried on the context it receives; commit on nil, rollback
// on error or panic. A call already inside a transaction joins it (no savepoint), so a use case
// can call a repository helper that itself uses WithTx; AfterCommit work registered anywhere inside
// runs once the outermost transaction commits.
func (db *DB) WithTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, ok := TxFromContext(ctx); ok {
		return fn(ctx)
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("postgres: rollback: %w", rbErr))
			}
		}
	}()
	st := &txState{tx: tx}
	if err = fn(context.WithValue(ctx, txKey{}, st)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	after := context.WithoutCancel(ctx)
	for _, hook := range st.afterCommit {
		hook(after)
	}
	return nil
}

// DefaultTxRetries is the attempt budget of WithTxRetry.
const DefaultTxRetries = 3

// WithTxRetry is WithTx plus a retry of the WHOLE function on a serialization failure or deadlock
// (SQLSTATE 40001 / 40P01), which is what those errors ask for. fn runs from the top on each
// attempt, reads included, so everything it does must be safe to repeat. A commit that reached
// the server before the connection dropped is indistinguishable from one that never did, so work
// that must not be applied twice still needs its own idempotency key.
func (db *DB) WithTxRetry(ctx context.Context, fn func(ctx context.Context) error) error {
	var err error
	for attempt := 1; attempt <= DefaultTxRetries; attempt++ {
		err = db.WithTx(ctx, fn)
		if err == nil || !IsSerializationFailure(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

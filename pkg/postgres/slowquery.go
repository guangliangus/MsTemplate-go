package postgres

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/time/rate"
)

// maxLoggedSQLBytes caps the statement text in a slow-query line: one IN list can run to hundreds
// of kilobytes, and the moment every statement is slow is the moment the log pipeline can least
// afford it.
const maxLoggedSQLBytes = 2000

// slowQueryTracer logs statements slower than the threshold with their text but never their
// arguments (user ids, phone numbers and card fragments travel in WHERE clauses, and the log
// platform is read by more people than the database), under a per-pod rate limit. Past the limit
// the statements are still counted, only the lines are dropped: when the pool saturates every
// statement is slow, and logging each one would make the log part of the outage.
//
// Elapsed starts when the statement starts on an acquired connection: waiting for a free
// connection is NOT in it; that is visible in the pool's pgxpool_empty_acquire metrics instead.
type slowQueryTracer struct {
	threshold time.Duration
	limiter   *rate.Limiter // nil means unlimited
	log       *slog.Logger
}

type slowQueryKey struct{}

type slowQueryStart struct {
	at  time.Time
	sql string
}

var slowQueries = sync.OnceValue(func() metric.Int64Counter {
	c, _ := otel.Meter("msvc/postgres").Int64Counter("msvc.db.slow_queries",
		metric.WithDescription("Statements slower than postgres.slow_query_threshold, by whether a log line "+
			"was written (logged=false: the line was suppressed by slow_query_log_rate)."))
	return c
})

func newSlowQueryTracer(threshold time.Duration, perSecond int, log *slog.Logger) *slowQueryTracer {
	t := &slowQueryTracer{threshold: threshold, log: log}
	if perSecond > 0 {
		t.limiter = rate.NewLimiter(rate.Limit(perSecond), perSecond)
	}
	return t
}

// TraceQueryStart implements pgx.QueryTracer.
func (t *slowQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData,
) context.Context {
	return context.WithValue(ctx, slowQueryKey{}, slowQueryStart{at: time.Now(), sql: data.SQL})
}

// TraceQueryEnd implements pgx.QueryTracer.
func (t *slowQueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	start, ok := ctx.Value(slowQueryKey{}).(slowQueryStart)
	if !ok {
		return
	}
	elapsed := time.Since(start.at)
	if elapsed < t.threshold {
		return
	}
	if t.limiter != nil && !t.limiter.Allow() {
		slowQueries().Add(ctx, 1, metric.WithAttributes(attribute.Bool("logged", false)))
		return
	}
	slowQueries().Add(ctx, 1, metric.WithAttributes(attribute.Bool("logged", true)))
	attrs := []slog.Attr{
		slog.Duration("elapsed", elapsed),
		slog.String("sql", truncateSQL(start.sql, maxLoggedSQLBytes)),
	}
	if data.Err != nil {
		attrs = append(attrs, slog.Any("error", data.Err))
	} else {
		attrs = append(attrs, slog.String("command_tag", data.CommandTag.String()))
	}
	t.log.LogAttrs(ctx, slog.LevelWarn, "slow query", attrs...)
}

func truncateSQL(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

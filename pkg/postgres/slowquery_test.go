package postgres

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSlowQueryTracerLogsOverThresholdWithoutArgumentsAndRateLimits(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	tr := newSlowQueryTracer(time.Millisecond, 1, log) // one line per second
	ctx := context.Background()

	run := func(sql string, sleep time.Duration) {
		c := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: sql, Args: []any{"secret-arg"}})
		time.Sleep(sleep)
		tr.TraceQueryEnd(c, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
	}
	run("SELECT 1 WHERE x = $1", 0) // fast: nothing
	run("SELECT 2 WHERE x = $1", 3*time.Millisecond)
	run("SELECT 3 WHERE x = $1", 3*time.Millisecond) // suppressed by the rate limit

	out := buf.String()
	if strings.Count(out, "slow query") != 1 {
		t.Fatalf("one line expected (fast query skipped, second slow one rate limited):\n%s", out)
	}
	if !strings.Contains(out, "SELECT 2 WHERE x = $1") || strings.Contains(out, "secret-arg") {
		t.Fatalf("statement text logged, arguments never:\n%s", out)
	}
	if got := truncateSQL(strings.Repeat("x", 10), 4); got != "xxxx…" {
		t.Fatalf("truncate: %q", got)
	}
}

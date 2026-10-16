// Package postgres opens the pgx pool with the team's conventions and carries transactions on the
// context, so a use case can write a business row, an outbox event and a task row in one commit
// without any of the packages knowing about each other.
//
// The schema is never touched here: db/*.sql is applied by hand, DDL first and code second.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is bound from the "postgres" section.
type Config struct {
	// DSN is a libpq URI or key=value string; it arrives from a Secret in every deployed environment.
	DSN string `koanf:"dsn" validate:"required"`
	// MaxConns caps the pool; the database's max_connections budget is shared by every replica.
	MaxConns int32 `koanf:"max_conns" validate:"min=0"`
	// MinConns keeps warm connections for the first requests after a scale-out.
	MinConns int32 `koanf:"min_conns" validate:"min=0"`
	// MaxConnLifetime recycles connections so a failover or a config change reaches the pool.
	MaxConnLifetime time.Duration `koanf:"max_conn_lifetime" validate:"min=0"`
	// MaxConnIdleTime releases idle connections back to the server.
	MaxConnIdleTime time.Duration `koanf:"max_conn_idle_time" validate:"min=0"`
	// HealthCheckPeriod is how often the pool probes idle connections.
	HealthCheckPeriod time.Duration `koanf:"health_check_period" validate:"min=0"`
	// ConnectTimeout bounds establishing one connection.
	ConnectTimeout time.Duration `koanf:"connect_timeout" validate:"min=0"`
	// StatementTimeout is set server-side on every session: a runaway query is cancelled by the
	// database even when the application forgot its context deadline.
	StatementTimeout time.Duration `koanf:"statement_timeout" validate:"min=0"`
	// IdleInTransactionTimeout is set server-side on every session: a transaction left open while the
	// application waits on something else (a broker, a downstream, a bug) is ended by the database
	// instead of holding its row locks and the vacuum horizon indefinitely. 0 disables it. A
	// transaction must never wait on anything slow; work that does belongs outside it.
	IdleInTransactionTimeout time.Duration `koanf:"idle_in_transaction_timeout" validate:"min=0"`
	// SlowQueryThreshold logs statements slower than this, text only, never arguments; 0 disables.
	// Independent of the log level: slow statements are the one SQL signal worth having on all the
	// time, and the full statement log that comes with a debug level is not.
	SlowQueryThreshold time.Duration `koanf:"slow_query_threshold" validate:"min=0"`
	// SlowQueryLogRate caps slow-query lines per second per pod; 0 is unlimited. Past the cap the
	// statements are still counted (msvc_db_slow_queries_total{logged="false"}), only the lines are
	// dropped: when the pool saturates every statement is slow, and a line per statement would make
	// the log itself part of the outage.
	SlowQueryLogRate int `koanf:"slow_query_log_rate" validate:"min=0"`
}

// DB is the pool plus the transaction-on-context helpers.
type DB struct {
	Pool *pgxpool.Pool
	log  *slog.Logger
}

// Open parses the configuration and creates the pool. It does not require the database to be
// reachable: a pod whose database is in maintenance starts, fails readiness and keeps retrying,
// instead of crash-looping. A malformed DSN is still a startup error.
func Open(ctx context.Context, cfg Config, appName string, log *slog.Logger) (*DB, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	// Only configured values override pgxpool's own defaults; a zero would otherwise disable a
	// pool feature (and a zero health-check period makes the pool panic).
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.HealthCheckPeriod > 0 {
		pc.HealthCheckPeriod = cfg.HealthCheckPeriod
	}
	if cfg.ConnectTimeout > 0 {
		pc.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}
	if pc.ConnConfig.RuntimeParams == nil {
		pc.ConnConfig.RuntimeParams = map[string]string{}
	}
	pc.ConnConfig.RuntimeParams["application_name"] = appName
	pc.ConnConfig.RuntimeParams["timezone"] = "UTC"
	if cfg.StatementTimeout > 0 {
		pc.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	}
	if cfg.IdleInTransactionTimeout > 0 {
		pc.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] =
			strconv.FormatInt(cfg.IdleInTransactionTimeout.Milliseconds(), 10)
	}
	tracers := []pgx.QueryTracer{otelpgx.NewTracer()}
	if cfg.SlowQueryThreshold > 0 {
		tracers = append(tracers, newSlowQueryTracer(cfg.SlowQueryThreshold, cfg.SlowQueryLogRate, log))
	}
	pc.ConnConfig.Tracer = multitracer.New(tracers...)

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	db := &DB{Pool: pool, log: log}
	// Pool gauges and counters, exported as pgxpool_* (acquired, idle, max and total connections,
	// empty_acquire = acquires that had to wait for a free connection, and the time they waited).
	// A pod whose acquired count sits at max while empty_acquire climbs is queueing on its own pool,
	// not on PostgreSQL; the fix is max_conns or fewer concurrent requests, not a faster query.
	if err := otelpgx.RecordStats(pool); err != nil {
		log.Warn("postgres pool metrics not registered", slog.Any("error", err))
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := db.Ping(pingCtx); err != nil {
		log.Warn("postgres is not reachable yet; readiness will report it until it is", slog.Any("error", err))
	}
	return db, nil
}

// Ping checks a connection can be acquired and answers; it doubles as the readiness check.
func (db *DB) Ping(ctx context.Context) error {
	return db.Pool.Ping(ctx)
}

// Close releases the pool.
func (db *DB) Close() {
	db.Pool.Close()
}

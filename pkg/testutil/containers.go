// Package testutil starts the real dependencies for integration tests with Testcontainers and
// applies the service's db/*.sql, so a test runs against the schema the DBA will apply — never
// against a schema derived from the code.
//
// Tests that need Docker call Stack(t) and are skipped when no daemon answers, unless CI=true,
// in which case a missing daemon is a failure: a suite that silently skipped itself proves
// nothing, and a green run must mean the tests ran.
package testutil

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Images pinned for the suite; keep them in step with docker-compose.yml.
const (
	PostgresImage = "postgres:18-alpine"
	RedisImage    = "redis:8-alpine"
	RabbitMQImage = "rabbitmq:4-management"
)

// Stack is the set of running dependencies.
type Stack struct {
	PostgresDSN string
	RedisAddr   string
	AmqpURL     string
}

// RequireDocker skips the test without Docker locally and fails it in CI.
func RequireDocker(t *testing.T) {
	t.Helper()
	provider, err := testcontainers.NewDockerProvider()
	if err == nil {
		_, err = provider.DaemonHost(context.Background())
	}
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("docker is required in CI: %v", err)
		}
		t.Skipf("docker not available: %v", err)
	}
}

// Postgres starts PostgreSQL, applies db/*.sql in order and returns the DSN.
func Postgres(t *testing.T) string {
	t.Helper()
	RequireDocker(t)
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, PostgresImage,
		tcpostgres.WithDatabase("msvcservice"),
		tcpostgres.WithUsername("msvc"),
		tcpostgres.WithPassword("msvc"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	ApplySchema(t, dsn)
	return dsn
}

// ApplySchema runs every db/*.sql script, in name order, stopping at the first error.
func ApplySchema(t *testing.T, dsn string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot(t), "db", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no db/*.sql scripts found: %v", err)
	}
	sort.Strings(files)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, file := range files {
		script, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(script)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(file), err)
		}
	}
}

// Redis starts Redis and returns host:port.
func Redis(t *testing.T) string {
	t.Helper()
	RequireDocker(t)
	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, RedisImage)
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	endpoint, err := ctr.Endpoint(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

// RabbitMQ starts the broker and returns the AMQP URL.
func RabbitMQ(t *testing.T) string {
	t.Helper()
	RequireDocker(t)
	ctx := context.Background()
	ctr, err := tcrabbitmq.Run(ctx, RabbitMQImage)
	if err != nil {
		t.Fatalf("start rabbitmq: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	amqpURL, err := ctr.AmqpURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return amqpURL
}

// All starts the three dependencies.
func All(t *testing.T) Stack {
	t.Helper()
	return Stack{PostgresDSN: Postgres(t), RedisAddr: Redis(t), AmqpURL: RabbitMQ(t)}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

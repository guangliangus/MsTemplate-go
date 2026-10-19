// Package app is the composition root: it reads the configuration, builds every dependency
// explicitly (no container), registers the long-lived components with the lifecycle group in
// start order and runs the host in one of two modes with the same binary and image:
//
//	serve   the API listener plus, by configuration, the consumers, the outbox dispatcher and the
//	        task runner (tasks.max_workers=0, rabbitmq.consumers_enabled=false and
//	        kafka.consumers_enabled=false turn those off on API pods that have a worker deployment
//	        beside them)
//	worker  the consumers, the dispatcher and the task runner, and no API listener
//
// Both modes serve the probes and /metrics on the admin listener (server.admin_addr).
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"example.com/msvcservice/internal/config"
	"example.com/msvcservice/internal/httpapi"
	"example.com/msvcservice/internal/inventory"
	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/internal/postgres"
	"example.com/msvcservice/internal/tasks"
	"github.com/guangliangus/go-msvc-kit/auth"
	"github.com/guangliangus/go-msvc-kit/cache"
	"github.com/guangliangus/go-msvc-kit/health"
	"github.com/guangliangus/go-msvc-kit/httpclient"
	"github.com/guangliangus/go-msvc-kit/httpserver"
	"github.com/guangliangus/go-msvc-kit/httpx"
	"github.com/guangliangus/go-msvc-kit/lifecycle"
	"github.com/guangliangus/go-msvc-kit/logging"
	"github.com/guangliangus/go-msvc-kit/messaging"
	kitpg "github.com/guangliangus/go-msvc-kit/postgres"
	"github.com/guangliangus/go-msvc-kit/redisx"
	"github.com/guangliangus/go-msvc-kit/taskqueue"
	"github.com/guangliangus/go-msvc-kit/telemetry"
)

// Mode is the host shape.
type Mode string

// Host modes.
const (
	ModeServe  Mode = "serve"
	ModeWorker Mode = "worker"
)

const (
	// healthCheckTimeout bounds each readiness check; the chart's readiness timeoutSeconds is
	// above it, so the kubelet never gives up on a check that would have passed.
	healthCheckTimeout = 3 * time.Second
	// healthWatchInterval refreshes msvc_health_check_up between probes.
	healthWatchInterval = 15 * time.Second
	// closeTimeout bounds releasing each connection pool after the components stopped: a component
	// abandoned by the shutdown budget may still hold a connection, and pgxpool.Close waits for it.
	closeTimeout = 3 * time.Second
	// poolCloseTimeout is the same bound as closeTimeout, named for the closers list.
	poolCloseTimeout = closeTimeout
)

// Run builds and runs the host until ctx is cancelled (SIGTERM) or a component fails.
func Run(ctx context.Context, mode Mode, version string, openAPI []byte) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	hostname, _ := os.Hostname()
	log := logging.New(cfg.Logging, os.Stdout,
		slog.String("service", cfg.Service.Name),
		slog.String("mode", string(mode)),
		slog.String("version", version),
		slog.String("env", cfg.Environment))
	slog.SetDefault(log)
	log.Info("starting", slog.String("addr", cfg.Server.Addr), slog.String("admin_addr", cfg.Server.AdminAddr))

	tel, err := telemetry.Setup(ctx, cfg.Telemetry, telemetry.ServiceInfo{
		Name:        cfg.Service.Name + "-" + string(mode),
		Version:     version,
		Environment: cfg.Environment,
		Instance:    hostname,
	}, log)
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	defer func() {
		if err := tel.Shutdown(context.WithoutCancel(ctx)); err != nil {
			log.Warn("telemetry shutdown", slog.Any("error", err))
		}
	}()

	deps, err := build(ctx, cfg, mode, log)
	if err != nil {
		return err
	}
	defer deps.closers.Close()

	// Readiness gates only on what THIS pod cannot answer without (docs/architecture.md, failure
	// semantics). Redis and the broker are shared by every replica and degraded around — the cache
	// falls back to the database, the outbox holds events — so their outage is reported, not
	// turned into every pod leaving the Service at once.
	registry := health.New(healthCheckTimeout)
	defer registry.Close()
	registry.AddReadiness("postgres", deps.db.Ping)
	registry.AddInformational("redis", deps.rdb.Ping)

	// Every component declares its drain so the group can tell at startup, not at the first
	// SIGTERM, when the declared drains add up to the whole budget and leave the last ones nothing.
	group := lifecycle.NewGroup(log, cfg.Server.ShutdownTimeout)
	// Registered first so it stops LAST: the probes keep answering (readiness 503 while draining)
	// and the metrics stay scrapeable until everything else has stopped.
	group.Add("admin", httpserver.NewAdmin(cfg.Server, httpserver.AdminHandler(registry, tel.MetricsHandler()), log))
	group.Add("health", lifecycle.RunnerFunc(func(ctx context.Context) error {
		return registry.Watch(ctx, healthWatchInterval)
	}))
	if err := deps.registerMessaging(cfg, group, registry, log); err != nil {
		return err
	}
	deps.registerTasks(cfg, group, log)

	if mode == ModeServe {
		handler := httpapi.NewRouter(httpapi.Deps{
			Config:      cfg,
			Log:         log,
			ErrorWriter: deps.errorWriter,
			Auth:        deps.authValidator,
			DevHeaders:  deps.devHeaders,
			Idempotency: redisx.NewIdempotencyStore(deps.rdb),
			Orders:      deps.orders,
			OpenAPI:     openAPI,
		})
		// Registered last so it stops FIRST: readiness fails, the endpoint controller stops routing,
		// in-flight requests drain within server.drain_timeout, and only then do the workers stop.
		group.Add("api", httpserver.New(cfg.Server, handler, log), lifecycle.WithDrain(cfg.Server.DrainTimeout))
	}

	// Readiness answers 503 from the moment shutdown is requested. Stopping the hook on return
	// releases it when the group ended for another reason (a component failed).
	stopDraining := context.AfterFunc(ctx, registry.MarkDraining)
	defer stopDraining()
	registry.MarkStarted()
	return group.Run(ctx)
}

type dependencies struct {
	db            *kitpg.DB
	rdb           *redisx.Client
	errorWriter   *httpx.ErrorWriter
	authValidator *auth.Validator
	devHeaders    *auth.DevHeaders
	orders        *order.Service
	mode          string
	// closers releases the pools in reverse order once the components stopped, each within
	// closeTimeout: past it the process exits without waiting, rather than overrunning the pod's
	// grace period into a SIGKILL.
	closers *lifecycle.Closers
}

// build opens the resources and wires the use cases. A failure part-way closes whatever was already
// opened; after a success, Run owns the close.
func build(ctx context.Context, cfg *config.Config, mode Mode, log *slog.Logger) (_ *dependencies, err error) {
	d := &dependencies{mode: string(mode), closers: lifecycle.NewClosers(log, poolCloseTimeout)}
	defer func() {
		if err != nil {
			d.closers.Close()
		}
	}()

	db, err := kitpg.Open(ctx, cfg.Postgres, cfg.Service.Name+"-"+string(mode), log)
	if err != nil {
		return nil, err
	}
	d.db = db
	d.closers.Add("postgres", db.Close)

	rdb, err := redisx.Open(cfg.Redis, log)
	if err != nil {
		return nil, err
	}
	d.rdb = rdb
	d.closers.Add("redis", func() { _ = rdb.Close() })

	d.errorWriter = httpx.NewErrorWriter(cfg.HTTP.ProblemTypeBaseURI, log)
	d.errorWriter.Mappers = append(d.errorWriter.Mappers, kitpg.ErrorMapper)

	d.authValidator, err = auth.NewValidator(cfg.Auth)
	if err != nil {
		return nil, err
	}
	d.devHeaders = auth.NewDevHeaders(cfg.Auth.DevHeaders, cfg.Environment, log)

	catalog, err := httpapi.Catalog()
	if err != nil {
		return nil, err
	}
	d.errorWriter.Customizers = append(d.errorWriter.Customizers, httpx.Localize(catalog))

	inventoryHTTP, err := httpclient.New("inventory", cfg.ServiceClients["inventory"], log,
		httpclient.WithServiceName(cfg.Service.Name))
	if err != nil {
		return nil, err
	}
	d.orders = order.NewService(
		postgres.NewOrderRepository(db),
		db,
		inventory.New(inventoryHTTP),
		messaging.NewOutboxPublisher(db),
		cache.New(cfg.Cache, rdb, log),
		log,
	)
	return d, nil
}

// registerTasks wires the task runner with the reference tasks. tasks.max_workers=0 keeps the
// registrations but the runner exits at once.
func (d *dependencies) registerTasks(cfg *config.Config, group *lifecycle.Group, log *slog.Logger) {
	store := taskqueue.NewStore(d.db)
	runner := taskqueue.NewRunner(store, cfg.Tasks, log)
	// The interval here is only the fallback: tasks.recurring.pending-orders-reaper in the
	// configuration overrides it (default.yaml sets it), so change it there, not here.
	must(runner.Recurring("pending-orders-reaper", time.Minute,
		tasks.NewPendingOrdersReaper(d.orders, cfg.Orders.PendingTimeout, log)))
	must(runner.Cron("heartbeat", "0 */5 * * * *", tasks.NewHeartbeat(log)))
	group.Add("tasks", runner, lifecycle.WithDrain(cfg.Tasks.DrainTimeout))
}

// must turns a registration error — a programming mistake caught at startup — into a panic.
func must(err error) {
	if err != nil {
		panic(err)
	}
}

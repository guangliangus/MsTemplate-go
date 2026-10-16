// Package app is the composition root: it reads the configuration, builds every dependency
// explicitly (no container), registers the long-lived components with the lifecycle group in
// start order and runs the host in one of two modes with the same binary and image:
//
//	serve   the API listener plus, by configuration, the consumers, the outbox dispatcher and the
//	        task runner (tasks.max_workers=0 and rabbitmq.consumers_enabled=false turn those off on
//	        API pods that have a worker deployment beside them)
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
	"example.com/msvcservice/internal/consumers"
	"example.com/msvcservice/internal/httpapi"
	"example.com/msvcservice/internal/inventory"
	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/internal/postgres"
	"example.com/msvcservice/internal/tasks"
	"example.com/msvcservice/pkg/auth"
	"example.com/msvcservice/pkg/cache"
	"example.com/msvcservice/pkg/health"
	"example.com/msvcservice/pkg/httpclient"
	"example.com/msvcservice/pkg/httpserver"
	"example.com/msvcservice/pkg/httpx"
	"example.com/msvcservice/pkg/lifecycle"
	"example.com/msvcservice/pkg/logging"
	"example.com/msvcservice/pkg/messaging"
	kitpg "example.com/msvcservice/pkg/postgres"
	"example.com/msvcservice/pkg/rabbitmq"
	"example.com/msvcservice/pkg/redisx"
	"example.com/msvcservice/pkg/taskqueue"
	"example.com/msvcservice/pkg/telemetry"
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
	defer deps.close(log)

	// Readiness gates only on what THIS pod cannot answer without (docs/architecture.md, failure
	// semantics). Redis and the broker are shared by every replica and degraded around — the cache
	// falls back to the database, the outbox holds events — so their outage is reported, not
	// turned into every pod leaving the Service at once.
	registry := health.New(healthCheckTimeout)
	registry.AddReadiness("postgres", deps.db.Ping)
	registry.AddInformational("redis", deps.rdb.Ping)

	group := lifecycle.NewGroup(log, cfg.Server.ShutdownTimeout)
	// Registered first so it stops LAST: the probes keep answering (readiness 503 while draining)
	// and the metrics stay scrapeable until everything else has stopped.
	group.Add("admin", httpserver.NewAdmin(cfg.Server, httpapi.AdminRouter(registry, tel.MetricsHandler()), log))
	group.Add("health", lifecycle.RunnerFunc(func(ctx context.Context) error {
		return registry.Watch(ctx, healthWatchInterval)
	}))
	deps.registerMessaging(cfg, group, registry, log)
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
		group.Add("api", httpserver.New(cfg.Server, handler, log))
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
	closers       []closer
}

type closer struct {
	name string
	fn   func()
}

// close releases the resources in reverse order, each within closeTimeout: past it the process
// exits without waiting, rather than overrunning the pod's grace period into a SIGKILL.
func (d *dependencies) close(log *slog.Logger) {
	for i := len(d.closers) - 1; i >= 0; i-- {
		c := d.closers[i]
		done := make(chan struct{})
		go func() {
			c.fn()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeTimeout):
			log.Warn("closing a resource timed out; exiting without it", slog.String("resource", c.name),
				slog.Duration("timeout", closeTimeout))
		}
	}
}

func (d *dependencies) onClose(name string, fn func()) {
	d.closers = append(d.closers, closer{name: name, fn: fn})
}

// build opens the resources and wires the use cases. A failure part-way closes whatever was already
// opened; after a success, Run owns the close.
func build(ctx context.Context, cfg *config.Config, mode Mode, log *slog.Logger) (_ *dependencies, err error) {
	d := &dependencies{mode: string(mode)}
	defer func() {
		if err != nil {
			d.close(log)
		}
	}()

	db, err := kitpg.Open(ctx, cfg.Postgres, cfg.Service.Name+"-"+string(mode), log)
	if err != nil {
		return nil, err
	}
	d.db = db
	d.onClose("postgres", db.Close)

	rdb, err := redisx.Open(cfg.Redis, log)
	if err != nil {
		return nil, err
	}
	d.rdb = rdb
	d.onClose("redis", func() { _ = rdb.Close() })

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

// registerMessaging wires the broker connection, the outbox dispatcher and the consumers. Start
// order is stop order reversed: the connection stops last, so draining consumers can still nack.
// The broker is informational for readiness: while it is away the outbox holds the events and the
// consumers reconnect on their own.
func (d *dependencies) registerMessaging(cfg *config.Config, group *lifecycle.Group,
	registry *health.Registry, log *slog.Logger) {
	if !cfg.RabbitMQ.Enabled {
		log.Warn("rabbitmq is disabled: outbox rows will accumulate as PENDING until a transport is enabled")
		return
	}
	conn := rabbitmq.NewConnection(cfg.RabbitMQ.Config, cfg.Service.Name+"-"+d.mode, log)
	registry.AddInformational("rabbitmq", conn.Ping)
	group.Add("rabbitmq", conn)

	publisher := rabbitmq.NewPublisher(conn, cfg.RabbitMQ.Config, log)
	group.Add("outbox", messaging.NewOutboxDispatcher(d.db, publisher, cfg.Outbox, log))

	if !cfg.RabbitMQ.ConsumersEnabled {
		log.Info("consumers disabled on this host (rabbitmq.consumers_enabled=false)")
		return
	}
	inbox := messaging.NewInbox(d.db)
	subscriptions := []messaging.Subscription{
		messaging.Subscribe(order.EventCreated, queueName(cfg.Service.Name, order.EventCreated),
			inbox, consumers.NewOrderCreatedHandler(d.orders, log)),
	}
	group.Add("consumers", rabbitmq.NewConsumerHost(conn, cfg.RabbitMQ.Config, publisher, log, subscriptions...))
}

// registerTasks wires the task runner with the reference tasks. tasks.max_workers=0 keeps the
// registrations but the runner exits at once.
func (d *dependencies) registerTasks(cfg *config.Config, group *lifecycle.Group, log *slog.Logger) {
	store := taskqueue.NewStore(d.db)
	runner := taskqueue.NewRunner(store, cfg.Tasks, log)
	must(runner.Recurring("pending-orders-reaper", time.Minute,
		tasks.NewPendingOrdersReaper(d.orders, cfg.Orders.PendingTimeout, log)))
	must(runner.Cron("heartbeat", "0 */5 * * * *", tasks.NewHeartbeat(log)))
	group.Add("tasks", runner)
}

// queueName is "{service}.{event}": the same in both host modes, so API pods and worker pods
// that both consume are competing consumers on one queue, never two copies of the work.
func queueName(service, event string) string {
	return service + "." + event
}

// must turns a registration error — a programming mistake caught at startup — into a panic.
func must(err error) {
	if err != nil {
		panic(err)
	}
}

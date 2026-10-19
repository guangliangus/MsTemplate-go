package app

import (
	"fmt"
	"log/slog"

	"example.com/msvcservice/internal/config"
	"example.com/msvcservice/internal/consumers"
	"example.com/msvcservice/internal/order"
	"github.com/guangliangus/go-msvc-kit/health"
	"github.com/guangliangus/go-msvc-kit/kafka"
	"github.com/guangliangus/go-msvc-kit/lifecycle"
	"github.com/guangliangus/go-msvc-kit/messaging"
	"github.com/guangliangus/go-msvc-kit/rabbitmq"
)

// registerMessaging wires the two buses. Each event lives on exactly one of them — kafka.events
// lists Kafka's, every other event is RabbitMQ's — so each bus gets its own outbox dispatcher over
// its own events and its own consumer host over its own subscriptions: one broker being away
// holds back nothing on the other. Per bus, start order is stop order reversed: the connection
// stops last, so draining consumers can still settle what they hold. Both brokers are
// informational for readiness: while one is away the outbox holds its events and its consumers
// reconnect on their own.
func (d *dependencies) registerMessaging(cfg *config.Config, group *lifecycle.Group,
	registry *health.Registry, log *slog.Logger) error {
	rabbitSubs, kafkaSubs := splitByBus(d.subscriptions(cfg, log), cfg.Kafka.OnKafka)
	switch {
	case !cfg.RabbitMQ.Enabled && !cfg.Kafka.Enabled:
		log.Warn("no message bus is enabled: outbox rows will accumulate as PENDING until one is")
	case !cfg.RabbitMQ.Enabled:
		log.Warn("rabbitmq is disabled: events not listed in kafka.events accumulate as PENDING",
			slog.Any("kafka_events", cfg.Kafka.Events))
	default:
	}
	housekept := false
	if cfg.RabbitMQ.Enabled {
		d.registerRabbitMQ(cfg, group, registry, log, rabbitSubs)
		housekept = true
	}
	if cfg.Kafka.Enabled {
		if err := d.registerKafka(cfg, group, registry, log, kafkaSubs, housekept); err != nil {
			return err
		}
	}
	return nil
}

// subscriptions are this service's event handlers, declared once whatever bus carries them.
// Queue and consumer-group names come from messaging.QueueName ("{service}.{event}"): the same in
// both host modes, so API pods and worker pods that both consume compete for one queue or group.
func (d *dependencies) subscriptions(cfg *config.Config, log *slog.Logger) []messaging.Subscription {
	inbox := messaging.NewInbox(d.db)
	return []messaging.Subscription{
		messaging.Subscribe(order.EventCreated, messaging.QueueName(cfg.Service.Name, order.EventCreated),
			inbox, consumers.NewOrderCreatedHandler(d.orders, log)),
	}
}

// splitByBus hands each subscription to the bus its event lives on.
func splitByBus(subs []messaging.Subscription, onKafka func(event string) bool,
) (rabbit, kafka []messaging.Subscription) {
	for _, sub := range subs {
		if onKafka(sub.EventName) {
			kafka = append(kafka, sub)
		} else {
			rabbit = append(rabbit, sub)
		}
	}
	return rabbit, kafka
}

// registerRabbitMQ wires the connection, the dispatcher of every event Kafka does not own (it also
// samples the outbox backlog and runs the cleanup) and the consumers of RabbitMQ's events.
func (d *dependencies) registerRabbitMQ(cfg *config.Config, group *lifecycle.Group, registry *health.Registry,
	log *slog.Logger, subs []messaging.Subscription) {
	conn := rabbitmq.NewConnection(cfg.RabbitMQ.Config, cfg.Service.Name+"-"+d.mode, log)
	registry.AddInformational("rabbitmq", conn.Ping)
	group.Add("rabbitmq", conn)

	// app-id names this service on every event, so a dead-letter queue says where a message came from.
	publisher := rabbitmq.NewPublisher(conn, cfg.RabbitMQ.Config, log, rabbitmq.WithAppID(cfg.Service.Name))
	group.Add("outbox", messaging.NewOutboxDispatcher(d.db, publisher, cfg.Outbox, log,
		messaging.ExceptEvents(cfg.Kafka.Events...)))

	switch {
	case !cfg.RabbitMQ.ConsumersEnabled:
		log.Info("rabbitmq consumers disabled on this host (rabbitmq.consumers_enabled=false)")
	case len(subs) > 0:
		group.Add("consumers", rabbitmq.NewConsumerHost(conn, cfg.RabbitMQ.Config, publisher, log, subs),
			lifecycle.WithDrain(cfg.RabbitMQ.DrainTimeout))
	default:
	}
}

// registerKafka wires the client (it stops last, after the dispatcher and the consumers), the
// dispatcher of the events listed in kafka.events and the consumers of those events. housekept says
// whether another dispatcher already samples the backlog and runs the cleanup.
func (d *dependencies) registerKafka(cfg *config.Config, group *lifecycle.Group, registry *health.Registry,
	log *slog.Logger, subs []messaging.Subscription, housekept bool) error {
	client, err := kafka.NewClient(cfg.Kafka.Config, cfg.Service.Name+"-"+d.mode, log)
	if err != nil {
		return fmt.Errorf("kafka: %w", err)
	}
	registry.AddInformational("kafka", client.Ping)
	group.Add("kafka", client)

	if len(cfg.Kafka.Events) == 0 {
		log.Info("kafka is enabled but kafka.events is empty: nothing is published to it")
	} else {
		publisher := kafka.NewPublisher(client, cfg.Kafka.Config, log, kafka.WithAppID(cfg.Service.Name))
		opts := []messaging.DispatcherOption{messaging.ForEvents(cfg.Kafka.Events...)}
		if housekept {
			opts = append(opts, messaging.WithoutHousekeeping())
		}
		group.Add("kafka-outbox", messaging.NewOutboxDispatcher(d.db, publisher, cfg.Outbox, log, opts...))
	}

	switch {
	case !cfg.Kafka.ConsumersEnabled:
		log.Info("kafka consumers disabled on this host (kafka.consumers_enabled=false)")
	case len(subs) > 0:
		group.Add("kafka-consumers", kafka.NewConsumerHost(client, cfg.Kafka.Config, log, subs),
			lifecycle.WithDrain(cfg.Kafka.DrainTimeout))
	default:
	}
	return nil
}

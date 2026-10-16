// Package rabbitmq is the broker half of messaging: a self-healing connection, the topology
// (topic exchange, quorum work queues, a TTL retry ladder and a dead-letter queue per consumer),
// a confirming publisher and the consumer host with manual acks, bounded concurrency, trace
// propagation and graceful drain.
//
// Per consumer queue Q the topology is:
//
//	Q            work queue bound to the topic exchange; rejects dead-letter to Q.dlq
//	Q.retry.N    one queue per retry delay, x-message-ttl = delay, expiring back into Q
//	Q.dlq        terminal dead letters (inspect by hand, replay with tools/dlq-replay.sh)
//
// No delayed-message plugin is needed, so any RabbitMQ runs it; the price is a fixed ladder.
package rabbitmq

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// Config is bound from the "rabbitmq" section.
type Config struct {
	// Enabled wires the connection, the publisher, the consumers and the outbox dispatcher. Off
	// leaves the outbox publisher writing rows that nobody dispatches — a visible backlog, never
	// a lost event — for a service that has not chosen its broker yet.
	Enabled bool `koanf:"enabled"`
	// URL is amqp:// or amqps:// (TLS with the system roots); credentials come from a Secret.
	URL string `koanf:"url"`
	// Exchange is the topic exchange every integration event is published to.
	Exchange string `koanf:"exchange" validate:"required"`
	// QueueType is quorum (default) or classic. Quorum queues are replicated, so on a clustered
	// broker a node loss loses no messages (classic mirroring was removed in RabbitMQ 4). A queue's
	// type cannot be changed in place: switching later means deleting and re-declaring.
	QueueType string `koanf:"queue_type" validate:"oneof=quorum classic"`
	// Prefetch is the unacked messages delivered per consumer channel (0 = unlimited).
	Prefetch int `koanf:"prefetch" validate:"min=0,max=65535"`
	// ConsumerConcurrency is the concurrent handler invocations per consumer channel.
	ConsumerConcurrency int `koanf:"consumer_concurrency" validate:"min=1,max=1024"`
	// RetryDelays is the delayed-retry ladder; empty means 30s, 2m, 10m.
	RetryDelays []time.Duration `koanf:"retry_delays"`
	// PublisherConfirms waits for the broker's ack on every publish (at-least-once from the
	// publisher's side).
	PublisherConfirms bool `koanf:"publisher_confirms"`
	// PublishTimeout bounds one publish as a whole: waiting for a live connection, for the
	// publisher's channel and for the confirm.
	PublishTimeout time.Duration `koanf:"publish_timeout" validate:"min=100ms"`
	// DrainTimeout is how long a stopping consumer host lets in-flight handlers finish (after it
	// stopped taking deliveries) before cancelling them; cancelled handlers' messages are requeued.
	DrainTimeout time.Duration `koanf:"drain_timeout" validate:"min=0"`
	// ReconnectDelay is the pause between connection attempts.
	ReconnectDelay time.Duration `koanf:"reconnect_delay" validate:"min=1s"`
	// Heartbeat is the AMQP heartbeat interval.
	Heartbeat time.Duration `koanf:"heartbeat" validate:"min=1s"`
}

// DefaultRetryDelays is the ladder used when none is configured.
var DefaultRetryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// Validate checks the URL when the broker is enabled.
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.URL == "" {
		return errors.New("url is required when enabled")
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "amqp" && u.Scheme != "amqps" {
		return fmt.Errorf("url scheme must be amqp or amqps, got %q", u.Scheme)
	}
	for _, d := range c.RetryDelays {
		if d <= 0 {
			return errors.New("retry_delays entries must be positive")
		}
	}
	return nil
}

// EffectiveRetryDelays returns the configured ladder or the default.
func (c Config) EffectiveRetryDelays() []time.Duration {
	if len(c.RetryDelays) > 0 {
		return c.RetryDelays
	}
	return DefaultRetryDelays
}

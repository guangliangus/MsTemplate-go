package rabbitmq

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Topology declares exchanges and queues; every declaration is idempotent, so every host runs
// them at startup.
type Topology struct {
	cfg Config
}

// NewTopology builds the declarer.
func NewTopology(cfg Config) Topology {
	return Topology{cfg: cfg}
}

// DeclareExchange declares the durable topic exchange.
func (t Topology) DeclareExchange(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(t.cfg.Exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: declare exchange %s: %w", t.cfg.Exchange, err)
	}
	return nil
}

// DeclareConsumerQueues declares the work queue, its retry ladder and its dead-letter queue, and
// binds the work queue to the exchange with routingKey.
func (t Topology) DeclareConsumerQueues(ch *amqp.Channel, queue, routingKey string) error {
	if err := t.DeclareExchange(ch); err != nil {
		return err
	}
	dlq := DeadLetterQueue(queue)
	if _, err := ch.QueueDeclare(dlq, true, false, false, false, t.queueArgs()); err != nil {
		return fmt.Errorf("rabbitmq: declare %s: %w", dlq, err)
	}

	workArgs := t.queueArgs()
	workArgs["x-dead-letter-exchange"] = ""
	workArgs["x-dead-letter-routing-key"] = dlq
	if _, err := ch.QueueDeclare(queue, true, false, false, false, workArgs); err != nil {
		return fmt.Errorf("rabbitmq: declare %s: %w", queue, err)
	}

	for i, delay := range t.cfg.EffectiveRetryDelays() {
		retryArgs := t.queueArgs()
		retryArgs["x-message-ttl"] = int32(delay.Milliseconds()) //nolint:gosec // delays are seconds to minutes
		retryArgs["x-dead-letter-exchange"] = ""
		retryArgs["x-dead-letter-routing-key"] = queue
		name := RetryQueue(queue, i+1)
		if _, err := ch.QueueDeclare(name, true, false, false, false, retryArgs); err != nil {
			return fmt.Errorf("rabbitmq: declare %s: %w", name, err)
		}
	}

	if err := ch.QueueBind(queue, routingKey, t.cfg.Exchange, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: bind %s to %s: %w", queue, routingKey, err)
	}
	return nil
}

func (t Topology) queueArgs() amqp.Table {
	args := amqp.Table{}
	if t.cfg.QueueType == "quorum" {
		args["x-queue-type"] = "quorum"
	}
	return args
}

// RetryQueue names the n-th retry queue of a work queue.
func RetryQueue(queue string, attempt int) string {
	return fmt.Sprintf("%s.retry.%d", queue, attempt)
}

// DeadLetterQueue names the terminal queue of a work queue.
func DeadLetterQueue(queue string) string {
	return queue + ".dlq"
}

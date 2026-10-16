package messaging

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"example.com/msvcservice/pkg/postgres"
)

// OutboxConfig is bound from the "outbox" section.
type OutboxConfig struct {
	// PollInterval is the dispatcher's sleep when it finds nothing to send.
	PollInterval time.Duration `koanf:"poll_interval" validate:"min=100ms"`
	// BatchSize is how many PENDING rows one cycle claims.
	BatchSize int `koanf:"batch_size" validate:"min=1,max=1000"`
	// MaxAttempts parks a row as FAILED after that many publish failures.
	MaxAttempts int `koanf:"max_attempts" validate:"min=1"`
	// CleanupEnabled deletes PUBLISHED outbox rows and processed inbox rows after Retention. Off
	// keeps them forever, for a service whose outbox doubles as an event journal; the table's
	// growth is then that service's to manage.
	CleanupEnabled bool `koanf:"cleanup_enabled"`
	// Retention is how long published rows are kept.
	Retention time.Duration `koanf:"retention" validate:"min=1m"`
	// CleanupInterval is how often the cleanup runs.
	CleanupInterval time.Duration `koanf:"cleanup_interval" validate:"min=1m"`
}

// Outbox row statuses.
const (
	StatusPending   = "PENDING"
	StatusPublished = "PUBLISHED"
	StatusFailed    = "FAILED"
)

// OutboxPublisher is the Publisher application code uses: Publish inserts a PENDING row on the
// caller's connection — inside the caller's transaction when there is one — so the business
// change and the event commit together or not at all (no 2PC). The dispatcher does the broker
// publish afterwards.
type OutboxPublisher struct {
	db *postgres.DB
}

// NewOutboxPublisher builds the publisher.
func NewOutboxPublisher(db *postgres.DB) *OutboxPublisher {
	return &OutboxPublisher{db: db}
}

// Publish writes the outbox row.
func (p *OutboxPublisher) Publish(ctx context.Context, event Event) error {
	env, err := NewEnvelope(ctx, event)
	if err != nil {
		return err
	}
	const sql = `
		INSERT INTO outbox_messages
			(message_id, event_name, payload, status, attempts, occurred_at, trace_parent, correlation_id)
		VALUES ($1, $2, $3, 'PENDING', 0, $4, $5, $6)`
	_, err = p.db.Q(ctx).Exec(ctx, sql, env.MessageID, env.EventName, env.Payload, env.OccurredAt,
		nullable(env.TraceParent), nullable(env.CorrelationID))
	if err != nil {
		return fmt.Errorf("messaging: write outbox row for %s: %w", env.EventName, err)
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type outboxRow struct {
	ID            int64     `db:"id"`
	MessageID     uuid.UUID `db:"message_id"`
	EventName     string    `db:"event_name"`
	Payload       []byte    `db:"payload"`
	Attempts      int       `db:"attempts"`
	OccurredAt    time.Time `db:"occurred_at"`
	TraceParent   *string   `db:"trace_parent"`
	CorrelationID *string   `db:"correlation_id"`
}

func (r outboxRow) envelope() Envelope {
	env := Envelope{MessageID: r.MessageID, EventName: r.EventName, Payload: r.Payload, OccurredAt: r.OccurredAt}
	if r.TraceParent != nil {
		env.TraceParent = *r.TraceParent
	}
	if r.CorrelationID != nil {
		env.CorrelationID = *r.CorrelationID
	}
	return env
}

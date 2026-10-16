package messaging

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"example.com/msvcservice/pkg/postgres"
)

// Inbox is the consumer-side ledger: UNIQUE(message_id, consumer) makes a redelivery a no-op, and
// running the handler inside the same transaction as the ledger insert makes the handler's
// database writes exactly-once in effect. A handler that fails rolls the ledger row back with its
// own changes, so the retry ladder sees a message that was never processed.
type Inbox struct {
	db *postgres.DB
}

// NewInbox builds the inbox.
func NewInbox(db *postgres.DB) *Inbox {
	return &Inbox{db: db}
}

// ProcessOnce records (messageID, consumer) and runs fn in one transaction. It returns false,
// without running fn, when the pair was already recorded.
func (i *Inbox) ProcessOnce(ctx context.Context, messageID uuid.UUID, consumer string,
	fn func(ctx context.Context) error) (bool, error) {
	processed := false
	err := i.db.WithTx(ctx, func(ctx context.Context) error {
		tag, err := i.db.Q(ctx).Exec(ctx, `
			INSERT INTO inbox_messages (message_id, consumer, processed_at)
			VALUES ($1, $2, now())
			ON CONFLICT (message_id, consumer) DO NOTHING`, messageID, consumer)
		if err != nil {
			return fmt.Errorf("messaging: record inbox row: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		processed = true
		return fn(ctx)
	})
	return processed, err
}

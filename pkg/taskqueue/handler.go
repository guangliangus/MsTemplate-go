package taskqueue

import (
	"context"
	"encoding/json"
	"time"
)

// Task is the claimed row as handed to the handler.
type Task struct {
	QueueName string
	TaskID    string
	Payload   json.RawMessage
	Priority  int
	// Attempt is 1 on the first run and grows with every claim (retries and fixer reclaims
	// included): use it for backoff ladders or to give up.
	Attempt   int
	DeliverOn time.Time
	// Misfired is true on a cron firing claimed later than the misfire threshold.
	Misfired bool
}

// Handler executes tasks of one queue. The returned Result decides the row's fate; returning an
// error (or panicking) re-arms the row with exponential backoff (tasks.retry_base_delay up to
// tasks.retry_max_delay) and spends one attempt of the budget, exactly like Retry. Delivery is
// at-least-once: handlers must be idempotent or re-check state, like the inbox pattern on the
// messaging side.
//
// Two ways to finish, and the difference is a crash window. Done lets the runner delete the row
// afterwards, in a second transaction, so a pod that dies between the handler's commit and that
// delete runs the task again. A handler whose work IS a database write can close the window:
// call Store.Complete inside its own transaction (WithTx) and return Settled, so the business row
// and the completion commit together or not at all.
type Handler interface {
	Handle(ctx context.Context, task Task) (Result, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, task Task) (Result, error)

// Handle calls f.
func (f HandlerFunc) Handle(ctx context.Context, task Task) (Result, error) { return f(ctx, task) }

// Action is what the runner does with the row after the handler.
type Action int

const (
	// ActionDone deletes the row (a recurring row is re-armed to its next run instead).
	ActionDone Action = iota
	// ActionRetry re-arms the row to run again after Result.RetryAfter.
	ActionRetry
	// ActionDiscard deletes the row and logs the error.
	ActionDiscard
	// ActionSettled means the handler already removed the row in its own transaction; the runner
	// touches nothing (a recurring row is still re-armed, because its row is its schedule).
	ActionSettled
)

func (a Action) String() string {
	switch a {
	case ActionDone:
		return "done"
	case ActionRetry:
		return "retry"
	case ActionDiscard:
		return "discard"
	case ActionSettled:
		return "settled"
	default:
		return "unknown"
	}
}

// Result is the handler's verdict.
type Result struct {
	Action       Action
	RetryAfter   time.Duration
	ErrorCode    string
	ErrorMessage string
}

// Done finishes the task.
func Done() Result { return Result{Action: ActionDone} }

// Retry re-arms the task after the delay; the code and message go to the log.
func Retry(after time.Duration, errorCode, errorMessage string) Result {
	return Result{Action: ActionRetry, RetryAfter: after, ErrorCode: errorCode, ErrorMessage: errorMessage}
}

// Discard gives up on the task.
func Discard(errorCode, errorMessage string) Result {
	return Result{Action: ActionDiscard, ErrorCode: errorCode, ErrorMessage: errorMessage}
}

// Settled reports that the handler completed the row itself, transactionally. Returning it
// without having called Store.Complete leaves the row claimed until the fixer releases it: a slow
// retry, never a lost task.
func Settled() Result { return Result{Action: ActionSettled} }

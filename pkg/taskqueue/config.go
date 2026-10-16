// Package taskqueue is the database task queue: one task_queues table, FOR UPDATE SKIP LOCKED
// claims so a task runs on exactly one replica, an attempt budget on the row, a fixer that
// re-arms claims whose worker died, and recurring (interval) and cron (wall-clock, with a misfire
// policy) tasks as rows that re-arm themselves. It is the one scheduler for both host shapes and
// shares its table design with the platform's other services, so one set of operational SQL fits all.
//
// Every "due" comparison and every relative re-arm uses the DATABASE clock, so a pod's clock skew
// never changes when a task runs. Delivery is at-least-once: handlers must tolerate a repeat.
package taskqueue

import (
	"errors"
	"fmt"
	"time"
)

// Config is bound from the "tasks" section.
type Config struct {
	// MaxWorkers is the concurrent handlers per queue per replica. 0 is the kill switch: the queue
	// is not polled, seeded or swept on this host at all, so the same image serves an API pod (0)
	// and a worker pod (>0) — a config change, not a release.
	MaxWorkers int `koanf:"max_workers" validate:"min=0,max=256"`
	// MaxAttempts is the attempt budget of one-off tasks (0 = unlimited): a task that fails — its
	// handler returned an error, panicked or asked to retry — after being claimed that many times is
	// dead-lettered (deleted and logged) instead. Recurring tasks are exempt: their schedule must
	// survive bad days.
	MaxAttempts int `koanf:"max_attempts" validate:"min=0"`
	// RetryBaseDelay is the first backoff after a handler error or panic; it doubles per attempt up
	// to RetryMaxDelay, with jitter. A handler that knows better returns Retry(after) instead.
	RetryBaseDelay time.Duration `koanf:"retry_base_delay" validate:"min=1s"`
	// RetryMaxDelay caps that backoff.
	RetryMaxDelay time.Duration `koanf:"retry_max_delay" validate:"min=1s"`
	// ShortPollInterval is the sleep while every worker slot is busy.
	ShortPollInterval time.Duration `koanf:"short_poll_interval" validate:"min=10ms"`
	// LongPollInterval is the sleep when the queue turned up empty.
	LongPollInterval time.Duration `koanf:"long_poll_interval" validate:"min=10ms"`
	// TaskTimeout bounds one handler run; the handler's context ends then — and ONLY then, never on
	// shutdown, so a handler cannot mistake "the pod is going away" for "you took too long".
	TaskTimeout time.Duration `koanf:"task_timeout" validate:"min=1s"`
	// StaleAfter is when a claimed row counts as abandoned and the fixer re-arms it. Must be
	// strictly greater than TaskTimeout: with the two equal, a handler that overruns is still
	// working when its row is handed to another worker, and the same task runs twice (measured).
	StaleAfter time.Duration `koanf:"stale_after" validate:"min=1s"`
	// DrainTimeout bounds how long shutdown waits for in-flight tasks; rows still running after it
	// stay claimed and come back through the fixer. Keep it below the host's shutdown budget.
	DrainTimeout time.Duration `koanf:"drain_timeout" validate:"min=1s"`
	// FixerInterval is how often the fixer sweeps (and samples the queue depth metric).
	FixerInterval time.Duration `koanf:"fixer_interval" validate:"min=1s"`
	// MisfireThreshold is how late a cron firing may be claimed before it counts as misfired.
	MisfireThreshold time.Duration `koanf:"misfire_threshold" validate:"min=0"`
	// Recurring overrides recurring intervals by task name.
	Recurring map[string]time.Duration `koanf:"recurring"`
	// Cron overrides cron schedules by task name.
	Cron map[string]CronConfig `koanf:"cron"`
	// Queues holds per-queue overrides.
	Queues map[string]QueueOverride `koanf:"queues"`
}

// CronConfig overrides one cron task.
type CronConfig struct {
	// Expression is five fields, or six with a leading seconds field.
	Expression string `koanf:"expression"`
	// TimeZone is an IANA id (Asia/Taipei); empty reads the expression in UTC.
	TimeZone string `koanf:"time_zone"`
	// Misfire is fire_once_now, skip or catch_up.
	Misfire string `koanf:"misfire" validate:"omitempty,oneof=fire_once_now skip catch_up"`
}

// QueueOverride narrows the defaults for one queue.
type QueueOverride struct {
	// MaxWorkers caps this queue only; 0 disables just this queue on this host.
	MaxWorkers *int `koanf:"max_workers" validate:"omitempty,min=0,max=256"`
	// MaxAttempts overrides the budget for this queue.
	MaxAttempts *int `koanf:"max_attempts" validate:"omitempty,min=0"`
	// ShortPollInterval and LongPollInterval override the poll cadence.
	ShortPollInterval *time.Duration `koanf:"short_poll_interval"`
	LongPollInterval  *time.Duration `koanf:"long_poll_interval"`
}

// Validate enforces the one relationship that cannot be left to judgement.
func (c *Config) Validate() error {
	if c.TaskTimeout >= c.StaleAfter {
		return fmt.Errorf("task_timeout (%s) must be strictly less than stale_after (%s), so a handler is "+
			"always cancelled before the fixer can give its row to another worker", c.TaskTimeout, c.StaleAfter)
	}
	if c.RetryBaseDelay > c.RetryMaxDelay {
		return fmt.Errorf("retry_base_delay (%s) must not exceed retry_max_delay (%s)",
			c.RetryBaseDelay, c.RetryMaxDelay)
	}
	for name, cron := range c.Cron {
		if cron.Expression == "" {
			return errors.New("cron." + name + ".expression is required")
		}
		if _, err := ParseCron(cron.Expression, cron.TimeZone, MisfirePolicy(cron.Misfire)); err != nil {
			return fmt.Errorf("cron.%s: %w", name, err)
		}
	}
	return nil
}

type queueSettings struct {
	maxWorkers  int
	maxAttempts int
	shortPoll   time.Duration
	longPoll    time.Duration
}

func (c Config) settingsFor(queue string) queueSettings {
	s := queueSettings{
		maxWorkers:  c.MaxWorkers,
		maxAttempts: c.MaxAttempts,
		shortPoll:   c.ShortPollInterval,
		longPoll:    c.LongPollInterval,
	}
	o, ok := c.Queues[queue]
	if !ok {
		return s
	}
	if o.MaxWorkers != nil {
		s.maxWorkers = *o.MaxWorkers
	}
	if o.MaxAttempts != nil {
		s.maxAttempts = *o.MaxAttempts
	}
	if o.ShortPollInterval != nil {
		s.shortPoll = *o.ShortPollInterval
	}
	if o.LongPollInterval != nil {
		s.longPoll = *o.LongPollInterval
	}
	return s
}

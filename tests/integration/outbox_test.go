//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/msvcservice/pkg/messaging"
)

// fakeTransport stands in for the broker: ready or not, and what each publish answers.
type fakeTransport struct {
	ready     atomic.Bool
	mu        sync.Mutex
	answer    error
	published int
}

func (f *fakeTransport) Ready() bool { return f.ready.Load() }

func (f *fakeTransport) Publish(context.Context, messaging.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.answer == nil {
		f.published++
	}
	return f.answer
}

func (f *fakeTransport) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = err
}

type outboxRowState struct {
	status    string
	attempts  int
	lastError *string
}

// A broker outage delays events, it never spends their attempt budget or holds rows locked; only a
// refusal of the message itself counts an attempt.
func TestOutboxDispatcherTellsOutagesFromRefusals(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := messaging.NewOutboxPublisher(db).Publish(ctx, testEvent{OrderID: 1}); err != nil {
		t.Fatal(err)
	}
	transport := &fakeTransport{}
	dispatcher := messaging.NewOutboxDispatcher(db, transport, messaging.OutboxConfig{PollInterval: 50 * time.Millisecond,
		BatchSize: 10, MaxAttempts: 3, Retention: time.Hour, CleanupInterval: time.Hour}, quiet)
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	row := func() outboxRowState {
		var s outboxRowState
		if err := db.Pool.QueryRow(ctx, `SELECT status, attempts, last_error FROM outbox_messages ORDER BY id LIMIT 1`).
			Scan(&s.status, &s.attempts, &s.lastError); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Not ready: nothing is claimed, so the row is not even locked.
	time.Sleep(300 * time.Millisecond)
	var claimable int
	err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT id FROM outbox_messages FOR UPDATE SKIP LOCKED) AS free`).
		Scan(&claimable)
	if err != nil || claimable != 1 {
		t.Fatalf("a dispatcher waiting for the broker holds no row lock: %d %v", claimable, err)
	}
	if s := row(); s.status != "PENDING" || s.attempts != 0 || s.lastError != nil {
		t.Fatalf("untouched while the broker is away: %+v", s)
	}

	// Ready, but the publish times out: an outage, recorded without spending an attempt.
	transport.set(context.DeadlineExceeded)
	transport.ready.Store(true)
	waitFor(t, func() bool { return row().lastError != nil })
	if s := row(); s.status != "PENDING" || s.attempts != 0 {
		t.Fatalf("an outage keeps the attempt budget: %+v", s)
	}

	// The broker refuses the message itself: that spends attempts, up to FAILED.
	transport.set(errors.New("broker nacked publish"))
	waitFor(t, func() bool { return row().status == "FAILED" })
	if s := row(); s.attempts != 3 || s.lastError == nil || *s.lastError != "broker nacked publish" {
		t.Fatalf("parked after max_attempts: %+v", s)
	}
	cancel()
	<-done
}

func TestOutboxCleanupDeletesInBatches(t *testing.T) {
	db := openDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// More than two cleanup batches of old PUBLISHED rows, plus one recent row that must stay.
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO outbox_messages (message_id, event_name, payload, status, attempts, occurred_at, published_at)
		SELECT gen_random_uuid(), 'test.event.v1', '{}', 'PUBLISHED', 0, now() - interval '2 days', now() - interval '2 days'
		FROM   generate_series(1, 12000)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO outbox_messages (message_id, event_name, payload, status, attempts, occurred_at, published_at)
		VALUES (gen_random_uuid(), 'test.event.v1', '{}', 'PUBLISHED', 0, now(), now())`); err != nil {
		t.Fatal(err)
	}
	transport := &fakeTransport{}
	dispatcher := messaging.NewOutboxDispatcher(db, transport, messaging.OutboxConfig{PollInterval: time.Second,
		BatchSize: 10, MaxAttempts: 3, CleanupEnabled: true, Retention: time.Hour, CleanupInterval: time.Hour}, quiet)
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	waitFor(t, func() bool {
		var n int
		_ = db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_messages`).Scan(&n)
		return n == 1
	})
	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

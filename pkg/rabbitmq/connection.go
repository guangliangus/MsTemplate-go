package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// ErrNotConnected is returned by Ping while the broker is unreachable.
var ErrNotConnected = errors.New("rabbitmq: not connected")

// Connection owns one AMQP connection and re-establishes it whenever the broker closes it. The
// official client has no automatic recovery, so this is where it lives; publishers and consumers
// ask for channels and are re-created on top of the new connection.
type Connection struct {
	cfg  Config
	name string
	log  *slog.Logger

	mu    sync.RWMutex
	conn  *amqp.Connection
	ready chan struct{}
}

// NewConnection prepares the connection; Run dials it.
func NewConnection(cfg Config, connectionName string, log *slog.Logger) *Connection {
	return &Connection{cfg: cfg, name: connectionName, log: log, ready: make(chan struct{})}
}

// Run dials, watches for closure and redials until ctx is cancelled.
func (c *Connection) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		conn, err := c.dial(ctx)
		if err != nil {
			return err // only when ctx ended
		}
		closed := conn.NotifyClose(make(chan *amqp.Error, 1))
		c.setConnection(conn)
		c.log.Info("rabbitmq connected", slog.String("connection", c.name))

		select {
		case <-ctx.Done():
			c.setConnection(nil)
			if err := conn.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
				c.log.Warn("rabbitmq close failed", slog.Any("error", err))
			}
			c.log.Info("rabbitmq connection closed")
			return nil
		case amqpErr := <-closed:
			c.setConnection(nil)
			c.log.Warn("rabbitmq connection lost; reconnecting",
				slog.Any("error", amqpErr), slog.Duration("after", c.cfg.ReconnectDelay))
			wait(ctx, c.cfg.ReconnectDelay)
		}
	}
	return nil
}

func (c *Connection) dial(ctx context.Context) (*amqp.Connection, error) {
	for {
		conn, err := amqp.DialConfig(c.cfg.URL, amqp.Config{
			Heartbeat:  c.cfg.Heartbeat,
			Properties: amqp.Table{"connection_name": c.name},
		})
		if err == nil {
			return conn, nil
		}
		c.log.Warn("rabbitmq dial failed; retrying",
			slog.Any("error", err), slog.Duration("after", c.cfg.ReconnectDelay))
		if !wait(ctx, c.cfg.ReconnectDelay) {
			return nil, ctx.Err()
		}
	}
}

func (c *Connection) setConnection(conn *amqp.Connection) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = conn
	if conn != nil {
		close(c.ready)
		return
	}
	c.ready = make(chan struct{})
}

// WaitReady blocks until the connection is up or ctx ends.
func (c *Connection) WaitReady(ctx context.Context) error {
	c.mu.RLock()
	ready := c.ready
	c.mu.RUnlock()
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Channel opens a channel on the live connection, waiting for one if necessary.
func (c *Connection) Channel(ctx context.Context) (*amqp.Channel, error) {
	if err := c.WaitReady(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || conn.IsClosed() {
		return nil, ErrNotConnected
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("rabbitmq: open channel: %w", err)
	}
	return ch, nil
}

// Connected reports whether the connection is up.
func (c *Connection) Connected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn != nil && !c.conn.IsClosed()
}

// Ping is the readiness check.
func (c *Connection) Ping(_ context.Context) error {
	if !c.Connected() {
		return ErrNotConnected
	}
	return nil
}

func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

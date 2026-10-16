// Package httpserver runs the host's HTTP listeners with the timeouts a public-facing service needs
// and a graceful stop that drains in-flight requests within a bounded share of the shutdown budget.
//
// A host has two listeners: the API on Addr, and the operational endpoints (probes and /metrics) on
// AdminAddr. Keeping them apart means the ingress can only ever route to the API, a saturated API
// cannot starve the kubelet's probes, and a worker serves nothing but the admin listener.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// adminTimeout bounds every phase of an operational request: probes and scrapes are small.
const adminTimeout = 10 * time.Second

// Config is bound from the "server" section.
type Config struct {
	// Addr is the API listen address (":8080").
	Addr string `koanf:"addr" validate:"required"`
	// AdminAddr is the listen address of the probes and /metrics (":8081"). The chart points the
	// probes and the ServiceMonitor at it and the ingress never does.
	AdminAddr string `koanf:"admin_addr" validate:"required"`
	// ReadHeaderTimeout bounds slowloris-style header dribbling.
	ReadHeaderTimeout time.Duration `koanf:"read_header_timeout" validate:"min=0"`
	// ReadTimeout bounds reading the whole request, body included.
	ReadTimeout time.Duration `koanf:"read_timeout" validate:"min=0"`
	// WriteTimeout bounds writing the response; it must exceed RequestTimeout, or the connection is
	// cut before the 504 can be written.
	WriteTimeout time.Duration `koanf:"write_timeout" validate:"min=0"`
	// IdleTimeout bounds keep-alive idleness.
	IdleTimeout time.Duration `koanf:"idle_timeout" validate:"min=0"`
	// ShutdownTimeout is the whole host's stop budget after SIGTERM, shared by every component in
	// stop order. preStop + ShutdownTimeout + the telemetry flush must fit in the pod's
	// terminationGracePeriodSeconds, because the grace period starts counting before preStop.
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout" validate:"min=1s"`
	// DrainTimeout is the API listener's share of ShutdownTimeout: how long in-flight requests may
	// finish. It must be below ShutdownTimeout, so the workers stopping after the HTTP server keep
	// a share of the budget instead of being abandoned.
	DrainTimeout time.Duration `koanf:"drain_timeout" validate:"min=1s"`
	// RequestTimeout is the per-request deadline the handlers see on their context; a handler that
	// honours it answers 504 REQUEST_TIMEOUT. Zero disables it.
	RequestTimeout time.Duration `koanf:"request_timeout" validate:"min=0"`
	// MaxBodyBytes caps request bodies (413 above it).
	MaxBodyBytes int64 `koanf:"max_body_bytes" validate:"min=1"`
}

// Validate keeps the timeouts and the two listeners consistent.
func (c *Config) Validate() error {
	if c.WriteTimeout > 0 && c.RequestTimeout > 0 && c.WriteTimeout <= c.RequestTimeout {
		return fmt.Errorf("write_timeout (%s) must exceed request_timeout (%s)", c.WriteTimeout, c.RequestTimeout)
	}
	if c.DrainTimeout >= c.ShutdownTimeout {
		return fmt.Errorf("drain_timeout (%s) must be below shutdown_timeout (%s): the components stopping "+
			"after the HTTP server need a share of the budget", c.DrainTimeout, c.ShutdownTimeout)
	}
	if c.AdminAddr == c.Addr {
		return fmt.Errorf("admin_addr (%s) must differ from addr: probes and metrics never share the API listener",
			c.AdminAddr)
	}
	return nil
}

// Server is a lifecycle component around http.Server.
type Server struct {
	name  string
	srv   *http.Server
	addr  string
	drain time.Duration
	log   *slog.Logger
	bound atomic.Pointer[net.Addr]
}

// New builds the API server on cfg.Addr, draining within cfg.DrainTimeout.
func New(cfg Config, handler http.Handler, log *slog.Logger) *Server {
	return &Server{
		name:  "api",
		addr:  cfg.Addr,
		drain: cfg.DrainTimeout,
		log:   log,
		srv: &http.Server{
			Addr:              cfg.Addr,
			Handler:           handler,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			ReadTimeout:       cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
		},
	}
}

// NewAdmin builds the operational server on cfg.AdminAddr. Register it with the lifecycle group
// before everything else, so it stops last: metrics stay scrapeable and readiness keeps answering
// 503 while the rest of the host drains.
func NewAdmin(cfg Config, handler http.Handler, log *slog.Logger) *Server {
	return &Server{
		name:  "admin",
		addr:  cfg.AdminAddr,
		drain: time.Second,
		log:   log,
		srv: &http.Server{
			Addr:              cfg.AdminAddr,
			Handler:           handler,
			ReadHeaderTimeout: adminTimeout,
			ReadTimeout:       adminTimeout,
			WriteTimeout:      adminTimeout,
			IdleTimeout:       cfg.IdleTimeout,
		},
	}
}

// Run listens until ctx is cancelled, then drains within the server's drain budget.
func (s *Server) Run(ctx context.Context) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("httpserver %s: listen %s: %w", s.name, s.addr, err)
	}
	bound := ln.Addr()
	s.bound.Store(&bound)
	s.log.Info("http server listening", slog.String("server", s.name), slog.String("addr", ln.Addr().String()))

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("httpserver %s: serve: %w", s.name, err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.drain)
	defer cancel()
	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		// Requests still running past the drain budget are cut, so the rest of the host keeps its share.
		_ = s.srv.Close()
		<-errCh
		return fmt.Errorf("httpserver %s: drain: %w", s.name, err)
	}
	<-errCh
	s.log.Info("http server stopped", slog.String("server", s.name))
	return nil
}

// Addr is the bound address once Run has started listening, nil before (useful with ":0" in tests).
func (s *Server) Addr() net.Addr {
	if a := s.bound.Load(); a != nil {
		return *a
	}
	return nil
}

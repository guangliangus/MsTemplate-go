// Package config is the service's typed configuration: the platform sections from the kit plus
// the service's own, bound from the embedded YAML defaults, the APP_ENV overlay and the
// environment (see pkg/conf for the rules), and validated before anything starts.
package config

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"example.com/msvcservice/pkg/auth"
	"example.com/msvcservice/pkg/cache"
	"example.com/msvcservice/pkg/conf"
	"example.com/msvcservice/pkg/httpclient"
	"example.com/msvcservice/pkg/httpserver"
	"example.com/msvcservice/pkg/logging"
	"example.com/msvcservice/pkg/messaging"
	"example.com/msvcservice/pkg/middleware"
	"example.com/msvcservice/pkg/postgres"
	"example.com/msvcservice/pkg/rabbitmq"
	"example.com/msvcservice/pkg/redisx"
	"example.com/msvcservice/pkg/taskqueue"
	"example.com/msvcservice/pkg/telemetry"
)

//go:embed default.yaml development.yaml
var files embed.FS

// EnvVar selects the environment overlay and names the environment in logs and traces.
const EnvVar = "APP_ENV"

// DefaultEnvironment applies when APP_ENV is unset.
const DefaultEnvironment = "production"

// Config is the whole configuration tree.
type Config struct {
	Service        ServiceConfig                `koanf:"service"`
	Server         httpserver.Config            `koanf:"server"`
	HTTP           HTTPConfig                   `koanf:"http"`
	Logging        logging.Config               `koanf:"logging"`
	Telemetry      telemetry.Config             `koanf:"telemetry"`
	Postgres       postgres.Config              `koanf:"postgres"`
	Redis          redisx.Config                `koanf:"redis"`
	Cache          cache.Config                 `koanf:"cache"`
	RabbitMQ       RabbitMQConfig               `koanf:"rabbitmq"`
	Outbox         messaging.OutboxConfig       `koanf:"outbox"`
	Tasks          taskqueue.Config             `koanf:"tasks"`
	Auth           auth.Config                  `koanf:"auth"`
	ServiceClients map[string]httpclient.Config `koanf:"service_clients"`
	Orders         OrdersConfig                 `koanf:"orders"`

	// Environment is APP_ENV, not a YAML key.
	Environment string `koanf:"-"`
}

// ServiceConfig is bound from "service".
type ServiceConfig struct {
	// Name is the service's short name: queue prefix, telemetry service.name, log attribute.
	Name string `koanf:"name" validate:"required"`
}

// HTTPConfig is bound from "http".
type HTTPConfig struct {
	ForwardedHeaders   middleware.ForwardedConfig   `koanf:"forwarded_headers"`
	CORS               middleware.CORSConfig        `koanf:"cors"`
	RateLimiting       middleware.RateLimitConfig   `koanf:"rate_limiting"`
	Idempotency        middleware.IdempotencyConfig `koanf:"idempotency"`
	RequiredHeaders    RequiredHeadersConfig        `koanf:"required_headers"`
	ProblemTypeBaseURI string                       `koanf:"problem_type_base_uri" validate:"required"`
	DocsEnabled        bool                         `koanf:"docs_enabled"`
}

// RequiredHeadersConfig is bound from "http.required_headers": the platform headers each plane's
// routes refuse to work without (400 MISSING_HEADERS). Empty lists enforce nothing.
type RequiredHeadersConfig struct {
	Consumer   []string `koanf:"consumer"`
	BackOffice []string `koanf:"backoffice"`
}

// RabbitMQConfig is the kit's broker config plus the host-shape switch.
type RabbitMQConfig struct {
	rabbitmq.Config `koanf:",squash"`
	// ConsumersEnabled runs the consumers on this host. The queue names do not depend on the host
	// mode, so an API deployment beside a worker deployment sets this false on the API pods.
	ConsumersEnabled bool `koanf:"consumers_enabled"`
}

// OrdersConfig is bound from "orders" (the reference module's own knobs).
type OrdersConfig struct {
	// PendingTimeout is the age at which the reaper cancels a still-PENDING order.
	PendingTimeout time.Duration `koanf:"pending_timeout" validate:"min=1m"`
}

// Load reads the environment name, the embedded YAML and the process environment.
func Load() (*Config, error) {
	env := strings.ToLower(strings.TrimSpace(os.Getenv(EnvVar)))
	if env == "" {
		env = DefaultEnvironment
	}
	return load(env, os.Environ)
}

func load(env string, environ func() []string) (*Config, error) {
	defaults, err := files.ReadFile("default.yaml")
	if err != nil {
		return nil, fmt.Errorf("config: embedded defaults: %w", err)
	}
	opts := conf.Options{Defaults: defaults, Environ: environ}
	overlay, err := files.ReadFile(env + ".yaml")
	switch {
	case err == nil:
		opts.Overlays = [][]byte{overlay}
	case errors.Is(err, fs.ErrNotExist):
		// an environment with no overlay runs on the defaults plus the environment variables
	default:
		return nil, fmt.Errorf("config: overlay %s.yaml: %w", env, err)
	}
	cfg, err := conf.Load[Config](opts)
	if err != nil {
		return nil, err
	}
	cfg.Environment = env
	return cfg, nil
}

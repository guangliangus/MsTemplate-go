// Package config is the service's typed configuration: the platform sections from the kit plus
// the service's own, bound from the embedded YAML defaults, the APP_ENV overlay and the
// environment (see the kit's conf package for the rules), and validated before anything starts.
package config

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/guangliangus/go-msvc-kit/auth"
	"github.com/guangliangus/go-msvc-kit/cache"
	"github.com/guangliangus/go-msvc-kit/conf"
	"github.com/guangliangus/go-msvc-kit/httpclient"
	"github.com/guangliangus/go-msvc-kit/httpserver"
	"github.com/guangliangus/go-msvc-kit/kafka"
	"github.com/guangliangus/go-msvc-kit/logging"
	"github.com/guangliangus/go-msvc-kit/messaging"
	"github.com/guangliangus/go-msvc-kit/middleware"
	"github.com/guangliangus/go-msvc-kit/postgres"
	"github.com/guangliangus/go-msvc-kit/rabbitmq"
	"github.com/guangliangus/go-msvc-kit/redisx"
	"github.com/guangliangus/go-msvc-kit/taskqueue"
	"github.com/guangliangus/go-msvc-kit/telemetry"
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
	Kafka          KafkaConfig                  `koanf:"kafka"`
	Outbox         messaging.OutboxConfig       `koanf:"outbox"`
	Tasks          taskqueue.Config             `koanf:"tasks"`
	Auth           auth.Config                  `koanf:"auth"`
	ServiceClients map[string]httpclient.Config `koanf:"service_clients"`
	Orders         OrdersConfig                 `koanf:"orders"`

	// Environment is APP_ENV, not a YAML key.
	Environment string `koanf:"-"`
}

// Validate holds the rules that are this service's, not the kit's. The kit no longer demands a
// signing key of every service (one that serves only cluster-internal routes has no tokens to
// check); this one serves user-svc's bearer tokens on both planes, so a deployment without the
// key is refused at load time, before anything is opened, rather than when the validator is built.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Auth.SigningKey) == "" {
		return errors.New("auth.signing_key is required: this service validates user-svc tokens (AUTH__SIGNING_KEY)")
	}
	return nil
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

// KafkaConfig is the kit's Kafka config plus which events live on Kafka and the host-shape switch.
type KafkaConfig struct {
	kafka.Config `koanf:",squash"`
	// Events are the event names that live on Kafka: published to it and consumed from it. Every
	// other event stays on RabbitMQ. A producer and all of an event's consumers must agree, so an
	// event moves bus by a platform decision, never by one service's configuration alone.
	Events []string `koanf:"events"`
	// ConsumersEnabled runs the Kafka consumers on this host; false on API pods beside a worker
	// deployment, as for RabbitMQ.
	ConsumersEnabled bool `koanf:"consumers_enabled"`
}

// Validate checks the kit's rules and the event list: an event on Kafka while Kafka is off would
// never be published, and an event name must be a legal topic unless kafka.topics maps it.
func (c *KafkaConfig) Validate() error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if len(c.Events) > 0 && !c.Enabled {
		return fmt.Errorf("events %v are routed to Kafka but kafka.enabled is false", c.Events)
	}
	seen := map[string]bool{}
	for _, event := range c.Events {
		if seen[event] {
			return fmt.Errorf("event %q is listed twice", event)
		}
		seen[event] = true
		if err := kafka.ValidateTopicName(c.TopicFor(event)); err != nil {
			return fmt.Errorf("event %q: %w", event, err)
		}
	}
	return nil
}

// OnKafka reports whether an event lives on Kafka (false for every event while Kafka is off).
func (c *KafkaConfig) OnKafka(event string) bool {
	return c.Enabled && slices.Contains(c.Events, event)
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

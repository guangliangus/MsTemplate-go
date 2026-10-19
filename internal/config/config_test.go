package config

import (
	"strings"
	"testing"
)

// What a production deployment supplies from Secrets and the config map; default.yaml carries
// none of it on purpose.
var productionEnv = []string{
	"POSTGRES__DSN=postgres://u:p@db:5432/x",
	"REDIS__ADDR=redis:6379",
	"RABBITMQ__URL=amqp://u:p@broker:5672/",
	"AUTH__SIGNING_KEY=ZGV2LW9ubHktYWNjZXNzLXRva2VuLWtleS0zMmJ5dGU=",
}

// The embedded defaults must pass every cross-field rule (drain below the shutdown budget, task
// timeout below stale_after, separate listeners, ...): a broken default is a refused boot on the
// next deploy. Production needs the environment above; development runs on its overlay alone.
func TestEmbeddedDefaultsValidate(t *testing.T) {
	for _, tc := range []struct {
		env     string
		environ []string
	}{
		{"production", productionEnv},
		{"development", nil},
		{"staging", productionEnv}, // no overlay: the defaults plus the environment
	} {
		cfg, err := load(tc.env, func() []string { return tc.environ })
		if err != nil {
			t.Fatalf("%s: %v", tc.env, err)
		}
		if cfg.Server.AdminAddr == "" || cfg.Server.DrainTimeout >= cfg.Server.ShutdownTimeout {
			t.Fatalf("%s: %+v", tc.env, cfg.Server)
		}
		if cfg.Environment != tc.env {
			t.Fatalf("environment recorded as %q", cfg.Environment)
		}
	}
}

// default.yaml is the production baseline, so it must not carry a localhost anything: a pod
// whose Secret was forgotten refuses to boot instead of starting against a database that is
// not there.
func TestProductionRefusesToBootWithoutItsConnections(t *testing.T) {
	for _, missing := range []string{"POSTGRES__DSN", "REDIS__ADDR", "RABBITMQ__URL", "AUTH__SIGNING_KEY"} {
		var environ []string
		for _, kv := range productionEnv {
			if !strings.HasPrefix(kv, missing+"=") {
				environ = append(environ, kv)
			}
		}
		if _, err := load("production", func() []string { return environ }); err == nil {
			t.Errorf("production without %s must be refused", missing)
		}
	}
}

func TestDevelopmentOverlayOnlyHoldsTheLocalStack(t *testing.T) {
	cfg, err := load("development", func() []string { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"postgres.dsn": cfg.Postgres.DSN, "redis.addr": cfg.Redis.Addr, "rabbitmq.url": cfg.RabbitMQ.URL,
	} {
		if !strings.Contains(value, "localhost") {
			t.Errorf("%s = %q: the development overlay should point at the compose stack", name, value)
		}
	}
	if !cfg.HTTP.DocsEnabled || !cfg.Auth.DevHeaders.Enabled {
		t.Fatal("the development conveniences come from the overlay")
	}
}

func TestAdditionalSigningKeysArriveFromTheEnvironment(t *testing.T) {
	cfg, err := load("production", func() []string {
		return append(productionEnv, "AUTH__ADDITIONAL_SIGNING_KEYS=dGhlLW5leHQta2V5LXVzZXItc3ZjLXNpZ25zLXdpdGgh")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth.AdditionalSigningKeys) != 1 {
		t.Fatalf("%v", cfg.Auth.AdditionalSigningKeys)
	}
}

// The drains every component declares add up in stop order (lifecycle.WithDrain); the default
// shape runs all of them, Kafka's included once a service turns it on, and their sum must leave
// the components stopping last (outbox, connections, admin) something of shutdown_timeout —
// otherwise every start logs that they would be abandoned.
func TestDeclaredDrainsFitTheShutdownBudget(t *testing.T) {
	cfg, err := load("production", func() []string { return productionEnv })
	if err != nil {
		t.Fatal(err)
	}
	drains := cfg.Server.DrainTimeout + cfg.RabbitMQ.DrainTimeout + cfg.Tasks.DrainTimeout + cfg.Kafka.DrainTimeout
	if drains >= cfg.Server.ShutdownTimeout {
		t.Fatalf("declared drains %s reach shutdown_timeout %s", drains, cfg.Server.ShutdownTimeout)
	}
}

func TestKafkaRoutesOnlyItsEvents(t *testing.T) {
	kafkaOn := append([]string{}, productionEnv...)
	kafkaOn = append(kafkaOn, "KAFKA__ENABLED=true", "KAFKA__BROKERS=k1:9092,k2:9092", "KAFKA__EVENTS=orders.order-created.v1")
	cfg, err := load("production", func() []string { return kafkaOn })
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Kafka.Brokers) != 2 || !cfg.Kafka.OnKafka("orders.order-created.v1") || cfg.Kafka.OnKafka("orders.other.v1") {
		t.Fatalf("routing: %+v", cfg.Kafka)
	}
	defaults, err := load("production", func() []string { return productionEnv })
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Kafka.Enabled || defaults.Kafka.OnKafka("orders.order-created.v1") {
		t.Fatal("Kafka is off by default and every event stays on RabbitMQ")
	}
}

func TestKafkaEventListIsValidated(t *testing.T) {
	for name, extra := range map[string][]string{
		"events while disabled":   {"KAFKA__EVENTS=orders.order-created.v1"},
		"enabled without brokers": {"KAFKA__ENABLED=true"},
		"illegal topic name":      {"KAFKA__ENABLED=true", "KAFKA__BROKERS=k:9092", "KAFKA__EVENTS=orders/created"},
		"listed twice": {"KAFKA__ENABLED=true", "KAFKA__BROKERS=k:9092",
			"KAFKA__EVENTS=orders.order-created.v1,orders.order-created.v1"},
	} {
		environ := append(append([]string{}, productionEnv...), extra...)
		if _, err := load("production", func() []string { return environ }); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

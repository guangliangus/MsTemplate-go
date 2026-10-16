package conf

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type nested struct {
	Addr    string          `koanf:"addr" validate:"required"`
	Timeout time.Duration   `koanf:"timeout" validate:"min=1s"`
	Delays  []time.Duration `koanf:"delays"`
	Flags   map[string]int  `koanf:"flags"`
}

func (n *nested) Validate() error {
	if n.Timeout > time.Hour {
		return errors.New("timeout must be at most 1h")
	}
	return nil
}

type testConfig struct {
	Name   string `koanf:"name" validate:"required"`
	Server nested `koanf:"server"`
}

const defaults = `
name: svc
server:
  addr: ":8080"
  timeout: 5s
  delays: [1s, 2s]
  flags: { a: 1 }
`

func environ(vars ...string) func() []string {
	return func() []string { return vars }
}

func TestLoadDefaultsOverlayAndEnvironment(t *testing.T) {
	cfg, err := Load[testConfig](Options{
		Defaults: []byte(defaults),
		Overlays: [][]byte{[]byte("server:\n  timeout: 10s\n")},
		Environ:  environ("SERVER__ADDR=:9090", "SERVER__DELAYS=30s,2m", "SERVER__FLAGS__B=2", "PATH=/usr/bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("env override lost: %q", cfg.Server.Addr)
	}
	if cfg.Server.Timeout != 10*time.Second {
		t.Errorf("overlay lost: %s", cfg.Server.Timeout)
	}
	if len(cfg.Server.Delays) != 2 || cfg.Server.Delays[1] != 2*time.Minute {
		t.Errorf("list from env not parsed: %v", cfg.Server.Delays)
	}
	if cfg.Server.Flags["a"] != 1 || cfg.Server.Flags["b"] != 2 {
		t.Errorf("map keys from env not merged: %v", cfg.Server.Flags)
	}
}

func TestVariablesWithoutSeparatorAreIgnored(t *testing.T) {
	cfg, err := Load[testConfig](Options{Defaults: []byte(defaults), Environ: environ("NAME=hijacked")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "svc" {
		t.Fatalf("a variable without __ must not override: %q", cfg.Name)
	}
}

func TestValidationReportsKoanfPaths(t *testing.T) {
	_, err := Load[testConfig](Options{Defaults: []byte(defaults), Environ: environ("SERVER__ADDR=", "SERVER__TIMEOUT=2h")})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	msg := err.Error()
	for _, want := range []string{"server.addr: failed 'required'", "server: timeout must be at most 1h"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

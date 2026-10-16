package config

import (
	"testing"
)

// The embedded defaults must pass every cross-field rule (drain below the shutdown budget, task
// timeout below stale_after, separate listeners, ...) with only the secrets supplied, in each
// environment: a broken default is a refused boot on the next deploy.
func TestEmbeddedDefaultsValidate(t *testing.T) {
	secrets := []string{
		"POSTGRES__DSN=postgres://u:p@db:5432/x",
		"AUTH__SIGNING_KEY=ZGV2LW9ubHktYWNjZXNzLXRva2VuLWtleS0zMmJ5dGU=",
	}
	for _, env := range []string{"production", "development"} {
		cfg, err := load(env, func() []string { return secrets })
		if err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		if cfg.Server.AdminAddr == "" || cfg.Server.DrainTimeout >= cfg.Server.ShutdownTimeout {
			t.Fatalf("%s: %+v", env, cfg.Server)
		}
	}
}

func TestAdditionalSigningKeysArriveFromTheEnvironment(t *testing.T) {
	cfg, err := load("production", func() []string {
		return []string{
			"POSTGRES__DSN=postgres://u:p@db:5432/x",
			"AUTH__SIGNING_KEY=ZGV2LW9ubHktYWNjZXNzLXRva2VuLWtleS0zMmJ5dGU=",
			"AUTH__ADDITIONAL_SIGNING_KEYS=dGhlLW5leHQta2V5LXVzZXItc3ZjLXNpZ25zLXdpdGgh",
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth.AdditionalSigningKeys) != 1 {
		t.Fatalf("%v", cfg.Auth.AdditionalSigningKeys)
	}
}

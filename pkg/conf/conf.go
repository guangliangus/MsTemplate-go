// Package conf loads typed configuration the way a Kubernetes deployment supplies it: YAML defaults
// embedded in the binary, an optional per-environment YAML overlay, then environment variables.
//
// An environment variable overrides one key when its name is the key path upper-cased with "__"
// between the levels: POSTGRES__DSN sets postgres.dsn, TASKS__RECURRING__HEARTBEAT sets
// tasks.recurring.heartbeat. Variables without "__" are ignored, so an unrelated PATH or HOME can
// never land in the configuration. List values arrive comma separated (30s,2m,10m).
//
// Everything is validated before the host starts (validate struct tags plus any Validate method,
// nested sections included) so a misconfiguration is a refused boot, not a 500 on the first request.
package conf

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

const (
	// Delim separates key levels in the flattened configuration map.
	Delim = "."
	// EnvSeparator separates key levels in an environment variable name.
	EnvSeparator = "__"
	// Tag is the struct tag the loader binds by.
	Tag = "koanf"
)

// Options describes the layers to load, lowest precedence first.
type Options struct {
	// Defaults is the YAML every deployment starts from (required).
	Defaults []byte
	// Overlays are YAML documents applied on top of Defaults, in order (per-environment files).
	Overlays [][]byte
	// Environ supplies the environment; nil means os.Environ.
	Environ func() []string
}

// Load builds a T from the layers and validates it.
func Load[T any](opts Options) (*T, error) {
	if len(opts.Defaults) == 0 {
		return nil, errors.New("conf: Defaults is required")
	}

	k := koanf.New(Delim)
	if err := k.Load(rawbytes.Provider(opts.Defaults), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("conf: load defaults: %w", err)
	}
	for i, overlay := range opts.Overlays {
		if len(overlay) == 0 {
			continue
		}
		if err := k.Load(rawbytes.Provider(overlay), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("conf: load overlay %d: %w", i, err)
		}
	}
	envProvider := env.Provider(Delim, env.Opt{EnvironFunc: opts.Environ, TransformFunc: transformEnv})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("conf: load environment: %w", err)
	}

	cfg := new(T)
	decoder := &mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			stringToSliceHook,
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.TextUnmarshallerHookFunc(),
		),
		WeaklyTypedInput: true,
		TagName:          Tag,
	}
	if err := k.UnmarshalWithConf("", cfg, koanf.UnmarshalConf{Tag: Tag, DecoderConfig: decoder}); err != nil {
		return nil, fmt.Errorf("conf: bind: %w", err)
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// transformEnv maps POSTGRES__MAX_CONNS to postgres.max_conns and drops everything without the
// separator; an empty key tells the provider to skip the variable.
func transformEnv(key, value string) (string, any) {
	if !strings.Contains(key, EnvSeparator) {
		return "", nil
	}
	return strings.ToLower(strings.ReplaceAll(key, EnvSeparator, Delim)), value
}

// stringToSliceHook turns "30s, 2m" from the environment into the list a YAML default would be;
// elements are trimmed, so hand-typed spacing does not matter.
func stringToSliceHook(from, to reflect.Type, data any) (any, error) {
	if from.Kind() != reflect.String || to.Kind() != reflect.Slice {
		return data, nil
	}
	raw, _ := data.(string)
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, nil
}

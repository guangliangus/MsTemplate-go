// Package logging builds the service's slog.Logger: JSON on stdout for the cluster's log collector
// (text locally), trace and span ids on every line written with a traced context, and masking of
// sensitive attribute values.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// Config is bound from the "logging" section.
type Config struct {
	// Level is debug, info, warn or error.
	Level string `koanf:"level" validate:"omitempty,oneof=debug info warn error"`
	// Format is json (default, for collectors) or text (local readability).
	Format string `koanf:"format" validate:"omitempty,oneof=json text"`
	// MaskedFields are attribute keys whose values are replaced with "***" (case-insensitive).
	// Empty means DefaultMaskedFields.
	MaskedFields []string `koanf:"masked_fields"`
}

// DefaultMaskedFields are masked when the configuration names none.
var DefaultMaskedFields = []string{
	"password", "secret", "token", "authorization", "client_secret", "signing_key", "api_key",
}

// New builds the logger. The attrs are attached to every line (service, version, environment).
func New(cfg Config, w io.Writer, attrs ...slog.Attr) *slog.Logger {
	opts := &slog.HandlerOptions{Level: ParseLevel(cfg.Level)}
	var h slog.Handler
	if cfg.Format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	masked := cfg.MaskedFields
	if len(masked) == 0 {
		masked = DefaultMaskedFields
	}
	h = NewMaskingHandler(h, masked)
	h = NewTraceHandler(h)
	logger := slog.New(h)
	if len(attrs) > 0 {
		args := make([]any, 0, len(attrs))
		for _, a := range attrs {
			args = append(args, a)
		}
		logger = logger.With(args...)
	}
	return logger
}

// ParseLevel maps a level name to slog.Level; unknown names mean info.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

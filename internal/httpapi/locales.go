package httpapi

import (
	"embed"

	"github.com/guangliangus/go-msvc-kit/i18n"
)

// locales carries this service's own error codes in every language the platform serves; the
// platform codes come with the kit. A guard test keeps the two complete.
//
//go:embed locales/*.json
var locales embed.FS

// Catalog is the message catalogue the error writer localizes problem titles from: the platform's
// codes plus this service's.
func Catalog() (*i18n.Catalog, error) {
	return i18n.PlatformWith(locales, "locales")
}

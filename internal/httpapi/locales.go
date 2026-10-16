package httpapi

import (
	"embed"

	"example.com/msvcservice/pkg/i18n"
)

// locales carries this service's own error codes in every language the platform serves; the
// platform codes come with the kit. A guard test keeps the two complete.
//
//go:embed locales/*.json
var locales embed.FS

// Catalog is the message catalogue the error writer localizes problem titles from: the platform's
// codes plus this service's.
func Catalog() (*i18n.Catalog, error) {
	c, err := i18n.Platform()
	if err != nil {
		return nil, err
	}
	if err := c.Merge(locales, "locales"); err != nil {
		return nil, err
	}
	return c, nil
}

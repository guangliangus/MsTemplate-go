// Package api embeds the OpenAPI document served at /openapi.yaml (and rendered at /docs) when
// http.docs_enabled is true. The document is written by hand, spec-first; keep it beside the
// handlers it describes.
package api

import _ "embed"

// OpenAPI is the raw YAML document.
//
//go:embed openapi.yaml
var OpenAPI []byte

package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// scalarPage renders the OpenAPI document with Scalar from its CDN; only mounted when
// http.docs_enabled is true (the development overlay), never in production.
const scalarPage = `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>API Reference</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
</head>
<body>
  <script id="api-reference" data-url="/openapi.yaml"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.25.0"></script>
</body>
</html>`

func mountDocs(r chi.Router, spec []byte) {
	r.Get("/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(spec)
	})
	r.Get("/docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(scalarPage))
	})
}

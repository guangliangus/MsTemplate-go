package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"example.com/msvcservice/api"
)

const apiPrefix = "/api/v1"

// The router and api/openapi.yaml describe the same API: every (method, path) served under
// /api/v1 is documented, and nothing documented is unserved. The probes and /metrics live on the
// admin listener and /docs and /openapi.yaml are the document itself, so none of those is compared.
func TestRouterMatchesTheOpenAPIDocument(t *testing.T) {
	served := routerOperations(t, testServer(t))
	documented := documentedOperations(t, string(api.OpenAPI))
	for op := range served {
		if !documented[op] {
			t.Errorf("%s is served but not in api/openapi.yaml", op)
		}
	}
	for op := range documented {
		if !served[op] {
			t.Errorf("%s is in api/openapi.yaml but not served", op)
		}
	}
}

// The parser sees exactly the operations under paths, nothing from other sections, and reports
// a layout it cannot read instead of passing on an empty set.
func TestDocumentedOperationsReadsThePathsSection(t *testing.T) {
	doc := strings.Join([]string{
		"openapi: 3.1.0",
		"paths:",
		"  /api/v1/orders:",
		"    post:",
		"      summary: create",
		"      responses:",
		"        get: not an operation",
		"    get:",
		"      summary: list",
		"  /api/v1/orders/{id}:",
		"    parameters:",
		"      - name: id",
		"    delete:",
		"      summary: cancel",
		"  # a comment between paths",
		"  /health/live:",
		"    get:",
		"      summary: not under the prefix",
		"components:",
		"  schemas:",
		"    get:",
		"      type: object",
	}, "\n")
	got := documentedOperations(t, doc)
	want := []string{"POST /api/v1/orders", "GET /api/v1/orders", "DELETE /api/v1/orders/{id}"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, op := range want {
		if !got[op] {
			t.Errorf("missing %s in %v", op, got)
		}
	}
}

// routerOperations walks the router: chi reports a mounted sub-router's root as "<mount>/", which
// is the document's "<mount>", and keeps {id} as the document writes it.
func routerOperations(t *testing.T, h http.Handler) map[string]bool {
	t.Helper()
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("NewRouter returned %T, not a chi router", h)
	}
	ops := map[string]bool{}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if strings.HasPrefix(route, apiPrefix) {
			ops[method+" "+route] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

var httpMethods = map[string]string{
	"get": http.MethodGet, "post": http.MethodPost, "put": http.MethodPut, "patch": http.MethodPatch,
	"delete": http.MethodDelete, "head": http.MethodHead, "options": http.MethodOptions,
}

// documentedOperations reads the paths section without a YAML library (none is a direct
// dependency): a path is a two-space indented key starting with '/', an operation a four-space
// indented HTTP method under it. The document is hand-written in exactly that shape; a reformat
// that breaks the assumption makes the test fail loudly rather than pass on an empty set.
func documentedOperations(t *testing.T, doc string) map[string]bool {
	t.Helper()
	ops := map[string]bool{}
	inPaths, path := false, ""
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && line != "" && line[0] != ' ':
			inPaths = false // the next top-level section
		case inPaths && strings.HasPrefix(line, "  /"):
			path = strings.TrimSuffix(strings.TrimSpace(line), ":")
		case inPaths && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     "):
			key := strings.TrimSuffix(strings.TrimSpace(line), ":")
			if method, ok := httpMethods[key]; ok && strings.HasPrefix(path, apiPrefix) {
				ops[method+" "+path] = true
			}
		}
	}
	if len(ops) == 0 {
		t.Fatal("no operations found under paths: in api/openapi.yaml; the parser's layout assumption no longer holds")
	}
	return ops
}

// Package httpapi is the service's HTTP surface: the API router with the platform's middleware in
// its load-bearing order, the API docs, the handlers and their wire types, and — on its own
// listener — the admin router with the probes and metrics. Handlers return errors; nothing here
// writes a problem by hand.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"example.com/msvcservice/internal/config"
	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/pkg/auth"
	"example.com/msvcservice/pkg/health"
	"example.com/msvcservice/pkg/httpx"
	"example.com/msvcservice/pkg/idempotency"
	"example.com/msvcservice/pkg/middleware"
)

// Deps is everything the API router needs.
type Deps struct {
	Config      *config.Config
	Log         *slog.Logger
	ErrorWriter *httpx.ErrorWriter
	Auth        *auth.Validator
	DevHeaders  *auth.DevHeaders
	Idempotency idempotency.Store
	Orders      *order.Service
	OpenAPI     []byte
}

// AdminRouter builds the admin listener's surface (server.admin_addr): the three probes and
// /metrics, unauthenticated and never behind the ingress. Both host modes serve it; a worker
// serves nothing else.
func AdminRouter(h *health.Registry, metrics http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Method(http.MethodGet, "/health/startup", h.Startup())
	r.Method(http.MethodGet, "/health/live", h.Live())
	r.Method(http.MethodGet, "/health/ready", h.Ready())
	r.Method(http.MethodGet, "/metrics", metrics)
	return r
}

// NewRouter builds the API listener's handler (server.addr). The probes and metrics are not here:
// whatever the ingress routes to this listener can never reach them.
func NewRouter(d Deps) http.Handler {
	cfg := d.Config
	root := chi.NewRouter()
	if cfg.HTTP.DocsEnabled {
		mountDocs(root, d.OpenAPI)
	}

	limiter := middleware.NewRateLimiter(cfg.HTTP.RateLimiting, d.Log)

	// The router's own 404/405 speak the error contract too. Set on the root BEFORE the group and
	// the mounts, so chi copies them into every sub-router; they take the writer directly because
	// a root-level miss never passes through the group's middlewares.
	root.NotFound(func(w http.ResponseWriter, r *http.Request) {
		d.ErrorWriter.WriteStatus(w, r, http.StatusNotFound, "No route matches "+r.Method+" "+r.URL.Path+".")
	})
	root.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		d.ErrorWriter.WriteStatus(w, r, http.StatusMethodNotAllowed, r.Method+" is not allowed on "+r.URL.Path+".")
	})

	// Middleware ORDER is load-bearing — reorder with care:
	//  1. the client address is resolved first: everything after it reads ClientIP; the platform
	//     headers are captured right after, before anything that calls a downstream;
	//  2. the error writer goes on the context before anything can fail;
	//  3. otelhttp opens the server span, so the trace id exists for the header and the log line;
	//  4. the request log sits OUTSIDE the recoverer: it records the status the client received,
	//     and a handled 400 is never logged as a 500;
	//  5. the request timeout and body cap bound every handler;
	//  6. authentication before rate limiting (partitions are per caller) and both before
	//     idempotency (keys are caller-scoped, and a replay still spends budget).
	root.Group(func(r chi.Router) {
		r.Use(middleware.ClientIPResolver(cfg.HTTP.ForwardedHeaders))
		r.Use(middleware.PlatformHeaders())
		r.Use(middleware.ErrorWriter(d.ErrorWriter))
		r.Use(otelhttp.NewMiddleware("http.server"))
		r.Use(middleware.RouteSpan())
		r.Use(middleware.TraceHeader())
		r.Use(middleware.RequestLogger(d.Log))
		r.Use(middleware.Recoverer(d.Log))
		r.Use(middleware.RequestTimeout(cfg.Server.RequestTimeout))
		r.Use(middleware.MaxBodyBytes(cfg.Server.MaxBodyBytes))
		r.Use(middleware.CORS(cfg.HTTP.CORS))
		r.Use(auth.Authenticate(d.Auth, d.DevHeaders, d.Log))
		r.Use(limiter.Global())
		r.Use(middleware.Idempotency(cfg.HTTP.Idempotency, d.Idempotency, d.Log))

		ordersHandler := NewOrdersHandler(d.Orders)
		r.Route("/api/v1", func(r chi.Router) {
			// Every route names the plane it serves. A back-office operator's token is a valid
			// token on this host; without the realm policy it would read as a consumer id. The two
			// plane groups carry only what every context on that plane shares (realm, required
			// headers); a scope belongs to one context and sits on that context's own routes, so
			// a second context mounted here is never asked for the first one's scope.
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRealm(auth.RealmConsumer),
					middleware.RequireHeaders(cfg.HTTP.RequiredHeaders.Consumer...))
				r.Mount("/orders", ordersHandler.ConsumerRoutes(limiter))
			})
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRealm(auth.RealmBackOffice),
					middleware.RequireHeaders(cfg.HTTP.RequiredHeaders.BackOffice...))
				r.Mount("/backoffice/orders", ordersHandler.BackOfficeRoutes())
			})
		})
	})
	return root
}

package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/config"
	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/internal/order/ordertest"
	"github.com/guangliangus/go-msvc-kit/auth"
	"github.com/guangliangus/go-msvc-kit/health"
	"github.com/guangliangus/go-msvc-kit/httpserver"
	"github.com/guangliangus/go-msvc-kit/httpx"
	"github.com/guangliangus/go-msvc-kit/middleware"
	"github.com/guangliangus/go-msvc-kit/paging"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// testInventory prices everything at 19.99 and knows OUT-OF-STOCK as unavailable.
var testInventory = ordertest.Inventory{
	UnitPrice:   decimal.RequireFromString("19.99"),
	Unavailable: map[string]bool{"OUT-OF-STOCK": true},
}

var rateLimits = middleware.RateLimitConfig{Enabled: true, Global: "global", Policies: map[string]middleware.RateLimitPolicy{
	"global": {Requests: 1000, Window: time.Minute, PartitionBy: "caller"},
	"writes": {Requests: 1000, Window: time.Minute, PartitionBy: "caller"},
}}

func testServer(t *testing.T) http.Handler {
	t.Helper()
	cfg := &config.Config{Environment: "development"}
	cfg.Server.RequestTimeout = 5 * time.Second
	cfg.Server.MaxBodyBytes = 1 << 20
	cfg.HTTP.ProblemTypeBaseURI = "urn:msvc:error:"
	cfg.HTTP.RateLimiting = rateLimits
	cfg.Auth = auth.Config{Issuer: "user-svc", SigningKey: base64.StdEncoding.EncodeToString([]byte("dev-only-access-token-key-32byte")),
		DevHeaders: auth.DevHeadersConfig{Enabled: true}}
	validator, err := auth.NewValidator(cfg.Auth)
	if err != nil {
		t.Fatal(err)
	}
	svc, _, _ := ordertest.NewService(testInventory)
	return NewRouter(Deps{
		Config:      cfg,
		Log:         quiet,
		ErrorWriter: localizedWriter(t, cfg.HTTP.ProblemTypeBaseURI),
		Auth:        validator,
		DevHeaders:  auth.NewDevHeaders(cfg.Auth.DevHeaders, cfg.Environment, quiet),
		Orders:      svc,
	})
}

func call(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "10.1.1.1:5000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func problem(t *testing.T, rec *httptest.ResponseRecorder) httpx.Problem {
	t.Helper()
	var p httpx.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem: %s", rec.Body.String())
	}
	return p
}

var consumer = map[string]string{auth.HeaderDevSubject: "1", auth.HeaderDevRealm: auth.RealmConsumer}

func TestOperationalEndpointsLiveOnlyOnTheAdminListener(t *testing.T) {
	registry := health.New(time.Second)
	registry.MarkStarted()
	admin := httpserver.AdminHandler(registry, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# metrics\n"))
	}))
	api := testServer(t)
	for _, path := range []string{"/health/startup", "/health/live", "/health/ready", "/metrics"} {
		if rec := call(admin, http.MethodGet, path, "", nil); rec.Code != 200 {
			t.Errorf("admin %s: %d", path, rec.Code)
		}
		// Whatever the ingress routes to the API listener never reaches them.
		if rec := call(api, http.MethodGet, path, "", nil); rec.Code != 404 {
			t.Errorf("api %s: %d", path, rec.Code)
		}
	}
}

func TestAuthenticationAndPlanes(t *testing.T) {
	h := testServer(t)
	if p := problem(t, call(h, http.MethodGet, "/api/v1/orders", "", nil)); p.Status != 401 || p.ErrorCode != "UNAUTHORIZED" {
		t.Fatalf("%+v", p)
	}
	operator := map[string]string{auth.HeaderDevSubject: "1", auth.HeaderDevRealm: auth.RealmBackOffice, auth.HeaderDevScopes: "orders.read"}
	if p := problem(t, call(h, http.MethodGet, "/api/v1/orders", "", operator)); p.Status != 403 {
		t.Fatalf("operator on the consumer plane: %+v", p)
	}
	if rec := call(h, http.MethodGet, "/api/v1/backoffice/orders", "", operator); rec.Code != 200 {
		t.Fatalf("operator on the back-office plane: %d %s", rec.Code, rec.Body.String())
	}
	noScope := map[string]string{auth.HeaderDevSubject: "1", auth.HeaderDevRealm: auth.RealmBackOffice}
	if p := problem(t, call(h, http.MethodGet, "/api/v1/backoffice/orders", "", noScope)); p.Status != 403 {
		t.Fatalf("scope enforced: %+v", p)
	}
}

// The handlers never dereference a missing principal: a route mounted without RequireRealm by
// mistake answers 401 instead of panicking.
func TestHandlersRefuseARequestWithoutAPrincipal(t *testing.T) {
	svc, _, _ := ordertest.NewService(testInventory)
	routes := NewOrdersHandler(svc).ConsumerRoutes(middleware.NewRateLimiter(rateLimits, quiet))
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/", `{"items":[{"sku":"A","quantity":1}]}`},
		{http.MethodGet, "/", ""},
		{http.MethodGet, "/1", ""},
		{http.MethodDelete, "/1", ""},
	} {
		if p := problem(t, call(routes, tc.method, tc.path, tc.body, nil)); p.Status != 401 || p.ErrorCode != "UNAUTHORIZED" {
			t.Errorf("%s %s: %+v", tc.method, tc.path, p)
		}
	}
}

func TestOrderFlow(t *testing.T) {
	h := testServer(t)
	// A client-sent unitPrice is ignored: the inventory service prices the line (19.99).
	rec := call(h, http.MethodPost, "/api/v1/orders", `{"items":[{"sku":"A","quantity":2,"unitPrice":0.01}]}`, consumer)
	if rec.Code != 201 || rec.Header().Get("Location") != "/api/v1/orders/1" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var dto OrderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if !dto.TotalAmount.Decimal().Equal(decimal.RequireFromString("39.98")) || dto.Status != order.StatusPending {
		t.Fatalf("%+v", dto)
	}
	// Money is a JSON number on the wire, in tests exactly as in production.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if total, ok := raw["totalAmount"].(float64); !ok || total != 39.98 {
		t.Fatalf("totalAmount must be a JSON number: %s", rec.Body.String())
	}

	if rec := call(h, http.MethodGet, "/api/v1/orders/1", "", consumer); rec.Code != 200 {
		t.Fatalf("get: %d", rec.Code)
	}
	other := map[string]string{auth.HeaderDevSubject: "2", auth.HeaderDevRealm: auth.RealmConsumer}
	if p := problem(t, call(h, http.MethodGet, "/api/v1/orders/1", "", other)); p.Status != 404 {
		t.Fatalf("another customer's order is 404: %+v", p)
	}
	if p := problem(t, call(h, http.MethodGet, "/api/v1/orders/abc", "", consumer)); p.Status != 400 {
		t.Fatalf("bad id: %+v", p)
	}

	var page paging.Result[OrderResponse]
	rec = call(h, http.MethodGet, "/api/v1/orders?page=1&pageSize=5", "", consumer)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || page.TotalCount != 1 || len(page.Items) != 1 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	if p := problem(t, call(h, http.MethodGet, "/api/v1/orders?page=10001", "", consumer)); p.Status != 400 ||
		len(p.Errors["page"]) == 0 {
		t.Fatalf("pages past the limit are refused: %+v", p)
	}

	if rec := call(h, http.MethodDelete, "/api/v1/orders/1", "", consumer); rec.Code != 204 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	if p := problem(t, call(h, http.MethodDelete, "/api/v1/orders/1", "", consumer)); p.Status != 409 || p.ErrorCode != order.CodeNotCancellable {
		t.Fatalf("cancel twice: %+v", p)
	}
}

// What api/openapi.yaml says about page, pageSize and limit: an out-of-range NUMBER is clamped
// (below 1 is the default, above the maximum is the maximum); a value that is not an integer is
// a 400 naming the parameter — a typo must not silently read as the first page.
func TestPageSizesAreClampedAndNonNumbersRefused(t *testing.T) {
	h := testServer(t)
	var page paging.Result[OrderResponse]
	rec := call(h, http.MethodGet, "/api/v1/orders?page=0&pageSize=999", "", consumer)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != 200 || page.Page != 1 || page.PageSize != paging.MaxPageSize {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	operator := map[string]string{auth.HeaderDevSubject: "9", auth.HeaderDevRealm: auth.RealmBackOffice, auth.HeaderDevScopes: ScopeOrdersRead}
	p := problem(t, call(h, http.MethodGet, "/api/v1/backoffice/orders?limit=abc", "", operator))
	if p.Status != 400 || p.ErrorCode != "VALIDATION_FAILED" || len(p.Errors["limit"]) == 0 {
		t.Fatalf("a limit that is not a number is refused, naming it: %+v", p)
	}
	if p := problem(t, call(h, http.MethodGet, "/api/v1/orders?page=1.5", "", consumer)); p.Status != 400 || len(p.Errors["page"]) == 0 {
		t.Fatalf("a page that is not an integer is refused, naming it: %+v", p)
	}
}

func TestErrorContract(t *testing.T) {
	h := testServer(t)
	p := problem(t, call(h, http.MethodPost, "/api/v1/orders", `{"items":[{"sku":"","quantity":0}]}`, consumer))
	if p.Status != 400 || p.ErrorCode != "VALIDATION_FAILED" || len(p.Errors["items[0].sku"]) == 0 || len(p.Errors["items[0].quantity"]) == 0 {
		t.Fatalf("validation shape: %+v", p)
	}
	p = problem(t, call(h, http.MethodPost, "/api/v1/orders", `{"items":[{"sku":"OUT-OF-STOCK","quantity":1}]}`, consumer))
	if p.Status != 422 || p.ErrorCode != order.CodeItemUnavailable || p.Type != "urn:msvc:error:order-item-unavailable" {
		t.Fatalf("domain rule: %+v", p)
	}
	p = problem(t, call(h, http.MethodGet, "/nope", "", nil))
	if p.Status != 404 || p.ErrorCode != "NOT_FOUND" || p.Title != "Not found." {
		t.Fatalf("router 404 speaks the contract: %+v", p)
	}
	p = problem(t, call(h, http.MethodPatch, "/api/v1/orders", "", consumer))
	if p.Status != 405 || p.ErrorCode != "METHOD_NOT_ALLOWED" {
		t.Fatalf("router 405 speaks the contract: %+v", p)
	}
	p = problem(t, call(h, http.MethodPost, "/api/v1/orders", `{"items":`, consumer))
	if p.Status != 400 || p.ErrorCode != "BAD_REQUEST" {
		t.Fatalf("malformed JSON: %+v", p)
	}
}

// The validation boundary (CreateOrderRequest): a well-formed body that breaks a rule of the
// order is a 422 with the order's code, not a 400 from the struct tags.
func TestOrderRulesAnswer422(t *testing.T) {
	h := testServer(t)
	p := problem(t, call(h, http.MethodPost, "/api/v1/orders", `{"items":[]}`, consumer))
	if p.Status != 422 || p.ErrorCode != order.CodeItemsRequired {
		t.Fatalf("no items: %+v", p)
	}
	item := `{"sku":"A","quantity":1}`
	tooMany := `{"items":[` + strings.Repeat(item+",", order.MaxItems) + item + `]}`
	p = problem(t, call(h, http.MethodPost, "/api/v1/orders", tooMany, consumer))
	if p.Status != 422 || p.ErrorCode != order.CodeItemInvalid {
		t.Fatalf("too many items: %+v", p)
	}
	p = problem(t, call(h, http.MethodPost, "/api/v1/orders", `{}`, consumer))
	if p.Status != 400 || p.ErrorCode != "VALIDATION_FAILED" || len(p.Errors["items"]) == 0 {
		t.Fatalf("a missing items field is the request's shape: %+v", p)
	}
}

func localizedWriter(t *testing.T, typeBase string) *httpx.ErrorWriter {
	t.Helper()
	ew := httpx.NewErrorWriter(typeBase, quiet)
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	ew.Customizers = append(ew.Customizers, httpx.Localize(catalog))
	return ew
}

func TestProblemTitlesSpeakTheCallersLanguage(t *testing.T) {
	h := testServer(t)
	headers := map[string]string{auth.HeaderDevSubject: "1", auth.HeaderDevRealm: auth.RealmConsumer, "X-Language": "zh-Hant-TW"}
	p := problem(t, call(h, http.MethodGet, "/api/v1/orders/999", "", headers))
	if p.Status != 404 || p.ErrorCode != "NOT_FOUND" || p.Title != "找不到資料" || p.Detail == "" {
		t.Fatalf("title localized, code and detail kept: %+v", p)
	}
	headers["X-Language"] = "ja"
	p = problem(t, call(h, http.MethodPost, "/api/v1/orders", `{"items":[{"sku":"OUT-OF-STOCK","quantity":1}]}`, headers))
	if p.Status != 422 || p.Title != "商品の在庫が不足しています" {
		t.Fatalf("service codes are localized too: %+v", p)
	}
	p = problem(t, call(h, http.MethodGet, "/nope", "", map[string]string{"Accept-Language": "fr, en;q=0.5"}))
	if p.Title != "Not found." {
		t.Fatalf("Accept-Language is honoured and unknown languages fall back to English: %+v", p)
	}
}

func TestBackOfficeListsByCursor(t *testing.T) {
	h := testServer(t)
	for _, sku := range []string{"A", "B", "C"} {
		if rec := call(h, http.MethodPost, "/api/v1/orders", `{"items":[{"sku":"`+sku+`","quantity":1}]}`, consumer); rec.Code != 201 {
			t.Fatalf("create: %d", rec.Code)
		}
	}
	operator := map[string]string{auth.HeaderDevSubject: "9", auth.HeaderDevRealm: auth.RealmBackOffice, auth.HeaderDevScopes: ScopeOrdersRead}
	var page paging.CursorResult[OrderResponse]
	rec := call(h, http.MethodGet, "/api/v1/backoffice/orders?limit=2", "", operator)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 2 || !page.HasMore ||
		page.Items[0].ID != 3 || page.Items[1].ID != 2 {
		t.Fatalf("first page, newest first: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(h, http.MethodGet, "/api/v1/backoffice/orders?limit=2&cursor="+page.NextCursor, "", operator)
	page = paging.CursorResult[OrderResponse]{}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.HasMore ||
		page.Items[0].ID != 1 || page.NextCursor != "" {
		t.Fatalf("second and last page: %d %s", rec.Code, rec.Body.String())
	}
	if p := problem(t, call(h, http.MethodGet, "/api/v1/backoffice/orders?cursor=forged", "", operator)); p.Status != 400 {
		t.Fatalf("a forged cursor is a 400: %+v", p)
	}
}

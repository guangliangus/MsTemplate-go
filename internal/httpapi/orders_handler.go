package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"example.com/msvcservice/internal/order"
	"github.com/guangliangus/go-msvc-kit/apperror"
	"github.com/guangliangus/go-msvc-kit/auth"
	"github.com/guangliangus/go-msvc-kit/httpx"
	"github.com/guangliangus/go-msvc-kit/middleware"
	"github.com/guangliangus/go-msvc-kit/money"
	"github.com/guangliangus/go-msvc-kit/paging"
)

// ScopeOrdersRead is the back-office scope that may list every customer's orders.
const ScopeOrdersRead = "orders.read"

// CreateOrderRequest is the POST body. Wire types live with the handlers that speak them: the
// JSON and validation tags are this package's concern, and the order package never sees them.
//
// Validation is split by what the client got wrong. The struct tags check the SHAPE of the
// request — the body is JSON, `items` is present, every item carries a `sku` (at most 64 chars)
// and a `quantity` (1..1000) — and answer 400 VALIDATION_FAILED with every failing field at once,
// before any business code runs. The order package checks the RULES of an order — how many items
// it may carry (1..order.MaxItems), what inventory can sell, the total cap — and answers 422 with
// one of its own codes (order.CheckLines, order.New). The per-item rules exist on both sides on
// purpose: here so a malformed body is reported field by field, there because an aggregate does
// not trust its callers (the consumers and the tasks never pass through these tags). The item
// count is deliberately not a tag: order.MaxItems is the single source of that limit, and the
// validation tag could not be derived from it.
type CreateOrderRequest struct {
	Items []CreateOrderItem `json:"items" validate:"required,dive"`
}

// CreateOrderItem is one requested line: what and how many. There is deliberately no price — the
// service prices every line with the inventory service; a unitPrice sent by a client is ignored.
type CreateOrderItem struct {
	SKU      string `json:"sku" validate:"required,max=64"`
	Quantity int    `json:"quantity" validate:"required,min=1,max=1000"`
}

func (r CreateOrderRequest) toLines() []order.Line {
	lines := make([]order.Line, len(r.Items))
	for i, it := range r.Items {
		lines[i] = order.Line{SKU: it.SKU, Quantity: it.Quantity}
	}
	return lines
}

// OrderResponse is the order as the API returns it. Amounts travel as JSON numbers (money.Amount).
type OrderResponse struct {
	ID          int                 `json:"id"`
	CustomerID  string              `json:"customerId"`
	Status      order.Status        `json:"status"`
	TotalAmount money.Amount        `json:"totalAmount"`
	Items       []OrderItemResponse `json:"items"`
	CreatedAt   time.Time           `json:"createdAt"`
	UpdatedAt   time.Time           `json:"updatedAt"`
}

// OrderItemResponse is one line as the API returns it.
type OrderItemResponse struct {
	ID        int          `json:"id"`
	SKU       string       `json:"sku"`
	Quantity  int          `json:"quantity"`
	UnitPrice money.Amount `json:"unitPrice"`
	LineTotal money.Amount `json:"lineTotal"`
}

func toOrderResponse(o *order.Order) OrderResponse {
	items := make([]OrderItemResponse, len(o.Items))
	for i, it := range o.Items {
		items[i] = OrderItemResponse{
			ID: it.ID, SKU: it.SKU, Quantity: it.Quantity,
			UnitPrice: money.From(it.UnitPrice), LineTotal: money.From(it.LineTotal()),
		}
	}
	return OrderResponse{
		ID:          o.ID,
		CustomerID:  o.CustomerID,
		Status:      o.Status,
		TotalAmount: money.From(o.TotalAmount),
		Items:       items,
		CreatedAt:   o.CreatedAt,
		UpdatedAt:   o.UpdatedAt,
	}
}

func toOrderResponses(orders []order.Order) []OrderResponse {
	out := make([]OrderResponse, len(orders))
	for i := range orders {
		out[i] = toOrderResponse(&orders[i])
	}
	return out
}

// OrdersHandler is the reference handler: URL-segment versioning, JSON decode plus validation,
// problem details on error, numbered paging for a customer's own (bounded) list, keyset paging for
// the back office's unbounded one, per-route rate limiting.
type OrdersHandler struct {
	orders *order.Service
}

// NewOrdersHandler builds the handler.
func NewOrdersHandler(svc *order.Service) *OrdersHandler {
	return &OrdersHandler{orders: svc}
}

// ConsumerRoutes serves the consumer plane: a customer's own orders.
func (h *OrdersHandler) ConsumerRoutes(limiter *middleware.RateLimiter) http.Handler {
	r := chi.NewRouter()
	r.With(limiter.Policy("writes")).Method(http.MethodPost, "/", httpx.HandlerFunc(h.create))
	r.Method(http.MethodGet, "/", httpx.HandlerFunc(h.list))
	r.Method(http.MethodGet, "/{id}", httpx.HandlerFunc(h.get))
	r.Method(http.MethodDelete, "/{id}", httpx.HandlerFunc(h.cancel))
	return r
}

// BackOfficeRoutes serves the operators' plane: every customer's orders, to operators holding
// orders.read. The router's plane group has already checked the realm.
func (h *OrdersHandler) BackOfficeRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(auth.RequireScope(ScopeOrdersRead))
	r.Method(http.MethodGet, "/", httpx.HandlerFunc(h.listAll))
	return r
}

// customer is the caller's account id. Every route here sits behind the plane group's
// RequireRealm, so a request without a principal is a wiring mistake; it is answered as a 401,
// never dereferenced.
func customer(r *http.Request) (string, error) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		return "", apperror.Unauthorized("Authentication is required.")
	}
	return p.Subject, nil
}

func (h *OrdersHandler) create(w http.ResponseWriter, r *http.Request) error {
	customerID, err := customer(r)
	if err != nil {
		return err
	}
	var req CreateOrderRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	o, err := h.orders.Create(r.Context(), customerID, req.toLines())
	if err != nil {
		return err
	}
	return httpx.Created(w, "/api/v1/orders/"+strconv.Itoa(o.ID), toOrderResponse(o))
}

func (h *OrdersHandler) get(w http.ResponseWriter, r *http.Request) error {
	customerID, err := customer(r)
	if err != nil {
		return err
	}
	id, err := httpx.ParamInt(r, "id")
	if err != nil {
		return err
	}
	o, err := h.orders.Get(r.Context(), id, customerID)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, toOrderResponse(o))
}

// list serves a numbered page of the caller's own orders (?page=&pageSize=).
func (h *OrdersHandler) list(w http.ResponseWriter, r *http.Request) error {
	customerID, err := customer(r)
	if err != nil {
		return err
	}
	page, err := paging.Parse(r.URL.Query())
	if err != nil {
		return err
	}
	items, total, err := h.orders.List(r.Context(), customerID, page)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, paging.NewResult(toOrderResponses(items), page, total))
}

// listAll serves a keyset page of every customer's orders (?cursor=&limit=): the back office's list
// grows without bound, so it is never counted and never OFFSET.
func (h *OrdersHandler) listAll(w http.ResponseWriter, r *http.Request) error {
	page, err := paging.ParseCursor(r.URL.Query())
	if err != nil {
		return err
	}
	items, err := h.orders.ListAll(r.Context(), page)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK,
		paging.NewCursorResult(toOrderResponses(items), page, func(o OrderResponse) int64 { return int64(o.ID) }))
}

func (h *OrdersHandler) cancel(w http.ResponseWriter, r *http.Request) error {
	customerID, err := customer(r)
	if err != nil {
		return err
	}
	id, err := httpx.ParamInt(r, "id")
	if err != nil {
		return err
	}
	if err := h.orders.Cancel(r.Context(), id, customerID); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

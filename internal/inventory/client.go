// Package inventory is the reference downstream adapter: a typed client over the kit's resilient
// HTTP client (timeout, retry, breaker, trace propagation), called by Kubernetes service name.
// Locally the compose stack answers it with WireMock.
package inventory

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/shopspring/decimal"

	"example.com/msvcservice/internal/order"
	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/httpclient"
)

// Client implements order.InventoryClient.
type Client struct {
	http *httpclient.Client
}

// New wraps the named client.
func New(c *httpclient.Client) *Client {
	return &Client{http: c}
}

// Quote asks GET /inventory/{sku}/availability?quantity=N for availability and the unit price. An
// unknown SKU (404) is simply not available; a 5xx or timeout is a 502 for our caller; any other
// 4xx is our bug (500). An available SKU without a price is the downstream breaking its contract:
// a 502, never an order priced at zero.
func (c *Client) Quote(ctx context.Context, sku string, quantity int) (order.Quote, error) {
	var resp struct {
		Available bool             `json:"available"`
		UnitPrice *decimal.Decimal `json:"unitPrice"`
	}
	query := url.Values{"quantity": {strconv.Itoa(quantity)}}
	err := c.http.DoJSON(ctx, http.MethodGet, "inventory/"+url.PathEscape(sku)+"/availability", query, nil, &resp)
	var status *httpclient.StatusError
	if errors.As(err, &status) && status.Status == http.StatusNotFound {
		return order.Quote{}, nil
	}
	if err != nil {
		return order.Quote{}, err
	}
	if !resp.Available {
		return order.Quote{}, nil
	}
	if resp.UnitPrice == nil {
		return order.Quote{}, apperror.Upstream(c.http.Name(), "the response carried no unit price.",
			errors.New("inventory: available SKU "+sku+" without unitPrice"))
	}
	return order.Quote{Available: true, UnitPrice: *resp.UnitPrice}, nil
}

package order

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"example.com/msvcservice/pkg/apperror"
	"example.com/msvcservice/pkg/cache"
	"example.com/msvcservice/pkg/messaging"
	"example.com/msvcservice/pkg/money"
	"example.com/msvcservice/pkg/paging"
)

const (
	reapBatchSize = 500
	// maxConcurrentQuotes bounds the inventory calls one order makes at once: up to MaxItems lines
	// priced in parallel, without one large order opening a hundred connections to the downstream.
	maxConcurrentQuotes = 8
)

// Service is the reference application service: downstream pricing through the resilient client,
// persist plus outbox event in ONE transaction, cached reads, optimistic concurrency on writes,
// cache invalidation after the commit. It speaks domain types only; the HTTP shape is
// internal/httpapi's business.
type Service struct {
	repo      Repository
	tx        Transactor
	inventory InventoryClient
	publisher messaging.Publisher
	cache     *cache.Cache
	log       *slog.Logger
}

// NewService wires the service.
func NewService(repo Repository, tx Transactor, inventory InventoryClient, publisher messaging.Publisher,
	c *cache.Cache, log *slog.Logger) *Service {
	return &Service{repo: repo, tx: tx, inventory: inventory, publisher: publisher, cache: c, log: log}
}

// Create validates the request, prices every line with the inventory service (which also says
// whether it can be sold) and commits the order and its CreatedEvent together.
func (s *Service) Create(ctx context.Context, customerID string, lines []Line) (*Order, error) {
	if err := CheckLines(customerID, lines); err != nil {
		return nil, err
	}
	items, err := s.price(ctx, lines)
	if err != nil {
		return nil, err
	}
	o, err := New(customerID, items)
	if err != nil {
		return nil, err
	}

	// One transaction: the order rows and the outbox row commit together, or not at all.
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.repo.Insert(ctx, o); err != nil {
			return err
		}
		event := CreatedEvent{OrderID: o.ID, CustomerID: o.CustomerID, TotalAmount: money.From(o.TotalAmount)}
		return s.publisher.Publish(ctx, event)
	})
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "order created", slog.Int("order_id", o.ID), slog.String("customer_id", o.CustomerID))
	return o, nil
}

// price quotes every line concurrently (at most maxConcurrentQuotes at once) and turns the answers
// into priced items; the first unavailable line, in request order, refuses the order.
func (s *Service) price(ctx context.Context, lines []Line) ([]Item, error) {
	quotes := make([]Quote, len(lines))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentQuotes)
	for i, l := range lines {
		g.Go(func() error {
			q, err := s.inventory.Quote(gctx, l.SKU, l.Quantity)
			quotes[i] = q
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	items := make([]Item, len(lines))
	for i, l := range lines {
		if !quotes[i].Available {
			return nil, apperror.DomainRule(CodeItemUnavailable,
				fmt.Sprintf("Item '%s' is not available in the requested quantity.", l.SKU))
		}
		items[i] = Item{SKU: l.SKU, Quantity: l.Quantity, UnitPrice: quotes[i].UnitPrice}
	}
	return items, nil
}

// Get returns one order through the two-level cache; customerID scopes the lookup ("" = any).
func (s *Service) Get(ctx context.Context, id int, customerID string) (*Order, error) {
	o, err := cache.GetOrCreate(ctx, s.cache, cacheKey(id), 0, func(ctx context.Context) (Order, error) {
		stored, err := s.repo.Get(ctx, id, "")
		if err != nil {
			return Order{}, err
		}
		return *stored, nil
	})
	if err != nil {
		return nil, err
	}
	if customerID != "" && o.CustomerID != customerID {
		// Another customer's order looks exactly like a missing one: never confirm it exists.
		return nil, apperror.NotFound("Order", id)
	}
	return &o, nil
}

// List returns a numbered page of one customer's orders and that customer's total.
func (s *Service) List(ctx context.Context, customerID string, page paging.Request) ([]Order, int64, error) {
	return s.repo.List(ctx, customerID, page)
}

// ListAll returns a keyset page of every customer's orders, newest first, for the back office:
// up to page.Fetch() orders, for paging.NewCursorResult to trim.
func (s *Service) ListAll(ctx context.Context, page paging.CursorRequest) ([]Order, error) {
	return s.repo.ListAll(ctx, page)
}

// Confirm moves the order to CONFIRMED; it is idempotent, because the consumer that calls it
// runs at-least-once: an order that is already past PENDING is left alone with a log line. The
// consumer calls it inside the inbox transaction, which is why the cache entry is dropped only
// after that transaction commits.
func (s *Service) Confirm(ctx context.Context, id int) error {
	o, err := s.repo.Get(ctx, id, "")
	if err != nil {
		return err
	}
	if o.Status != StatusPending {
		s.log.InfoContext(ctx, "order confirmation skipped: not pending",
			slog.Int("order_id", id), slog.String("status", string(o.Status)))
		return nil
	}
	if err := o.Confirm(); err != nil {
		return err
	}
	if err := s.repo.UpdateStatus(ctx, o); err != nil {
		return err
	}
	s.invalidateAfterCommit(ctx, id)
	s.log.InfoContext(ctx, "order confirmed", slog.Int("order_id", id))
	return nil
}

// Cancel cancels a customer's PENDING order.
func (s *Service) Cancel(ctx context.Context, id int, customerID string) error {
	o, err := s.repo.Get(ctx, id, customerID)
	if err != nil {
		return err
	}
	if err := o.Cancel(); err != nil {
		return err
	}
	if err := s.repo.UpdateStatus(ctx, o); err != nil {
		return err
	}
	s.invalidateAfterCommit(ctx, id)
	s.log.InfoContext(ctx, "order cancelled", slog.Int("order_id", id), slog.String("customer_id", customerID))
	return nil
}

// ReapPending cancels orders that stayed PENDING longer than timeout; the recurring task calls it.
// A bulk status change is still a status change: every cancelled order leaves the cache.
func (s *Service) ReapPending(ctx context.Context, timeout time.Duration) (int64, error) {
	ids, err := s.repo.CancelPendingOlderThan(ctx, time.Now().Add(-timeout), reapBatchSize)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		s.invalidateAfterCommit(ctx, id)
	}
	return int64(len(ids)), nil
}

// invalidateAfterCommit drops the cached order once the change is committed. Dropping it before
// the commit lets a concurrent reader load the old row and cache it again for a whole TTL.
func (s *Service) invalidateAfterCommit(ctx context.Context, id int) {
	s.tx.AfterCommit(ctx, func(ctx context.Context) { s.invalidate(ctx, id) })
}

func (s *Service) invalidate(ctx context.Context, id int) {
	if err := s.cache.Remove(ctx, cacheKey(id)); err != nil {
		s.log.WarnContext(ctx, "order cache invalidation failed; stale reads possible until the TTL",
			slog.Int("order_id", id), slog.Any("error", err))
	}
}

func cacheKey(id int) string {
	return fmt.Sprintf("orders:%d", id)
}

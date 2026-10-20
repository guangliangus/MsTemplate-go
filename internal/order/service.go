package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/guangliangus/go-msvc-kit/apperror"
	"github.com/guangliangus/go-msvc-kit/cache"
	"github.com/guangliangus/go-msvc-kit/messaging"
	"github.com/guangliangus/go-msvc-kit/money"
	"github.com/guangliangus/go-msvc-kit/paging"
)

const (
	// reapBatchSize bounds one CancelPendingOlderThan statement, so the reaper never locks a huge
	// set of rows in one UPDATE.
	reapBatchSize = 500
	// maxReapBatches bounds one ReapPending run: a backlog above reapBatchSize drains over several
	// batches within the run, but a run never holds the task slot indefinitely; the next firing
	// continues where this one stopped.
	maxReapBatches = 20
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
	cache     *cache.Redis
	log       *slog.Logger
}

// NewService wires the service.
func NewService(repo Repository, tx Transactor, inventory InventoryClient, publisher messaging.Publisher,
	c *cache.Redis, log *slog.Logger) *Service {
	return &Service{repo: repo, tx: tx, inventory: inventory, publisher: publisher, cache: c, log: log}
}

// Create checks the lines, prices every one with the inventory service (which also says whether it
// can be sold) and commits the order and its CreatedEvent together. The line rules run once, before
// any downstream call; newChecked then adds only what the prices can tell.
//
// requestID (the client's Idempotency-Key, "" for none) makes the call idempotent for the customer:
// a repeated request is answered with the order the first one created and created=false, and
// nothing is priced, written or published again. What counts as a repeat is this business's call —
// the same customer, the same request id and the same lines; the id reused for another basket is a
// 422 ORDER_REQUEST_ID_REUSED, never another basket's order. The guarantee is the database's: a
// unique index on (customer_id, request_id) commits with the order and its event, so it holds
// across replicas and restarts, and a repeat racing the first request waits for it at the index.
func (s *Service) Create(ctx context.Context, customerID, requestID string, lines []Line,
) (o *Order, created bool, err error) {
	if err := CheckLines(customerID, lines); err != nil {
		return nil, false, err
	}
	if prior, found, err := s.priorOrder(ctx, customerID, requestID); err != nil || found {
		if err != nil {
			return nil, false, err
		}
		return answerRepeat(prior, lines)
	}
	items, err := s.price(ctx, lines)
	if err != nil {
		return nil, false, err
	}
	o, err = newChecked(customerID, items)
	if err != nil {
		return nil, false, err
	}
	o.RequestID = requestID

	// One transaction: the order rows and the outbox row commit together, or not at all.
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.repo.Insert(ctx, o); err != nil {
			return err
		}
		event := CreatedEvent{OrderID: o.ID, CustomerID: o.CustomerID, TotalAmount: money.From(o.TotalAmount)}
		return s.publisher.Publish(ctx, event)
	})
	if errors.Is(err, ErrDuplicateRequest) {
		// A repeat of this request committed between the lookup above and this insert; the unique
		// index refused this one, and the rollback took its event with it. Answer with the winner's.
		prior, found, lookupErr := s.priorOrder(ctx, customerID, requestID)
		if lookupErr != nil || !found {
			return nil, false, errors.Join(err, lookupErr)
		}
		return answerRepeat(prior, lines)
	}
	if err != nil {
		return nil, false, err
	}
	s.log.InfoContext(ctx, "order created", slog.Int("order_id", o.ID), slog.String("customer_id", o.CustomerID))
	return o, true, nil
}

// priorOrder finds the order an earlier request with this id created; a request without an id has
// none.
func (s *Service) priorOrder(ctx context.Context, customerID, requestID string) (*Order, bool, error) {
	if requestID == "" {
		return nil, false, nil
	}
	return s.repo.FindByRequestID(ctx, customerID, requestID)
}

// answerRepeat answers a repeated request with the order the first one created, as it stands now
// (a cancelled order stays cancelled) — provided the repeat asks for the same lines.
func answerRepeat(prior *Order, lines []Line) (*Order, bool, error) {
	if !prior.createdFrom(lines) {
		return nil, false, apperror.DomainRule(CodeRequestIDReused,
			"This Idempotency-Key was already used for a different order; send a new key for a new order.")
	}
	return prior, false, nil
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

// Get returns one order through the shared (Redis) cache: an order changes on writes, so every
// replica must see its invalidation at once, which a per-pod copy could not. customerID scopes the
// lookup ("" = any).
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
// The cutoff is taken once per run, so every batch of the run agrees on it. A backlog above
// reapBatchSize drains batch by batch until a short batch comes back, at most maxReapBatches per
// run. A bulk status change is still a status change: every cancelled order leaves the cache.
func (s *Service) ReapPending(ctx context.Context, timeout time.Duration) (int64, error) {
	cutoff := time.Now().Add(-timeout)
	var total int64
	for range maxReapBatches {
		ids, err := s.repo.CancelPendingOlderThan(ctx, cutoff, reapBatchSize)
		if err != nil {
			return total, err
		}
		s.invalidateAfterCommit(ctx, ids...)
		total += int64(len(ids))
		if len(ids) < reapBatchSize {
			break
		}
	}
	return total, nil
}

// invalidateAfterCommit drops the cached orders once the change is committed, all of them in one
// pipelined round trip. Dropping them before the commit lets a concurrent reader load the old row
// and cache it again for a whole TTL; dropping them after it is safe even against a reader that
// read the old row before the commit (the cache's Remove keeps that late write out).
func (s *Service) invalidateAfterCommit(ctx context.Context, ids ...int) {
	if len(ids) == 0 {
		return
	}
	s.tx.AfterCommit(ctx, func(ctx context.Context) { s.invalidate(ctx, ids) })
}

func (s *Service) invalidate(ctx context.Context, ids []int) {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = cacheKey(id)
	}
	if err := s.cache.Remove(ctx, keys...); err != nil {
		s.log.WarnContext(ctx, "order cache invalidation failed; stale reads possible until the TTL",
			slog.Any("order_ids", ids), slog.Any("error", err))
	}
}

func cacheKey(id int) string {
	return fmt.Sprintf("orders:%d", id)
}

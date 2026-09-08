package trading

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/policy"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/trading/sqlcgen"
)

// commandTimeout bounds a group's database work (plus
// commandTimeoutPerCommand each). It is independent of the callers'
// contexts: a client that gives up mid-transaction must not turn a
// committed command into a rebuild.
const commandTimeout = 10 * time.Second

// request is one unit of work for a runner goroutine.
type request struct {
	ctx    context.Context
	place  *PlaceOrderRequest
	cancel *CancelRequest
	depth  int  // > 0: depth query with n levels; < 0: full snapshot
	query  bool // depth/snapshot request
	reply  chan response
}

type response struct {
	result   PlaceOrderResult
	depth    matching.Depth
	snapshot matching.Snapshot
	err      error
}

// runner owns one market: its order book, its sequence and the single
// goroutine that mutates both (docs/plan-v1.0.md §5.2). Everything it
// persists for one command is one Postgres transaction.
type runner struct {
	symbol  string
	market  func() (registry.Market, bool) // live view of the registry cache
	pool    *pgxpool.Pool
	ledger  *ledger.Service
	outbox  eventbus.Outbox
	policy  policy.OrderPolicy
	tenant  string
	log     *slog.Logger
	metrics *Metrics

	cmds      chan request
	batchSize int // commands per transaction at most (1 = one each)

	// owned by the goroutine
	book    *matching.Book
	seq     uint64            // last seq committed (== trading.market_sequences.last_seq)
	dirty   bool              // book may disagree with the database; rebuild before use
	mid     string            // registry market id
	orders  map[string]*Order // the resting orders' rows, as the database has them
	touched map[string]*Order // orders the current group ended (batch.go)
	pending *request          // taken off the queue but held for the next group

	beforeCommit func(market string, commands int) error // Engine.WithFaultInjection
}

func newRunner(e *Engine, m registry.Market) *runner {
	return &runner{
		symbol: m.Symbol, mid: m.ID,
		market: func() (registry.Market, bool) { return e.reg.Market(m.Symbol) },
		pool:   e.pool, ledger: e.ledger, outbox: e.outbox, policy: e.policy, tenant: e.tenant,
		log: e.log.With(slog.String("market", m.Symbol)), metrics: e.metrics,
		cmds: make(chan request, e.queueSize), batchSize: e.batchSize, beforeCommit: e.beforeCommit,
	}
}

// restore rebuilds the book from the database (ADR-0002): open orders by
// (price, seq) and the last committed sequence. The rows themselves are
// kept beside the book so settling against a maker needs no read.
func (r *runner) restore(ctx context.Context) error {
	start := time.Now()
	m, ok := r.market()
	if !ok {
		return fmt.Errorf("%w: %s", ErrMarketNotFound, r.symbol)
	}
	cfg, err := marketConfig(m)
	if err != nil {
		return fmt.Errorf("trading: market %s: %w", r.symbol, err)
	}
	book, err := matching.New(cfg)
	if err != nil {
		return err
	}
	q := sqlcgen.New(r.pool)
	lastSeq, err := q.EnsureMarketSequence(ctx, r.mid)
	if err != nil {
		return fmt.Errorf("trading: market sequence %s: %w", r.symbol, err)
	}
	rows, err := q.ListOpenOrdersByMarket(ctx, r.mid)
	if err != nil {
		return fmt.Errorf("trading: open orders %s: %w", r.symbol, err)
	}
	orders, err := ordersFromRows(rows)
	if err != nil {
		return err
	}
	resting := make([]matching.RestingOrder, 0, len(orders))
	cache := make(map[string]*Order, len(orders))
	for i := range orders {
		o := orders[i]
		if o.Seq == nil || o.Price == nil || o.Qty == nil {
			return fmt.Errorf("%w: open order %s lacks seq/price/qty", ErrBookInconsistent, o.ID)
		}
		cache[o.ID] = &orders[i]
		resting = append(resting, matching.RestingOrder{
			OrderID: matching.OrderID(o.ID), AccountID: matching.AccountID(o.AccountID), Side: o.Side,
			Price: *o.Price, Qty: *o.Qty, Remaining: o.RemainingQty, FilledQty: o.FilledQty, FilledQuote: o.FilledQuote,
			Seq: *o.Seq, Timestamp: o.CreatedAt,
		})
	}
	if err := book.Restore(uint64(lastSeq), resting); err != nil { //nolint:gosec // last_seq >= 0 by CHECK
		return fmt.Errorf("trading: restore %s: %w", r.symbol, err)
	}
	r.book, r.seq, r.dirty, r.orders = book, uint64(lastSeq), false, cache //nolint:gosec // last_seq >= 0 by CHECK
	if r.metrics != nil {
		r.metrics.rebuildDuration.Observe(time.Since(start).Seconds())
		r.metrics.observeBook(r.symbol, r.seq, len(resting))
	}
	r.log.Info("order book restored", slog.Int("open_orders", len(resting)), slog.Uint64("last_seq", r.seq),
		slog.Duration("took", time.Since(start)))
	return nil
}

// run is the goroutine body: queries answered one at a time, commands in
// groups of up to batchSize (batch.go), until ctx ends.
func (r *runner) run(ctx context.Context) {
	for {
		req, ok := r.next(ctx)
		if !ok {
			// answer whatever is still queued so callers do not hang
			for {
				select {
				case req := <-r.cmds:
					req.reply <- response{err: ErrEngineUnavailable}
				default:
					return
				}
			}
		}
		if r.metrics != nil {
			r.metrics.observeQueue(r.symbol, len(r.cmds))
		}
		if req.query {
			req.reply <- r.handle(ctx, req)
			continue
		}
		r.handleGroup(ctx, r.drain(req))
	}
}

// next takes the request held back by the last drain, or the next one
// off the queue; false when ctx ended.
func (r *runner) next(ctx context.Context) (request, bool) {
	if r.pending != nil {
		req := *r.pending
		r.pending = nil
		return req, true
	}
	select {
	case <-ctx.Done():
		return request{}, false
	case req := <-r.cmds:
		return req, true
	}
}

// drain collects up to batchSize commands that are already waiting behind
// first, without blocking. It stops at a query (queries see committed
// books only, so they run between groups) and at a second command with
// the same key (a repeated client_order_id, a repeated cancel): that one
// belongs to the next group, where the pre-reads see what this group did.
func (r *runner) drain(first request) []request {
	group := []request{first}
	seen := map[string]struct{}{first.key(): {}}
	for len(group) < r.batchSize {
		select {
		case req := <-r.cmds:
			if _, dup := seen[req.key()]; req.query || dup {
				r.pending = &req
				return group
			}
			seen[req.key()] = struct{}{}
			group = append(group, req)
		default:
			return group
		}
	}
	return group
}

// handle answers a depth or snapshot query from the book in memory.
func (r *runner) handle(ctx context.Context, req request) response {
	if req.ctx != nil {
		ctx = telemetry.WithSpanOf(ctx, req.ctx)
	}
	_, end := telemetry.StartSpan(ctx, "engine "+req.kind(), telemetry.SpanInternal, map[string]string{"exchange.market": r.symbol})
	defer end(nil)
	if err := r.restoreOrFail(ctx); err != nil {
		return response{err: err}
	}
	if req.depth < 0 {
		return response{snapshot: r.book.Snapshot()}
	}
	return response{depth: r.book.Depth(req.depth)}
}

// kind names a queued request for its span.
func (req request) kind() string {
	switch {
	case req.query && req.depth < 0:
		return "snapshot"
	case req.query:
		return "depth"
	case req.cancel != nil:
		return "cancel"
	default:
		return "place"
	}
}

func (r *runner) observe(o Order) {
	if r.metrics == nil {
		return
	}
	r.metrics.orders.WithLabelValues(r.symbol, o.Type.String(), string(o.Status)).Inc()
	r.metrics.observeBook(r.symbol, r.seq, r.book.Len())
}

// pgErrorCode extracts the SQLSTATE of a Postgres error, or "".
func pgErrorCode(err error) string {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState()
	}
	return ""
}

package trading

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/policy"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/trading/sqlcgen"
)

// commandTimeout bounds one command's database work. It is independent of
// the caller's context: a client that gives up mid-transaction must not
// turn a committed command into a rebuild.
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

	cmds chan request

	// owned by the goroutine
	book  *matching.Book
	seq   uint64 // last seq committed (== trading.market_sequences.last_seq)
	dirty bool   // book may disagree with the database; rebuild before use
	mid   string // registry market id
}

func newRunner(e *Engine, m registry.Market) *runner {
	return &runner{
		symbol: m.Symbol, mid: m.ID,
		market: func() (registry.Market, bool) { return e.reg.Market(m.Symbol) },
		pool:   e.pool, ledger: e.ledger, outbox: e.outbox, policy: e.policy, tenant: e.tenant,
		log: e.log.With(slog.String("market", m.Symbol)), metrics: e.metrics,
		cmds: make(chan request, e.queueSize),
	}
}

// restore rebuilds the book from the database (ADR-0002): open orders by
// (price, seq) and the last committed sequence.
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
	for _, o := range orders {
		if o.Seq == nil || o.Price == nil || o.Qty == nil {
			return fmt.Errorf("%w: open order %s lacks seq/price/qty", ErrBookInconsistent, o.ID)
		}
		resting = append(resting, matching.RestingOrder{
			OrderID: matching.OrderID(o.ID), AccountID: matching.AccountID(o.AccountID), Side: o.Side,
			Price: *o.Price, Qty: *o.Qty, Remaining: o.RemainingQty, FilledQty: o.FilledQty, FilledQuote: o.FilledQuote,
			Seq: *o.Seq, Timestamp: o.CreatedAt,
		})
	}
	if err := book.Restore(uint64(lastSeq), resting); err != nil { //nolint:gosec // last_seq >= 0 by CHECK
		return fmt.Errorf("trading: restore %s: %w", r.symbol, err)
	}
	r.book, r.seq, r.dirty = book, uint64(lastSeq), false //nolint:gosec // last_seq >= 0 by CHECK
	if r.metrics != nil {
		r.metrics.rebuildDuration.Observe(time.Since(start).Seconds())
		r.metrics.observeBook(r.symbol, r.seq, len(resting))
	}
	r.log.Info("order book restored", slog.Int("open_orders", len(resting)), slog.Uint64("last_seq", r.seq),
		slog.Duration("took", time.Since(start)))
	return nil
}

// run is the goroutine body: one command at a time until ctx ends.
func (r *runner) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// answer whatever is still queued so callers do not hang
			for {
				select {
				case req := <-r.cmds:
					req.reply <- response{err: ErrEngineUnavailable}
				default:
					return
				}
			}
		case req := <-r.cmds:
			if r.metrics != nil {
				r.metrics.observeQueue(r.symbol, len(r.cmds))
			}
			req.reply <- r.handle(ctx, req)
		}
	}
}

func (r *runner) handle(ctx context.Context, req request) response {
	if r.dirty {
		if err := r.restore(ctx); err != nil {
			r.log.Error("order book rebuild failed", slog.String("err", err.Error()))
			return response{err: fmt.Errorf("%w: %v", ErrEngineUnavailable, err)}
		}
		if r.metrics != nil {
			r.metrics.rebuilds.Inc()
		}
	}
	switch {
	case req.query:
		if req.depth < 0 {
			return response{snapshot: r.book.Snapshot()}
		}
		return response{depth: r.book.Depth(req.depth)}
	case req.place != nil:
		start := time.Now()
		res, err := r.place(ctx, *req.place)
		if r.metrics != nil {
			r.metrics.applyDuration.WithLabelValues(r.symbol).Observe(time.Since(start).Seconds())
		}
		return response{result: res, err: err}
	case req.cancel != nil:
		o, err := r.cancelOrder(ctx, *req.cancel)
		return response{result: PlaceOrderResult{Order: o}, err: err}
	}
	return response{err: fmt.Errorf("%w: empty request", ErrInvalidRequest)}
}

// txContext detaches the command's database work from the caller's
// cancellation but keeps its values (correlation id, logger).
func txContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), commandTimeout)
}

// place runs the new-order flow of docs/plan-v1.0.md §5.2 step 4.
func (r *runner) place(ctx context.Context, req PlaceOrderRequest) (PlaceOrderResult, error) {
	m, ok := r.market()
	if !ok {
		return PlaceOrderResult{}, fmt.Errorf("%w: %s", ErrMarketNotFound, req.MarketSymbol)
	}
	cctx, cancel := txContext(ctx)
	defer cancel()

	// client_order_id replay: same request → the original order; different → mismatch
	if existing, err := getOrderByClientID(cctx, r.pool, r.tenant, req.AccountID, req.ClientOrderID); err == nil {
		if !req.sameAs(existing) {
			return PlaceOrderResult{}, ErrClientOrderIDMismatch
		}
		trades, err := r.tradesOfOrder(cctx, existing.ID)
		if err != nil {
			return PlaceOrderResult{}, err
		}
		return PlaceOrderResult{Order: existing, Trades: trades, Replayed: true}, nil
	} else if !errors.Is(err, ErrOrderNotFound) {
		return PlaceOrderResult{}, err
	}

	now := time.Now().UTC().Truncate(time.Microsecond) // timestamptz precision: memory == database
	holdAsset, holdAmount := holdFor(m, req)
	order := Order{
		ID: eventbus.NewID(now), TenantID: r.tenant, AccountID: req.AccountID, MarketID: m.ID, MarketSymbol: m.Symbol,
		ClientOrderID: req.ClientOrderID, Side: req.Side, Type: req.Type, TimeInForce: req.effectiveTIF(),
		FilledQty: money.Zero, FilledQuote: money.Zero, RemainingQty: req.Qty,
		HoldAsset: holdAsset, HoldAmount: money.Zero, HoldRemaining: money.Zero,
		CorrelationID: req.CorrelationID, CreatedAt: now, UpdatedAt: now,
	}
	if req.Price.IsPositive() {
		p := req.Price
		order.Price = &p
	}
	if req.Qty.IsPositive() {
		q := req.Qty
		order.Qty = &q
	}
	if req.QuoteQty.IsPositive() {
		q := req.QuoteQty
		order.QuoteQty = &q
	}

	// policy (market status, frozen account) — refused before touching the book
	account, err := r.ledger.Account(cctx, req.AccountID)
	if err != nil {
		if errors.Is(err, ledger.ErrAccountNotFound) {
			return PlaceOrderResult{}, fmt.Errorf("%w: account %s", ErrInvalidRequest, req.AccountID)
		}
		return PlaceOrderResult{}, err
	}
	if reason := r.policy.NewOrder(m, account); reason != "" {
		return r.persistRejected(cctx, order, RejectReason(reason), nil)
	}

	cmd := matching.Command{Seq: r.seq + 1, Timestamp: now, New: newOrderCommand(order.ID, req)}
	var res PlaceOrderResult
	applied := false
	err = r.inTx(cctx, func(tx pgx.Tx) error {
		if err := r.advanceSeq(cctx, tx, cmd.Seq); err != nil {
			return err
		}
		seq := cmd.Seq
		order.Seq = &seq

		// savepoint: the hold is undone when the book rejects the order
		sp, err := tx.Begin(cctx)
		if err != nil {
			return fmt.Errorf("trading: savepoint: %w", err)
		}
		holdEntry, _, err := r.ledger.Hold(cctx, sp, ledger.HoldParams{
			AccountID: order.AccountID, Asset: holdAsset, Amount: holdAmount,
			IdempotencyKey: "hold:order:" + order.ID, Ref: ledger.Ref{Type: "order", ID: order.ID}, CorrelationID: req.CorrelationID,
		})
		if err != nil {
			if errors.Is(err, ledger.ErrInsufficient) || errors.Is(err, ledger.ErrInvalidEntry) {
				_ = sp.Rollback(cctx)
				res, err = r.writeRejected(cctx, tx, order, RejectInsufficientBalance)
				return err
			}
			return err
		}
		events, err := r.book.Apply(cmd)
		if err != nil {
			return fmt.Errorf("trading: apply: %w", err)
		}
		applied = true
		if rej, ok := events[0].(matching.Rejected); ok && len(events) == 1 {
			_ = sp.Rollback(cctx)
			res, err = r.writeRejected(cctx, tx, order, RejectReason(rej.Reason))
			return err
		}
		if err := sp.Commit(cctx); err != nil {
			return fmt.Errorf("trading: release savepoint: %w", err)
		}
		order.HoldAmount, order.HoldRemaining = holdAmount, holdAmount
		res, err = r.persistAccepted(cctx, tx, m, order, req, cmd, events, holdEntry)
		return err
	})
	if err != nil {
		if applied {
			r.dirty = true // the book moved on but the database did not
		}
		return PlaceOrderResult{}, err
	}
	r.seq = cmd.Seq
	r.observe(res.Order)
	return res, nil
}

// persistRejected writes a pre-book rejection (no seq) in its own
// transaction.
func (r *runner) persistRejected(ctx context.Context, order Order, reason RejectReason, seq *uint64) (PlaceOrderResult, error) {
	order.Seq = seq
	var res PlaceOrderResult
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		res, err = r.writeRejected(ctx, tx, order, reason)
		return err
	})
	if err != nil {
		return PlaceOrderResult{}, err
	}
	r.observe(res.Order)
	return res, nil
}

// writeRejected inserts the order in StatusRejected and its event inside tx.
func (r *runner) writeRejected(ctx context.Context, tx pgx.Tx, order Order, reason RejectReason) (PlaceOrderResult, error) {
	order.Status, order.RejectReason = StatusRejected, reason
	order.RemainingQty, order.HoldAmount, order.HoldRemaining = money.Zero, money.Zero, money.Zero
	row, err := sqlcgen.New(tx).InsertOrder(ctx, insertParams(order))
	if err != nil {
		return PlaceOrderResult{}, r.insertErr(err)
	}
	saved, err := orderFromRow(row)
	if err != nil {
		return PlaceOrderResult{}, err
	}
	b := &eventBuilder{tenant: r.tenant, market: order.MarketSymbol, now: order.CreatedAt, correlation: order.CorrelationID, seq: order.Seq}
	b.add(EventOrderRejected, order.AccountID, OrderRejectedPayload{
		OrderID: order.ID, ClientOrderID: order.ClientOrderID, AccountID: order.AccountID, Market: order.MarketSymbol, Reason: reason, Seq: order.Seq,
	})
	if err := r.flushEvents(ctx, tx, b); err != nil {
		return PlaceOrderResult{}, err
	}
	return PlaceOrderResult{Order: saved}, nil
}

// insertErr maps a unique violation on client_order_id (a race between two
// markets of the same account) to ErrClientOrderIDMismatch.
func (r *runner) insertErr(err error) error {
	if pgErrorCode(err) == "23505" {
		return ErrClientOrderIDMismatch
	}
	return fmt.Errorf("trading: insert order: %w", err)
}

// persistAccepted writes everything an accepted command produced: the taker
// order, trades with their settlements, maker updates, releases, and the
// outbox rows (docs/plan-v1.0.md §6.2 transition table).
func (r *runner) persistAccepted(ctx context.Context, tx pgx.Tx, m registry.Market, order Order, req PlaceOrderRequest, cmd matching.Command, events []matching.Event, holdEntry ledger.JournalEntry) (PlaceOrderResult, error) {
	q := sqlcgen.New(tx)
	b := &eventBuilder{tenant: r.tenant, market: m.Symbol, now: cmd.Timestamp, correlation: req.CorrelationID, seq: order.Seq}
	fees := ledger.FeeParams{MakerBps: m.MakerBps, TakerBps: m.TakerBps, BaseScale: m.BaseScale, QuoteScale: m.QuoteScale}
	var (
		entries = []ledger.JournalEntry{holdEntry}
		trades  []Trade
		makers  = map[string]*Order{} // maker orders touched by this command
		taker   = &order
	)

	// the taker row must exist before trades reference it; its final state
	// is written after the events are applied
	taker.Status = StatusOpen
	if _, err := q.InsertOrder(ctx, insertParams(*taker)); err != nil {
		return PlaceOrderResult{}, r.insertErr(err)
	}
	b.add(EventOrderAccepted, taker.AccountID, OrderAcceptedPayload{
		OrderID: taker.ID, ClientOrderID: taker.ClientOrderID, AccountID: taker.AccountID, Market: m.Symbol,
		Side: taker.Side.String(), Type: taker.Type.String(), TimeInForce: taker.TimeInForce.String(),
		Price: taker.Price, Qty: taker.Qty, QuoteQty: taker.QuoteQty, Seq: cmd.Seq,
	})

	loadMaker := func(id string) (*Order, error) {
		if mo, ok := makers[id]; ok {
			return mo, nil
		}
		mo, err := getOrder(ctx, tx, id)
		if err != nil {
			return nil, fmt.Errorf("%w: maker %s: %v", ErrBookInconsistent, id, err)
		}
		makers[id] = &mo
		return &mo, nil
	}

	for _, ev := range events[1:] {
		switch e := ev.(type) {
		case matching.Trade:
			maker, err := loadMaker(string(e.MakerOrderID))
			if err != nil {
				return PlaceOrderResult{}, err
			}
			t, entry, err := r.settle(ctx, tx, m, fees, cmd, e, taker, maker)
			if err != nil {
				return PlaceOrderResult{}, err
			}
			entries = append(entries, entry)
			trades = append(trades, t)
			b.add(EventTradeExecuted, "", TradeExecutedPayload{
				TradeID: t.ID, Market: m.Symbol, MakerOrderID: t.MakerOrderID, TakerOrderID: t.TakerOrderID,
				MakerAccountID: t.MakerAccountID, TakerAccountID: t.TakerAccountID, TakerSide: t.TakerSide.String(),
				Price: t.Price, Qty: t.Qty, QuoteQty: t.QuoteQty, MakerFee: t.MakerFee, MakerFeeAsset: t.MakerFeeAsset,
				TakerFee: t.TakerFee, TakerFeeAsset: t.TakerFeeAsset, Seq: cmd.Seq,
			})
		case matching.Updated:
			o := taker
			if string(e.OrderID) != taker.ID {
				var err error
				if o, err = loadMaker(string(e.OrderID)); err != nil {
					return PlaceOrderResult{}, err
				}
			}
			o.FilledQty, o.FilledQuote, o.RemainingQty, o.Status = e.FilledQty, e.FilledQuote, e.RemainingQty, StatusPartiallyFilled
			b.add(EventOrderUpdated, o.AccountID, OrderUpdatedPayload{
				OrderID: o.ID, AccountID: o.AccountID, Market: m.Symbol, Status: o.Status,
				FilledQty: o.FilledQty, FilledQuote: o.FilledQuote, RemainingQty: o.RemainingQty, Seq: cmd.Seq,
			})
		case matching.Filled:
			o := taker
			if string(e.OrderID) != taker.ID {
				var err error
				if o, err = loadMaker(string(e.OrderID)); err != nil {
					return PlaceOrderResult{}, err
				}
			}
			o.FilledQty, o.FilledQuote, o.RemainingQty, o.Status = e.FilledQty, e.FilledQuote, money.Zero, StatusFilled
			// a filled order must have nothing frozen; release any residue
			// (rounding) so Σ hold_remaining == balances.hold stays exact
			if o.HoldRemaining.IsPositive() {
				entry, err := r.release(ctx, tx, o, cmd.Seq, req.CorrelationID)
				if err != nil {
					return PlaceOrderResult{}, err
				}
				entries = append(entries, entry)
			}
			b.add(EventOrderFilled, o.AccountID, OrderFilledPayload{
				OrderID: o.ID, AccountID: o.AccountID, Market: m.Symbol, FilledQty: o.FilledQty, FilledQuote: o.FilledQuote, Seq: cmd.Seq,
			})
		case matching.Cancelled: // taker remainder: IOC, market, STP, price band
			taker.FilledQty, taker.FilledQuote, taker.RemainingQty = e.FilledQty, e.FilledQuote, e.RemainingQty
			taker.Status, taker.CancelReason = StatusCancelled, e.Reason
			released := taker.HoldRemaining
			if released.IsPositive() {
				entry, err := r.release(ctx, tx, taker, cmd.Seq, req.CorrelationID)
				if err != nil {
					return PlaceOrderResult{}, err
				}
				entries = append(entries, entry)
			}
			b.add(EventOrderCancelled, taker.AccountID, OrderCancelledPayload{
				OrderID: taker.ID, AccountID: taker.AccountID, Market: m.Symbol, Reason: e.Reason,
				FilledQty: e.FilledQty, FilledQuote: e.FilledQuote, RemainingQty: e.RemainingQty, RemainingQuote: e.RemainingQuote,
				Released: released, ReleasedAsset: taker.HoldAsset, Seq: cmd.Seq,
			})
		default:
			return PlaceOrderResult{}, fmt.Errorf("%w: unexpected event %s after accepted", ErrBookInconsistent, ev.Kind())
		}
	}

	// persist the final states
	for _, mo := range makers {
		if _, err := q.UpdateOrderProgress(ctx, progressParams(*mo)); err != nil {
			return PlaceOrderResult{}, fmt.Errorf("trading: update maker %s: %w", mo.ID, err)
		}
	}
	row, err := q.UpdateOrderProgress(ctx, progressParams(*taker))
	if err != nil {
		return PlaceOrderResult{}, fmt.Errorf("trading: update taker %s: %w", taker.ID, err)
	}
	saved, err := orderFromRow(row)
	if err != nil {
		return PlaceOrderResult{}, err
	}
	for _, bal := range balanceEvents(entries) {
		b.addAccountOnly(EventBalanceUpdated, bal.AccountID, BalanceUpdatedPayload{AccountID: bal.AccountID, Asset: bal.Asset, Available: bal.Available, Hold: bal.Hold})
	}
	if err := r.flushEvents(ctx, tx, b); err != nil {
		return PlaceOrderResult{}, err
	}
	if r.metrics != nil {
		r.metrics.addTrades(m.Symbol, len(trades))
	}
	return PlaceOrderResult{Order: saved, Trades: trades}, nil
}

// settle books one fill: the ledger entry (fees, price-improvement
// release), the trade row, and the hold bookkeeping of both orders.
func (r *runner) settle(ctx context.Context, tx pgx.Tx, m registry.Market, fees ledger.FeeParams, cmd matching.Command, e matching.Trade, taker, maker *Order) (Trade, ledger.JournalEntry, error) {
	buyer, seller := maker, taker
	buyerIsTaker := e.TakerSide == matching.Buy
	if buyerIsTaker {
		buyer, seller = taker, maker
	}
	// the buyer's hold was computed with its limit price; market buys hold
	// their budget and release the leftover when the order completes
	buyerLimit := money.Zero
	if buyer.Type == matching.Limit && buyer.Price != nil {
		buyerLimit = *buyer.Price
	}
	tradeID := eventbus.NewID(cmd.Timestamp)
	res, _, err := r.ledger.Settle(ctx, tx, ledger.SettleParams{
		TradeID: tradeID, IdempotencyKey: "settle:trade:" + tradeID, CorrelationID: taker.CorrelationID,
		BuyerAccountID: buyer.AccountID, SellerAccountID: seller.AccountID, BuyerIsTaker: buyerIsTaker,
		BaseAsset: m.BaseSymbol, QuoteAsset: m.QuoteSymbol, Price: e.Price, Qty: e.Qty, QuoteQty: e.QuoteQty,
		BuyerLimitPrice: buyerLimit, Fees: fees,
	})
	if err != nil {
		return Trade{}, ledger.JournalEntry{}, fmt.Errorf("trading: settle trade %s: %w", tradeID, err)
	}
	// what each side no longer has frozen
	buyer.HoldRemaining = buyer.HoldRemaining.Sub(e.QuoteQty).Sub(res.Release)
	seller.HoldRemaining = seller.HoldRemaining.Sub(e.Qty)
	if buyer.HoldRemaining.IsNegative() || seller.HoldRemaining.IsNegative() {
		return Trade{}, ledger.JournalEntry{}, fmt.Errorf("%w: hold went negative settling %s", ErrBookInconsistent, tradeID)
	}
	makerFeeAsset, takerFeeAsset := m.QuoteSymbol, m.BaseSymbol // maker sells (receives quote), taker buys (receives base)
	if !buyerIsTaker {
		makerFeeAsset, takerFeeAsset = m.BaseSymbol, m.QuoteSymbol
	}
	t := Trade{
		ID: tradeID, TenantID: r.tenant, MarketID: m.ID, MarketSymbol: m.Symbol, Seq: cmd.Seq, Index: e.Index,
		MakerOrderID: maker.ID, TakerOrderID: taker.ID, MakerAccountID: maker.AccountID, TakerAccountID: taker.AccountID,
		TakerSide: e.TakerSide, Price: e.Price, Qty: e.Qty, QuoteQty: e.QuoteQty,
		MakerFee: res.MakerFee, MakerFeeAsset: makerFeeAsset, TakerFee: res.TakerFee, TakerFeeAsset: takerFeeAsset,
		CreatedAt: cmd.Timestamp,
	}
	if err := sqlcgen.New(tx).InsertTrade(ctx, tradeParams(t)); err != nil {
		return Trade{}, ledger.JournalEntry{}, fmt.Errorf("trading: insert trade: %w", err)
	}
	return t, res.Entry, nil
}

// release gives an order's remaining hold back (cancel, IOC remainder,
// residue on fill) and zeroes HoldRemaining. Key: release:order:{id}:{seq}
// (docs/domain.md §8 E4).
func (r *runner) release(ctx context.Context, tx pgx.Tx, o *Order, seq uint64, correlation string) (ledger.JournalEntry, error) {
	entry, _, err := r.ledger.Release(ctx, tx, ledger.HoldParams{
		AccountID: o.AccountID, Asset: o.HoldAsset, Amount: o.HoldRemaining,
		IdempotencyKey: fmt.Sprintf("release:order:%s:%d", o.ID, seq), Ref: ledger.Ref{Type: "order", ID: o.ID}, CorrelationID: correlation,
	})
	if err != nil {
		return ledger.JournalEntry{}, fmt.Errorf("trading: release order %s: %w", o.ID, err)
	}
	o.HoldRemaining = money.Zero
	return entry, nil
}

// cancelOrder runs the user-cancel flow. Terminal orders are returned as
// they are (the cancel is idempotent, docs/plan-v1.0.md §6.2).
func (r *runner) cancelOrder(ctx context.Context, req CancelRequest) (Order, error) {
	m, ok := r.market()
	if !ok {
		return Order{}, fmt.Errorf("%w: %s", ErrMarketNotFound, r.symbol)
	}
	cctx, cancel := txContext(ctx)
	defer cancel()
	current, err := getOrder(cctx, r.pool, req.OrderID)
	if err != nil {
		return Order{}, err
	}
	if current.AccountID != req.AccountID || current.TenantID != r.tenant {
		return Order{}, ErrOrderNotFound
	}
	if current.Status.Terminal() {
		return current, nil
	}
	if reason := r.policy.Cancel(m); reason != "" {
		return Order{}, fmt.Errorf("%w: %s", ErrMarketNotFound, reason)
	}
	now := time.Now().UTC().Truncate(time.Microsecond) // timestamptz precision: memory == database
	cmd := matching.Command{Seq: r.seq + 1, Timestamp: now, Cancel: &matching.Cancel{OrderID: matching.OrderID(req.OrderID), AccountID: matching.AccountID(req.AccountID)}}
	var out Order
	applied := false
	err = r.inTx(cctx, func(tx pgx.Tx) error {
		if err := r.advanceSeq(cctx, tx, cmd.Seq); err != nil {
			return err
		}
		events, err := r.book.Apply(cmd)
		if err != nil {
			return fmt.Errorf("trading: apply cancel: %w", err)
		}
		applied = true
		ev, ok := events[0].(matching.Cancelled)
		if !ok {
			// the database says open but the book does not know the order
			return fmt.Errorf("%w: cancel %s: %s", ErrBookInconsistent, req.OrderID, events[0].Kind())
		}
		o := current
		o.Status, o.CancelReason = StatusCancelled, ev.Reason
		o.FilledQty, o.FilledQuote, o.RemainingQty = ev.FilledQty, ev.FilledQuote, ev.RemainingQty
		var entries []ledger.JournalEntry
		released := o.HoldRemaining
		if released.IsPositive() {
			entry, err := r.release(cctx, tx, &o, cmd.Seq, req.CorrelationID)
			if err != nil {
				return err
			}
			entries = append(entries, entry)
		}
		row, err := sqlcgen.New(tx).UpdateOrderProgress(cctx, progressParams(o))
		if err != nil {
			return fmt.Errorf("trading: update cancelled %s: %w", o.ID, err)
		}
		if out, err = orderFromRow(row); err != nil {
			return err
		}
		b := &eventBuilder{tenant: r.tenant, market: m.Symbol, now: now, correlation: req.CorrelationID, seq: &cmd.Seq}
		b.add(EventOrderCancelled, o.AccountID, OrderCancelledPayload{
			OrderID: o.ID, AccountID: o.AccountID, Market: m.Symbol, Reason: ev.Reason,
			FilledQty: ev.FilledQty, FilledQuote: ev.FilledQuote, RemainingQty: ev.RemainingQty, RemainingQuote: ev.RemainingQuote,
			Released: released, ReleasedAsset: o.HoldAsset, Seq: cmd.Seq,
		})
		for _, bal := range balanceEvents(entries) {
			b.addAccountOnly(EventBalanceUpdated, bal.AccountID, BalanceUpdatedPayload{AccountID: bal.AccountID, Asset: bal.Asset, Available: bal.Available, Hold: bal.Hold})
		}
		return r.flushEvents(cctx, tx, b)
	})
	if err != nil {
		if applied {
			r.dirty = true
		}
		return Order{}, err
	}
	r.seq = cmd.Seq
	r.observe(out)
	return out, nil
}

// advanceSeq persists the next command seq with the guard that detects a
// second writer.
func (r *runner) advanceSeq(ctx context.Context, tx pgx.Tx, seq uint64) error {
	n, err := sqlcgen.New(tx).AdvanceMarketSequence(ctx, sqlcgen.AdvanceMarketSequenceParams{MarketID: r.mid, LastSeq: int64(seq)}) //nolint:gosec // engine seq
	if err != nil {
		return fmt.Errorf("trading: advance sequence: %w", err)
	}
	if n != 1 {
		r.dirty = true
		return fmt.Errorf("%w: market %s seq %d", ErrSequenceConflict, r.symbol, seq)
	}
	return nil
}

// flushEvents assigns account sequences and appends every pending event.
func (r *runner) flushEvents(ctx context.Context, tx pgx.Tx, b *eventBuilder) error {
	for _, p := range b.items {
		var accountSeq int64
		if p.account != "" {
			var err error
			if accountSeq, err = r.ledger.NextAccountSeq(ctx, tx, p.account); err != nil {
				return err
			}
			if bp, ok := p.payload.(BalanceUpdatedPayload); ok {
				bp.AccountSeq = accountSeq
				p.payload = bp
			}
		}
		env, err := b.envelope(p, accountSeq)
		if err != nil {
			return err
		}
		if _, err := r.outbox.Append(ctx, tx, env); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) tradesOfOrder(ctx context.Context, orderID string) ([]Trade, error) {
	rows, err := sqlcgen.New(r.pool).ListTradesByOrder(ctx, orderID)
	if err != nil {
		return nil, fmt.Errorf("trading: trades of order: %w", err)
	}
	return tradesFromRows(rows)
}

func (r *runner) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("trading: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("trading: commit: %w", err)
	}
	return nil
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

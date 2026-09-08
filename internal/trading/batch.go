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
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/trading/sqlcgen"
)

// A runner persists the commands it takes off its queue in groups: one
// Postgres transaction per group, every command inside its own savepoint,
// the replies sent only after COMMIT (docs/plan-v1.0.md §5.2 and
// docs/loadtest.md §5, which measured the engine saturating on round trips).
//
// Round trips, for a group of n commands:
//
//	1   BEGIN + the pre-reads of every command + the sequence lock
//	    + the first command's locks
//	n-1 each command's writes ride with the next command's locks
//	+1  per command that settles trades (their locks, then their writes)
//	1   the last command's writes + the account sequences
//	1   the outbox rows + the market sequence + COMMIT
//
// so a lone resting order costs four, a lone fill five, and a full group
// about one per command. The book is applied in memory as the group is
// walked; queries wait for the group, so nobody observes a book the
// database may still roll back.
//
// Postgres decides what can share a round trip: the first failing
// statement aborts the transaction, so a statement whose failure the
// engine must recover from (the hold of an order the book then rejects)
// is sent, checked in Go, and only then followed by the writes. Every
// command is bracketed by a savepoint so a rejection undoes exactly its
// own rows.
//
// Any infrastructure error (the database, a deadlock with another market
// touching the same accounts, a book/database disagreement) fails the
// whole group: ROLLBACK, the book is marked dirty and rebuilt, and the
// commands are re-run one per transaction in the same order, so a
// poisoned command fails alone and the others still commit.

// commandKind is what a queued request asks the book to do.
type commandKind int

const (
	kindPlace commandKind = iota
	kindCancel
)

// command is one place or cancel request as the group walks it.
type command struct {
	req  request
	kind commandKind
	ctx  context.Context // the request's trace, for its spans and outbox rows
	end  func(error)

	// pre-reads (the group's first round trip)
	existing *pg.Result[sqlcgen.TradingOrder] // place: the order under this client id; cancel: the order
	account  *ledger.PendingAccount

	// state
	done  bool // answered without touching the book (replay, policy, terminal cancel)
	resp  response
	order Order   // the taker (place) or the order being cancelled
	seq   *uint64 // consumed sequence, once the command reached the book
	sp    string  // savepoint name while one is open
	now   time.Time

	hold     *ledger.PendingEntry
	release  *ledger.PendingEntry // cancel: the order's remaining hold
	settles  []*pendingSettle     // place: one per trade
	releases []*pendingRelease    // place: taker remainder, filled residue
	trades   []Trade
	makers   map[string]*Order // resting orders this command changed (cache pointers)
	events   *eventBuilder
	rejected *pg.Result[sqlcgen.TradingOrder] // the rejected order row
	progress []*pg.Result[sqlcgen.TradingOrder]
	touched  []*Order                         // the resting orders behind progress, in order
	final    *pg.Result[sqlcgen.TradingOrder] // the taker's / cancelled order's final row
	deferred *Order                           // cancel of an order an earlier command in the group ended
}

type pendingSettle struct {
	entry *ledger.PendingEntry
	res   ledger.SettleResult
}

type pendingRelease struct {
	entry *ledger.PendingEntry
	order *Order
}

// key identifies the command for the drain loop: a second place with the
// same (account, client_order_id) or a second cancel of the same order
// waits for the next group, where the pre-read sees what the first did.
func (req request) key() string {
	switch {
	case req.place != nil:
		return "p:" + req.place.AccountID + ":" + req.place.ClientOrderID
	case req.cancel != nil:
		return "c:" + req.cancel.OrderID
	}
	return ""
}

// handleGroup executes reqs as one group, falling back to one transaction
// per command when the group fails for a reason that is not one command's
// own business outcome. Every request is answered.
func (r *runner) handleGroup(ctx context.Context, reqs []request) {
	start := time.Now()
	cmds := make([]*command, 0, len(reqs))
	for _, req := range reqs {
		c := &command{req: req, now: time.Now().UTC().Truncate(time.Microsecond)} // timestamptz precision: memory == database
		if req.place != nil {
			c.kind = kindPlace
		} else {
			c.kind = kindCancel
		}
		cctx := ctx
		if req.ctx != nil {
			cctx = telemetry.WithSpanOf(ctx, req.ctx)
		}
		c.ctx, c.end = telemetry.StartSpan(cctx, "engine "+req.kind(), telemetry.SpanInternal, map[string]string{"exchange.market": r.symbol})
		cmds = append(cmds, c)
	}
	if r.metrics != nil {
		r.metrics.observeBatch(r.symbol, len(cmds))
	}
	err := r.executeGroup(ctx, cmds)
	if err == nil {
		for _, c := range cmds {
			c.end(c.resp.err)
			c.req.reply <- c.resp
		}
		if r.metrics != nil {
			r.metrics.batchDuration.WithLabelValues(r.symbol).Observe(time.Since(start).Seconds())
			for range cmds {
				r.metrics.applyDuration.WithLabelValues(r.symbol).Observe(time.Since(start).Seconds())
			}
		}
		return
	}

	// the group failed as a whole
	r.dirty = true
	reason := failureReason(err)
	if r.metrics != nil {
		r.metrics.batchFallbacks.WithLabelValues(r.symbol, reason).Inc()
	}
	for _, c := range cmds {
		c.end(err)
	}
	if len(cmds) == 1 {
		c := cmds[0]
		if pgErrorCode(err) == "23505" { // client_order_id raced with another market
			err = ErrClientOrderIDMismatch
		}
		r.log.Error("command failed", slog.String("op", c.req.kind()), slog.String("err", err.Error()))
		c.req.reply <- response{err: err}
		return
	}
	r.log.Warn("command group failed; re-running its commands one by one",
		slog.Int("commands", len(cmds)), slog.String("reason", reason), slog.String("err", err.Error()))
	abort := false
	for i, req := range reqs {
		if abort || (r.dirty && r.restoreOrFail(ctx) != nil) {
			req.reply <- response{err: fmt.Errorf("%w: earlier command in the group failed", ErrEngineUnavailable)}
			continue
		}
		r.handleGroup(ctx, reqs[i:i+1])
		if r.dirty && !r.retryable(ctx) {
			// the database is gone rather than one command being bad:
			// answer the rest now instead of paying a timeout each
			abort = true
		}
	}
}

// restoreOrFail rebuilds a dirty book; the error is what the caller
// answers with.
func (r *runner) restoreOrFail(ctx context.Context) error {
	if !r.dirty {
		return nil
	}
	if err := r.restore(ctx); err != nil {
		r.log.Error("order book rebuild failed", slog.String("err", err.Error()))
		return fmt.Errorf("%w: %v", ErrEngineUnavailable, err)
	}
	if r.metrics != nil {
		r.metrics.rebuilds.Inc()
	}
	return nil
}

// retryable reports whether the database still answers: a single command
// that failed for its own reasons leaves the book dirty but the database
// up; a lost connection does not, and the rest of the group should not
// each wait out a timeout to find that.
func (r *runner) retryable(ctx context.Context) bool {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return r.pool.Ping(pctx) == nil
}

// failureReason labels a group failure for the fallback counter.
func failureReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrSequenceConflict):
		return "sequence"
	case errors.Is(err, ErrBookInconsistent):
		return "inconsistent"
	}
	switch pgErrorCode(err) {
	case "40P01":
		return "deadlock"
	case "40001":
		return "serialization"
	}
	return "error"
}

// executeGroup runs the group in one transaction. It returns nil when the
// transaction committed and every command carries its response; any error
// means nothing was committed.
func (r *runner) executeGroup(ctx context.Context, cmds []*command) error {
	if err := r.restoreOrFail(ctx); err != nil {
		for _, c := range cmds {
			c.resp = response{err: err}
		}
		return nil
	}
	m, ok := r.market()
	if !ok {
		for _, c := range cmds {
			c.resp = response{err: fmt.Errorf("%w: %s", ErrMarketNotFound, r.symbol)}
		}
		return nil
	}
	cctx, cancel := groupContext(ctx, len(cmds))
	defer cancel()
	conn, err := r.pool.Acquire(cctx)
	if err != nil {
		return fmt.Errorf("trading: acquire: %w", err)
	}
	defer conn.Release()
	committed := false
	defer func() {
		if !committed {
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(cctx), 2*time.Second)
			_, _ = conn.Exec(rctx, "ROLLBACK")
			rcancel()
		}
	}()

	// round trip 1: BEGIN, every command's pre-reads, the sequence lock,
	// and the first command's locks (speculative: if the pre-reads answer
	// it, its savepoint is rolled back)
	b := &pg.Batch{}
	b.QueueExec("BEGIN")
	for _, c := range cmds {
		build(c, r, m)
		r.queuePreReads(b, c)
	}
	seqRow := pg.QueueValue[int64](b, sqlcgen.LockMarketSequence, r.mid)
	next := r.seq
	r.touched = map[string]*Order{}
	if err := r.prepare(cmds[0], m, next); err != nil {
		cmds[0].done, cmds[0].resp = true, response{err: err}
	} else {
		r.queueLocks(b, cmds[0], 0)
	}
	if err := b.Send(cctx, conn); err != nil {
		return fmt.Errorf("trading: begin group: %w", err)
	}
	if seqRow.Err != nil {
		return fmt.Errorf("%w: no sequence row for %s", ErrBookInconsistent, r.symbol)
	}
	if uint64(seqRow.Val) != r.seq { //nolint:gosec // last_seq >= 0 by CHECK
		return fmt.Errorf("%w: market %s at %d, engine at %d", ErrSequenceConflict, r.symbol, seqRow.Val, r.seq)
	}
	for _, c := range cmds {
		if err := r.resolve(cctx, conn, c, m); err != nil {
			return err
		}
	}

	pending := &pg.Batch{} // statements waiting to ride with the next send
	for i, c := range cmds {
		if c.done {
			if c.sp != "" { // the first command's speculative locks
				pending.QueueExec("ROLLBACK TO SAVEPOINT " + c.sp)
				pending.QueueExec("RELEASE SAVEPOINT " + c.sp)
				c.sp = ""
			}
			if c.resp.err == nil && c.order.Status == StatusRejected && c.rejected == nil {
				r.queueRejected(pending, c, nil) // policy rejection: no sequence
			}
			continue
		}
		if i > 0 {
			if c.kind == kindCancel {
				if _, ok := r.orders[c.req.cancel.OrderID]; !ok {
					// open when the group began, terminal by now (filled or
					// cancelled by an earlier command): answered as it stands,
					// no sequence consumed
					if t, ok := r.touched[c.req.cancel.OrderID]; ok {
						c.done, c.deferred = true, t
						continue
					}
					return fmt.Errorf("%w: cancel %s: not in the book", ErrBookInconsistent, c.req.cancel.OrderID)
				}
			}
			if err := r.prepare(c, m, next); err != nil {
				c.done, c.resp = true, response{err: err}
				continue
			}
			r.queueLocks(pending, c, i)
		}
		if err := pending.Send(cctx, conn); err != nil {
			return fmt.Errorf("trading: command %d: %w", i, err)
		}
		pending = &pg.Batch{}
		var err error
		switch c.kind {
		case kindPlace:
			err = r.walkPlace(cctx, conn, pending, c, m, &next)
		case kindCancel:
			err = r.walkCancel(cctx, conn, pending, c, m, &next)
		}
		if err != nil {
			return err
		}
	}

	// the account sequences ride with the last command's writes: how many
	// events each account gets is known once every command is walked
	bumps := r.queueAccountSeqs(pending, cmds)
	if err := pending.Send(cctx, conn); err != nil {
		return fmt.Errorf("trading: finish group: %w", err)
	}
	for _, c := range cmds {
		if err := r.finish(c); err != nil {
			return err
		}
	}

	if r.beforeCommit != nil {
		if err := r.beforeCommit(r.symbol, len(cmds)); err != nil {
			return fmt.Errorf("trading: fault injection: %w", err)
		}
	}

	// the outbox rows, the market sequence, COMMIT
	b = &pg.Batch{}
	if err := r.queueOutbox(b, cmds, bumps); err != nil {
		return err
	}
	var advanced *pg.Result[int64]
	if next > r.seq {
		advanced = b.QueueExecRows(sqlcgen.SetMarketSequence, r.mid, int64(next), int64(r.seq)) //nolint:gosec // engine seq
	}
	commit := b.QueueExecTag("COMMIT")
	if err := b.Send(cctx, conn); err != nil {
		return fmt.Errorf("trading: commit group: %w", err)
	}
	if advanced != nil && advanced.Val != 1 {
		return fmt.Errorf("%w: market %s seq %d", ErrSequenceConflict, r.symbol, next)
	}
	if commit.Val.String() != "COMMIT" {
		return fmt.Errorf("trading: commit group: transaction ended with %s", commit.Val.String())
	}
	committed = true
	r.seq = next
	for _, c := range cmds {
		r.respond(c, m)
	}
	return nil
}

// commandTimeoutPerCommand extends a group's budget per command.
const commandTimeoutPerCommand = 100 * time.Millisecond

// groupContext detaches the group's database work from the callers'
// cancellation but keeps the runner's values; the budget grows with the
// group.
func groupContext(ctx context.Context, n int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), commandTimeout+time.Duration(n)*commandTimeoutPerCommand)
}

// queuePreReads queues what the command needs to know before the book:
// a place reads the order under its client id (replay) and the account
// (policy); a cancel reads its order.
func (r *runner) queuePreReads(b *pg.Batch, c *command) {
	switch c.kind {
	case kindPlace:
		req := c.req.place
		c.existing = pg.QueueOne[sqlcgen.TradingOrder](b, sqlcgen.GetOrderByClientID, r.tenant, req.AccountID, req.ClientOrderID)
		c.account = r.ledger.QueueAccount(b, req.AccountID)
	case kindCancel:
		c.existing = pg.QueueOne[sqlcgen.TradingOrder](b, sqlcgen.GetOrder, c.req.cancel.OrderID)
	}
}

// build fills in the order a place command is about to create; every
// path (accepted, rejected before or after the book) writes this row.
func build(c *command, r *runner, m registry.Market) {
	if c.kind != kindPlace {
		return
	}
	req := *c.req.place
	holdAsset, _ := holdFor(m, req)
	c.order = Order{
		ID: eventbus.NewID(c.now), TenantID: r.tenant, AccountID: req.AccountID, MarketID: m.ID, MarketSymbol: m.Symbol,
		ClientOrderID: req.ClientOrderID, Side: req.Side, Type: req.Type, TimeInForce: req.effectiveTIF(),
		FilledQty: money.Zero, FilledQuote: money.Zero, RemainingQty: req.Qty,
		HoldAsset: holdAsset, HoldAmount: money.Zero, HoldRemaining: money.Zero,
		CorrelationID: req.CorrelationID, CreatedAt: c.now, UpdatedAt: c.now,
	}
	if req.Price.IsPositive() {
		p := req.Price
		c.order.Price = &p
	}
	if req.Qty.IsPositive() {
		q := req.Qty
		c.order.Qty = &q
	}
	if req.QuoteQty.IsPositive() {
		q := req.QuoteQty
		c.order.QuoteQty = &q
	}
}

// prepare begins the command's ledger entry for the locks round trip: the
// hold of a new order, the release of a cancel. A cancel's release is only
// possible when the order rests in the book (the cache says how much is
// still held); next is the sequence the cancel will consume.
func (r *runner) prepare(c *command, m registry.Market, next uint64) error {
	switch c.kind {
	case kindPlace:
		req := *c.req.place
		holdAsset, holdAmount := holdFor(m, req)
		hold, err := r.ledger.BeginHold(ledger.HoldParams{
			AccountID: c.order.AccountID, Asset: holdAsset, Amount: holdAmount,
			IdempotencyKey: "hold:order:" + c.order.ID, Ref: ledger.Ref{Type: "order", ID: c.order.ID}, CorrelationID: req.CorrelationID,
		})
		if err != nil {
			if errors.Is(err, ledger.ErrInvalidEntry) {
				// a zero hold: the book will reject the order for the same reason
				return nil
			}
			return err
		}
		c.hold = hold
		return nil
	case kindCancel:
		o, ok := r.orders[c.req.cancel.OrderID]
		if !ok {
			return nil // terminal or unknown: resolve says which
		}
		c.order = *o
		if !o.HoldRemaining.IsPositive() {
			return nil
		}
		rel, err := r.ledger.BeginRelease(ledger.HoldParams{
			AccountID: o.AccountID, Asset: o.HoldAsset, Amount: o.HoldRemaining,
			IdempotencyKey: fmt.Sprintf("release:order:%s:%d", o.ID, next+1),
			Ref:            ledger.Ref{Type: "order", ID: o.ID}, CorrelationID: c.req.cancel.CorrelationID,
		})
		if err != nil {
			return err
		}
		c.release = rel
	}
	return nil
}

// queueLocks opens the command's savepoint and queues its ledger locks.
func (r *runner) queueLocks(b *pg.Batch, c *command, i int) {
	c.sp = fmt.Sprintf("cmd_%d", i)
	b.QueueExec("SAVEPOINT " + c.sp)
	switch {
	case c.kind == kindPlace && c.hold != nil:
		c.hold.QueueLocks(b)
	case c.kind == kindCancel && c.release != nil:
		c.release.QueueLocks(b)
	}
}

// resolve answers what the pre-reads decide: replays, reused client ids,
// unknown accounts, policy rejections, cancels of terminal or foreign
// orders. Everything else goes on to the book.
func (r *runner) resolve(ctx context.Context, conn *pgxpool.Conn, c *command, m registry.Market) error {
	if c.done {
		return nil
	}
	switch c.kind {
	case kindPlace:
		req := *c.req.place
		if c.existing.Err == nil {
			existing, err := orderFromRow(c.existing.Val)
			if err != nil {
				return err
			}
			if !req.sameAs(existing) {
				c.done, c.resp = true, response{err: ErrClientOrderIDMismatch}
				return nil
			}
			rows, err := sqlcgen.New(conn).ListTradesByOrder(ctx, existing.ID)
			if err != nil {
				return fmt.Errorf("trading: trades of order: %w", err)
			}
			trades, err := tradesFromRows(rows)
			if err != nil {
				return err
			}
			c.done, c.resp = true, response{result: PlaceOrderResult{Order: existing, Trades: trades, Replayed: true}}
			return nil
		}
		account, err := c.account.Get()
		if err != nil {
			if errors.Is(err, ledger.ErrAccountNotFound) {
				c.done, c.resp = true, response{err: fmt.Errorf("%w: account %s", ErrInvalidRequest, req.AccountID)}
				return nil
			}
			return err
		}
		if reason := r.policy.NewOrder(m, account); reason != "" {
			c.done = true
			c.order.Status, c.order.RejectReason = StatusRejected, RejectReason(reason)
		}
	case kindCancel:
		req := *c.req.cancel
		if c.existing.Err != nil {
			c.done, c.resp = true, response{err: ErrOrderNotFound}
			return nil
		}
		current, err := orderFromRow(c.existing.Val)
		if err != nil {
			return err
		}
		if current.AccountID != req.AccountID || current.TenantID != r.tenant {
			c.done, c.resp = true, response{err: ErrOrderNotFound}
			return nil
		}
		if current.Status.Terminal() {
			c.done, c.resp = true, response{result: PlaceOrderResult{Order: current}}
			return nil
		}
		if reason := r.policy.Cancel(m); reason != "" {
			c.done, c.resp = true, response{err: fmt.Errorf("%w: %s", ErrMarketNotFound, reason)}
			return nil
		}
		if _, ok := r.orders[current.ID]; !ok {
			// the database says open but the book does not know the order
			return fmt.Errorf("%w: cancel %s: not in the book", ErrBookInconsistent, current.ID)
		}
	}
	return nil
}

// walkPlace runs the new-order flow of docs/plan-v1.0.md §5.2 step 4 for
// one command whose locks are in: check funds, apply to the book, then
// queue the writes into pending (sent with the next command's locks).
func (r *runner) walkPlace(ctx context.Context, conn *pgxpool.Conn, pending *pg.Batch, c *command, m registry.Market, next *uint64) error {
	req := *c.req.place
	if c.hold != nil {
		replayed, err := c.hold.Check(ctx, conn)
		if err != nil {
			if errors.Is(err, ledger.ErrInsufficient) {
				*next++
				seq := *next
				c.seq = &seq
				if err := r.book.Advance(seq); err != nil {
					return fmt.Errorf("trading: advance: %w", err)
				}
				c.order.RejectReason = RejectInsufficientBalance
				r.queueRejected(pending, c, &seq)
				return nil
			}
			return err
		}
		if replayed {
			return fmt.Errorf("%w: hold key of %s already used", ErrBookInconsistent, c.order.ID)
		}
	}
	*next++
	seq := *next
	c.seq = &seq
	c.order.Seq = &seq
	cmd := matching.Command{Seq: seq, Timestamp: c.now, New: newOrderCommand(c.order.ID, req)}
	events, err := r.book.Apply(cmd)
	if err != nil {
		return fmt.Errorf("trading: apply: %w", err)
	}
	if rej, ok := events[0].(matching.Rejected); ok && len(events) == 1 {
		c.order.RejectReason = RejectReason(rej.Reason)
		r.queueRejected(pending, c, &seq)
		return nil
	}
	if c.hold == nil {
		return fmt.Errorf("%w: order %s accepted without a hold", ErrBookInconsistent, c.order.ID)
	}
	_, holdAmount := holdFor(m, req)
	c.order.HoldAmount, c.order.HoldRemaining = holdAmount, holdAmount
	c.order.Status = StatusOpen
	c.events = &eventBuilder{tenant: r.tenant, market: m.Symbol, now: c.now, correlation: req.CorrelationID, seq: &seq}
	c.makers = map[string]*Order{}
	taker := &c.order

	// the writes that need no further answer from the database: the hold,
	// the taker row, and every trade with its settlement's locks
	c.hold.QueueApply(pending)
	pg.QueueOne[sqlcgen.TradingOrder](pending, sqlcgen.InsertOrder, insertArgs(insertParams(*taker))...)
	c.events.add(EventOrderAccepted, taker.AccountID, OrderAcceptedPayload{
		OrderID: taker.ID, ClientOrderID: taker.ClientOrderID, AccountID: taker.AccountID, Market: m.Symbol,
		Side: taker.Side.String(), Type: taker.Type.String(), TimeInForce: taker.TimeInForce.String(),
		Price: taker.Price, Qty: taker.Qty, QuoteQty: taker.QuoteQty, Seq: seq,
	})
	fees := ledger.FeeParams{MakerBps: m.MakerBps, TakerBps: m.TakerBps, BaseScale: m.BaseScale, QuoteScale: m.QuoteScale}
	loadMaker := func(id string) (*Order, error) {
		if mo, ok := c.makers[id]; ok {
			return mo, nil
		}
		mo, ok := r.orders[id]
		if !ok {
			return nil, fmt.Errorf("%w: maker %s not in the book", ErrBookInconsistent, id)
		}
		c.makers[id] = mo
		return mo, nil
	}
	for _, ev := range events[1:] {
		switch e := ev.(type) {
		case matching.Trade:
			maker, err := loadMaker(string(e.MakerOrderID))
			if err != nil {
				return err
			}
			t, ps, err := r.beginSettle(m, fees, cmd, e, taker, maker)
			if err != nil {
				return err
			}
			c.settles = append(c.settles, ps)
			c.trades = append(c.trades, t)
			ps.entry.QueueLocks(pending)
			pending.QueueExec(sqlcgen.InsertTrade, tradeArgs(tradeParams(t))...)
			c.events.add(EventTradeExecuted, "", TradeExecutedPayload{
				TradeID: t.ID, Market: m.Symbol, MakerOrderID: t.MakerOrderID, TakerOrderID: t.TakerOrderID,
				MakerAccountID: t.MakerAccountID, TakerAccountID: t.TakerAccountID, TakerSide: t.TakerSide.String(),
				Price: t.Price, Qty: t.Qty, QuoteQty: t.QuoteQty, MakerFee: t.MakerFee, MakerFeeAsset: t.MakerFeeAsset,
				TakerFee: t.TakerFee, TakerFeeAsset: t.TakerFeeAsset, Seq: seq,
			})
		case matching.Updated:
			o := taker
			if string(e.OrderID) != taker.ID {
				var err error
				if o, err = loadMaker(string(e.OrderID)); err != nil {
					return err
				}
			}
			o.FilledQty, o.FilledQuote, o.RemainingQty, o.Status = e.FilledQty, e.FilledQuote, e.RemainingQty, StatusPartiallyFilled
			c.events.add(EventOrderUpdated, o.AccountID, OrderUpdatedPayload{
				OrderID: o.ID, AccountID: o.AccountID, Market: m.Symbol, Status: o.Status,
				FilledQty: o.FilledQty, FilledQuote: o.FilledQuote, RemainingQty: o.RemainingQty, Seq: seq,
			})
		case matching.Filled:
			o := taker
			if string(e.OrderID) != taker.ID {
				var err error
				if o, err = loadMaker(string(e.OrderID)); err != nil {
					return err
				}
			}
			o.FilledQty, o.FilledQuote, o.RemainingQty, o.Status = e.FilledQty, e.FilledQuote, money.Zero, StatusFilled
			// a filled order must have nothing frozen; release any residue
			// (rounding) so Σ hold_remaining == balances.hold stays exact
			if o.HoldRemaining.IsPositive() {
				if err := r.beginRelease(pending, c, o, seq, req.CorrelationID); err != nil {
					return err
				}
			}
			c.events.add(EventOrderFilled, o.AccountID, OrderFilledPayload{
				OrderID: o.ID, AccountID: o.AccountID, Market: m.Symbol, FilledQty: o.FilledQty, FilledQuote: o.FilledQuote, Seq: seq,
			})
		case matching.Cancelled: // taker remainder: IOC, market, STP, price band
			taker.FilledQty, taker.FilledQuote, taker.RemainingQty = e.FilledQty, e.FilledQuote, e.RemainingQty
			taker.Status, taker.CancelReason = StatusCancelled, e.Reason
			released := taker.HoldRemaining
			if released.IsPositive() {
				if err := r.beginRelease(pending, c, taker, seq, req.CorrelationID); err != nil {
					return err
				}
			}
			c.events.add(EventOrderCancelled, taker.AccountID, OrderCancelledPayload{
				OrderID: taker.ID, AccountID: taker.AccountID, Market: m.Symbol, Reason: e.Reason,
				FilledQty: e.FilledQty, FilledQuote: e.FilledQuote, RemainingQty: e.RemainingQty, RemainingQuote: e.RemainingQuote,
				Released: released, ReleasedAsset: taker.HoldAsset, Seq: seq,
			})
		default:
			return fmt.Errorf("%w: unexpected event %s after accepted", ErrBookInconsistent, ev.Kind())
		}
	}

	if len(c.settles) > 0 || len(c.releases) > 0 {
		// the settlements' and releases' locks must answer before their
		// writes: one round trip of this command's own
		if err := pending.Send(ctx, conn); err != nil {
			return fmt.Errorf("trading: settle %s: %w", c.order.ID, err)
		}
		*pending = pg.Batch{}
		for _, ps := range c.settles {
			if err := checkEntry(ctx, conn, ps.entry); err != nil {
				return err
			}
			ps.entry.QueueApply(pending)
		}
		for _, pr := range c.releases {
			if err := checkEntry(ctx, conn, pr.entry); err != nil {
				return err
			}
			pr.entry.QueueApply(pending)
		}
	}
	// the final states of every order this command touched
	for _, mo := range c.makers {
		c.progress = append(c.progress, pg.QueueOne[sqlcgen.TradingOrder](pending, sqlcgen.UpdateOrderProgress, progressArgs(progressParams(*mo))...))
		c.touched = append(c.touched, mo)
		if mo.Status.Terminal() {
			delete(r.orders, mo.ID)
			r.touched[mo.ID] = mo
		}
	}
	c.final = pg.QueueOne[sqlcgen.TradingOrder](pending, sqlcgen.UpdateOrderProgress, progressArgs(progressParams(*taker))...)
	pending.QueueExec("RELEASE SAVEPOINT " + c.sp)
	c.sp = ""
	// the cache must know the order now, not after COMMIT: the next command
	// of the group may fill it (as maker) or cancel it
	if taker.Status.Terminal() {
		r.touched[taker.ID] = taker
	} else {
		resting := *taker
		r.orders[taker.ID] = &resting
	}
	return nil
}

// checkEntry reads a settlement's or release's locks; funds are already
// held, so anything but success is a book/database disagreement.
func checkEntry(ctx context.Context, conn *pgxpool.Conn, pe *ledger.PendingEntry) error {
	replayed, err := pe.Check(ctx, conn)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBookInconsistent, pe.Kind(), err)
	}
	if replayed {
		return fmt.Errorf("%w: %s entry replayed", ErrBookInconsistent, pe.Kind())
	}
	return nil
}

// beginSettle books one fill: the ledger entry (fees, price-improvement
// release), the trade row, and the hold bookkeeping of both orders.
func (r *runner) beginSettle(m registry.Market, fees ledger.FeeParams, cmd matching.Command, e matching.Trade, taker, maker *Order) (Trade, *pendingSettle, error) {
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
	pe, res, err := r.ledger.BeginSettle(ledger.SettleParams{
		TradeID: tradeID, IdempotencyKey: "settle:trade:" + tradeID, CorrelationID: taker.CorrelationID,
		BuyerAccountID: buyer.AccountID, SellerAccountID: seller.AccountID, BuyerIsTaker: buyerIsTaker,
		BaseAsset: m.BaseSymbol, QuoteAsset: m.QuoteSymbol, Price: e.Price, Qty: e.Qty, QuoteQty: e.QuoteQty,
		BuyerLimitPrice: buyerLimit, Fees: fees,
	})
	if err != nil {
		return Trade{}, nil, fmt.Errorf("trading: settle trade %s: %w", tradeID, err)
	}
	// what each side no longer has frozen
	buyer.HoldRemaining = buyer.HoldRemaining.Sub(e.QuoteQty).Sub(res.Release)
	seller.HoldRemaining = seller.HoldRemaining.Sub(e.Qty)
	if buyer.HoldRemaining.IsNegative() || seller.HoldRemaining.IsNegative() {
		return Trade{}, nil, fmt.Errorf("%w: hold went negative settling %s", ErrBookInconsistent, tradeID)
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
	return t, &pendingSettle{entry: pe, res: res}, nil
}

// beginRelease gives an order's remaining hold back (cancel, IOC
// remainder, residue on fill) and zeroes HoldRemaining. Key:
// release:order:{id}:{seq} (docs/domain.md §8 E4).
func (r *runner) beginRelease(pending *pg.Batch, c *command, o *Order, seq uint64, correlation string) error {
	pe, err := r.ledger.BeginRelease(ledger.HoldParams{
		AccountID: o.AccountID, Asset: o.HoldAsset, Amount: o.HoldRemaining,
		IdempotencyKey: fmt.Sprintf("release:order:%s:%d", o.ID, seq), Ref: ledger.Ref{Type: "order", ID: o.ID}, CorrelationID: correlation,
	})
	if err != nil {
		return fmt.Errorf("trading: release order %s: %w", o.ID, err)
	}
	o.HoldRemaining = money.Zero
	pe.QueueLocks(pending)
	c.releases = append(c.releases, &pendingRelease{entry: pe, order: o})
	return nil
}

// walkCancel runs the user-cancel flow for one command whose release locks
// are in.
func (r *runner) walkCancel(ctx context.Context, conn *pgxpool.Conn, pending *pg.Batch, c *command, m registry.Market, next *uint64) error {
	req := *c.req.cancel
	o, ok := r.orders[req.OrderID]
	if !ok {
		return fmt.Errorf("%w: cancel %s: not in the book", ErrBookInconsistent, req.OrderID)
	}
	if c.release != nil {
		if err := checkEntry(ctx, conn, c.release); err != nil {
			return err
		}
	}
	*next++
	seq := *next
	c.seq = &seq
	cmd := matching.Command{Seq: seq, Timestamp: c.now, Cancel: &matching.Cancel{OrderID: matching.OrderID(req.OrderID), AccountID: matching.AccountID(req.AccountID)}}
	events, err := r.book.Apply(cmd)
	if err != nil {
		return fmt.Errorf("trading: apply cancel: %w", err)
	}
	ev, ok := events[0].(matching.Cancelled)
	if !ok {
		return fmt.Errorf("%w: cancel %s: %s", ErrBookInconsistent, req.OrderID, events[0].Kind())
	}
	o.Status, o.CancelReason = StatusCancelled, ev.Reason
	o.FilledQty, o.FilledQuote, o.RemainingQty = ev.FilledQty, ev.FilledQuote, ev.RemainingQty
	released := o.HoldRemaining
	if c.release != nil {
		c.release.QueueApply(pending)
		o.HoldRemaining = money.Zero
	}
	c.final = pg.QueueOne[sqlcgen.TradingOrder](pending, sqlcgen.UpdateOrderProgress, progressArgs(progressParams(*o))...)
	pending.QueueExec("RELEASE SAVEPOINT " + c.sp)
	c.sp = ""
	delete(r.orders, o.ID)
	r.touched[o.ID] = o
	c.order = *o
	c.events = &eventBuilder{tenant: r.tenant, market: m.Symbol, now: c.now, correlation: req.CorrelationID, seq: &seq}
	c.events.add(EventOrderCancelled, o.AccountID, OrderCancelledPayload{
		OrderID: o.ID, AccountID: o.AccountID, Market: m.Symbol, Reason: ev.Reason,
		FilledQty: ev.FilledQty, FilledQuote: ev.FilledQuote, RemainingQty: ev.RemainingQty, RemainingQuote: ev.RemainingQuote,
		Released: released, ReleasedAsset: o.HoldAsset, Seq: seq,
	})
	return nil
}

// queueRejected undoes the command's savepoint and writes the order as
// rejected with its event (seq nil for pre-book rejections).
func (r *runner) queueRejected(pending *pg.Batch, c *command, seq *uint64) {
	if c.sp != "" {
		pending.QueueExec("ROLLBACK TO SAVEPOINT " + c.sp)
		pending.QueueExec("RELEASE SAVEPOINT " + c.sp)
		c.sp = ""
	}
	c.order.Status = StatusRejected
	c.order.Seq = seq
	c.order.RemainingQty, c.order.HoldAmount, c.order.HoldRemaining = money.Zero, money.Zero, money.Zero
	c.rejected = pg.QueueOne[sqlcgen.TradingOrder](pending, sqlcgen.InsertOrder, insertArgs(insertParams(c.order))...)
	c.events = &eventBuilder{tenant: r.tenant, market: c.order.MarketSymbol, now: c.now, correlation: c.order.CorrelationID, seq: seq}
	c.events.add(EventOrderRejected, c.order.AccountID, nil) // payload filled in finish, once the reason is final
}

// queueAccountSeqs reserves each account's private-stream sequences for
// the group in one statement per account: as many as the account has
// order events plus balances it changed, counted before anything is read
// back. Returns the reservations in first-appearance order.
func (r *runner) queueAccountSeqs(pending *pg.Batch, cmds []*command) map[string]*pg.Result[int64] {
	counts := map[string]int{}
	var order []string
	bump := func(account string) {
		if account == "" {
			return
		}
		if _, seen := counts[account]; !seen {
			order = append(order, account)
		}
		counts[account]++
	}
	for _, c := range cmds {
		if c.events == nil {
			continue
		}
		for _, p := range c.events.items {
			bump(p.account)
		}
		for _, k := range c.balanceKeys() {
			bump(k.AccountID)
		}
	}
	out := make(map[string]*pg.Result[int64], len(order))
	for _, account := range order {
		out[account] = r.ledger.QueueReserveAccountSeqs(pending, account, counts[account])
	}
	return out
}

// balanceKeys lists the (account, asset) balances the command changed, in
// the order their balance.updated events are published (first appearance,
// latest value wins).
func (c *command) balanceKeys() []ledger.BalanceKey {
	seen := map[ledger.BalanceKey]struct{}{}
	var out []ledger.BalanceKey
	add := func(pe *ledger.PendingEntry) {
		if pe == nil {
			return
		}
		for _, k := range pe.BalanceKeys() {
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				out = append(out, k)
			}
		}
	}
	if c.order.Status != StatusRejected {
		add(c.hold)
	}
	add(c.release)
	for _, ps := range c.settles {
		add(ps.entry)
	}
	for _, pr := range c.releases {
		add(pr.entry)
	}
	return out
}

// finish reads the command's writes back: the ledger balances (for the
// balance.updated events), the final order rows.
func (r *runner) finish(c *command) error {
	if c.done && c.rejected == nil {
		return nil
	}
	if c.rejected != nil {
		if c.rejected.Err != nil {
			return fmt.Errorf("%w: rejected order %s not written", ErrBookInconsistent, c.order.ID)
		}
		saved, err := orderFromRow(c.rejected.Val)
		if err != nil {
			return err
		}
		c.order = saved
		c.events.items[0].payload = OrderRejectedPayload{
			OrderID: saved.ID, ClientOrderID: saved.ClientOrderID, AccountID: saved.AccountID, Market: saved.MarketSymbol,
			Reason: saved.RejectReason, Seq: saved.Seq,
		}
		return nil
	}
	var entries []ledger.JournalEntry
	finishEntry := func(pe *ledger.PendingEntry) error {
		if pe == nil {
			return nil
		}
		if err := pe.Finish(); err != nil {
			return err
		}
		entries = append(entries, pe.Entry())
		return nil
	}
	if err := finishEntry(c.hold); err != nil {
		return err
	}
	if err := finishEntry(c.release); err != nil {
		return err
	}
	for _, ps := range c.settles {
		if err := finishEntry(ps.entry); err != nil {
			return err
		}
	}
	for _, pr := range c.releases {
		if err := finishEntry(pr.entry); err != nil {
			return err
		}
	}
	for _, bal := range balanceEvents(entries) {
		c.events.addAccountOnly(EventBalanceUpdated, bal.AccountID, BalanceUpdatedPayload{AccountID: bal.AccountID, Asset: bal.Asset, Available: bal.Available, Hold: bal.Hold})
	}
	if c.final == nil || c.final.Err != nil {
		return fmt.Errorf("%w: order %s final row not written", ErrBookInconsistent, c.order.ID)
	}
	saved, err := orderFromRow(c.final.Val)
	if err != nil {
		return err
	}
	c.order = saved
	// the cache mirrors the row (updated_at included); a later command of
	// the group may have moved the order on, and its own finish, which
	// runs after this one, writes that later row over this one
	if p, ok := r.orders[saved.ID]; ok {
		*p = saved
	} else if t, ok := r.touched[saved.ID]; ok && t != &c.order {
		*t = saved
	}
	for i, p := range c.progress {
		if p.Err != nil {
			return fmt.Errorf("%w: maker row not written", ErrBookInconsistent)
		}
		row, err := orderFromRow(p.Val)
		if err != nil {
			return err
		}
		*c.touched[i] = row // the cache mirrors the row, updated_at included
	}
	return nil
}

// queueOutbox assigns the reserved account sequences in (command, event)
// order and queues every envelope. That order is what makes seq ascend
// with the outbox id per market and account_seq ascend per account.
func (r *runner) queueOutbox(b *pg.Batch, cmds []*command, bumps map[string]*pg.Result[int64]) error {
	nextSeq := map[string]int64{}
	for account, res := range bumps {
		if res.Err != nil {
			return fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, account)
		}
		nextSeq[account] = res.Val // the last one reserved; walked back below
	}
	counts := map[string]int{}
	for _, c := range cmds {
		if c.events == nil {
			continue
		}
		for _, p := range c.events.items {
			if p.account != "" {
				counts[p.account]++
			}
		}
	}
	for account, n := range counts {
		nextSeq[account] -= int64(n) - 1
	}
	for _, c := range cmds {
		if c.events == nil {
			continue
		}
		for _, p := range c.events.items {
			var accountSeq int64
			if p.account != "" {
				accountSeq = nextSeq[p.account]
				nextSeq[p.account]++
				if bp, ok := p.payload.(BalanceUpdatedPayload); ok {
					bp.AccountSeq = accountSeq
					p.payload = bp
				}
			}
			env, err := c.events.envelope(p, accountSeq)
			if err != nil {
				return err
			}
			if err := r.outbox.QueueAppend(c.ctx, b, env); err != nil {
				return err
			}
		}
	}
	return nil
}

// respond builds the reply of a committed command and updates the cache
// of resting orders.
func (r *runner) respond(c *command, m registry.Market) {
	if c.deferred != nil {
		c.resp = response{result: PlaceOrderResult{Order: *c.deferred}}
		return
	}
	if c.done && c.rejected == nil {
		return // resp set by resolve
	}
	switch c.kind {
	case kindPlace:
		c.resp = response{result: PlaceOrderResult{Order: c.order, Trades: c.trades}}
	case kindCancel:
		c.resp = response{result: PlaceOrderResult{Order: c.order}}
	}
	r.observe(c.order)
	if r.metrics != nil && len(c.trades) > 0 {
		r.metrics.addTrades(m.Symbol, len(c.trades))
	}
}

// insertArgs lists InsertOrder's parameters in statement order.
func insertArgs(p sqlcgen.InsertOrderParams) []any {
	return []any{
		p.ID, p.TenantID, p.AccountID, p.MarketID, p.MarketSymbol, p.ClientOrderID,
		p.Side, p.Type, p.TimeInForce, p.Price, p.Qty, p.QuoteQty,
		p.FilledQty, p.FilledQuote, p.RemainingQty,
		p.HoldAsset, p.HoldAmount, p.HoldRemaining,
		p.Status, p.RejectReason, p.CancelReason, p.Seq, p.CorrelationID, p.CreatedAt,
	}
}

// progressArgs lists UpdateOrderProgress's parameters in statement order.
func progressArgs(p sqlcgen.UpdateOrderProgressParams) []any {
	return []any{p.ID, p.FilledQty, p.FilledQuote, p.RemainingQty, p.HoldRemaining, p.Status, p.CancelReason}
}

// tradeArgs lists InsertTrade's parameters in statement order.
func tradeArgs(p sqlcgen.InsertTradeParams) []any {
	return []any{
		p.ID, p.TenantID, p.MarketID, p.MarketSymbol, p.Seq, p.Idx,
		p.MakerOrderID, p.TakerOrderID, p.MakerAccountID, p.TakerAccountID, p.TakerSide,
		p.Price, p.Qty, p.QuoteQty, p.MakerFee, p.MakerFeeAsset, p.TakerFee, p.TakerFeeAsset, p.CreatedAt,
	}
}

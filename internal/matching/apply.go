package matching

import (
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Apply executes one command and returns the resulting events in order:
// Accepted, then for each fill a Trade followed by the maker's Updated or
// Filled, then the taker's terminal or resting event (Filled, Cancelled or
// Updated). Rejected and CancelRejected are the only events of a refused
// command. Errors are caller bugs (seq, malformed command); a valid seq with
// any order content never returns an error.
func (b *Book) Apply(cmd Command) ([]Event, error) {
	if cmd.Seq <= b.lastSeq {
		return nil, fmt.Errorf("%w: seq %d after %d", ErrSeqNotIncreasing, cmd.Seq, b.lastSeq)
	}
	var (
		events []Event
		err    error
	)
	switch {
	case cmd.New != nil && cmd.Cancel == nil:
		events, err = b.applyNew(cmd)
	case cmd.Cancel != nil && cmd.New == nil:
		events = b.applyCancel(cmd)
	default:
		return nil, fmt.Errorf("%w: exactly one of new or cancel must be set", ErrInvalidCommand)
	}
	if err != nil {
		return nil, err
	}
	b.lastSeq = cmd.Seq
	return events, nil
}

// taker is the in-flight state of the incoming order while it matches.
type taker struct {
	o           NewOrder
	remaining   money.Amount // base remaining (limit, market sell)
	quoteRem    money.Amount // unspent quote budget (market buy)
	filledQty   money.Amount
	filledQuote money.Amount
	stop        CancelReason // set when matching stopped early (self trade, price band)
}

func (t *taker) marketBuy() bool { return t.o.Type == Market && t.o.Side == Buy }

func (t *taker) exhausted() bool {
	if t.marketBuy() {
		return t.quoteRem.IsZero()
	}
	return t.remaining.IsZero()
}

func (b *Book) applyNew(cmd Command) ([]Event, error) {
	o := *cmd.New
	if o.Type == Market {
		o.TimeInForce = IOC
	} else if o.TimeInForce == 0 {
		o.TimeInForce = GTC
	}
	if reason := ValidateNewOrder(b.cfg, o); reason != "" {
		return []Event{Rejected{Seq: cmd.Seq, OrderID: o.OrderID, AccountID: o.AccountID, Reason: reason}}, nil
	}
	if _, dup := b.byID[o.OrderID]; dup {
		return []Event{Rejected{Seq: cmd.Seq, OrderID: o.OrderID, AccountID: o.AccountID, Reason: RejectDuplicateOrderID}}, nil
	}
	opp := b.sideOf(o.Side.Opposite())

	t := &taker{o: o, remaining: o.Qty, quoteRem: o.QuoteQty, filledQty: money.Zero, filledQuote: money.Zero}
	var band *money.Amount
	if o.Type == Market {
		// Market orders need liquidity now: an empty opposite side, or a
		// quote budget too small for one step at the best price, is a
		// rejection (nothing held, nothing to release).
		best := opp.best()
		if best == nil {
			return []Event{Rejected{Seq: cmd.Seq, OrderID: o.OrderID, AccountID: o.AccountID, Reason: RejectEmptyBook}}, nil
		}
		if t.marketBuy() {
			q, err := floorToStep(quotient(t.quoteRem, best.price), b.cfg.QtyStep)
			if err != nil {
				return nil, err
			}
			if q.IsZero() {
				return []Event{Rejected{Seq: cmd.Seq, OrderID: o.OrderID, AccountID: o.AccountID, Reason: RejectQuoteQtyTooSmall}}, nil
			}
		}
		if b.cfg.MaxSlippageBps != nil {
			limit, err := slippageBound(best.price, *b.cfg.MaxSlippageBps, o.Side)
			if err != nil {
				return nil, err
			}
			band = &limit
		}
	}

	events := []Event{Accepted{
		Seq: cmd.Seq, OrderID: o.OrderID, AccountID: o.AccountID, Side: o.Side, Type: o.Type,
		TimeInForce: o.TimeInForce, Price: o.Price, Qty: o.Qty, QuoteQty: o.QuoteQty,
	}}
	events, err := b.match(cmd, t, opp, band, events)
	if err != nil {
		return nil, err
	}

	switch {
	case t.exhausted():
		events = append(events, Filled{Seq: cmd.Seq, OrderID: o.OrderID, AccountID: o.AccountID, FilledQty: t.filledQty, FilledQuote: t.filledQuote})
	case t.stop != "":
		events = append(events, b.cancelledEvent(cmd.Seq, t, t.stop))
	case o.Type == Limit && o.TimeInForce == GTC:
		rest := &order{RestingOrder: RestingOrder{
			OrderID: o.OrderID, AccountID: o.AccountID, Side: o.Side, Price: o.Price, Qty: o.Qty,
			Remaining: t.remaining, FilledQty: t.filledQty, FilledQuote: t.filledQuote,
			Seq: cmd.Seq, Timestamp: cmd.Timestamp,
		}}
		b.sideOf(o.Side).insert(rest)
		b.byID[o.OrderID] = rest
		if t.filledQty.IsPositive() {
			events = append(events, Updated{Seq: cmd.Seq, OrderID: o.OrderID, AccountID: o.AccountID, FilledQty: t.filledQty, FilledQuote: t.filledQuote, RemainingQty: t.remaining})
		}
	default: // IOC limit or market remainder
		events = append(events, b.cancelledEvent(cmd.Seq, t, CancelIOC))
	}
	return events, nil
}

func (b *Book) cancelledEvent(seq uint64, t *taker, reason CancelReason) Cancelled {
	ev := Cancelled{
		Seq: seq, OrderID: t.o.OrderID, AccountID: t.o.AccountID, Reason: reason,
		FilledQty: t.filledQty, FilledQuote: t.filledQuote, RemainingQty: t.remaining, RemainingQuote: money.Zero,
	}
	if t.marketBuy() {
		ev.RemainingQty = money.Zero
		ev.RemainingQuote = t.quoteRem
	}
	return ev
}

// match walks the opposite side best level first, FIFO within a level,
// until the taker is exhausted, the price no longer crosses, the slippage
// band is hit, or a self-trade stops it.
func (b *Book) match(cmd Command, t *taker, opp *bookSide, band *money.Amount, events []Event) ([]Event, error) {
	o := t.o
	tradeIdx := 0
	for !t.exhausted() {
		lvl := opp.best()
		if lvl == nil {
			break
		}
		if o.Type == Limit && !crosses(o.Side, o.Price, lvl.price) {
			break
		}
		if band != nil && outsideBand(o.Side, *band, lvl.price) {
			t.stop = CancelPriceProtection
			break
		}
		stopped := false
		for len(lvl.orders) > 0 && !t.exhausted() {
			maker := lvl.orders[0]
			if maker.AccountID == o.AccountID && b.cfg.SelfTradePolicy == STPCancelNewest {
				t.stop = CancelSelfTrade
				stopped = true
				break
			}
			qty := t.remaining
			if t.marketBuy() {
				var err error
				qty, err = floorToStep(quotient(t.quoteRem, lvl.price), b.cfg.QtyStep)
				if err != nil {
					return nil, err
				}
				if qty.IsZero() {
					// budget left is smaller than one step at this price: done
					stopped = true
					break
				}
			}
			if maker.Remaining.Cmp(qty) < 0 {
				qty = maker.Remaining
			}
			quote := lvl.price.Mul(qty)
			if quote.Scale() > b.cfg.QuoteScale {
				return nil, fmt.Errorf("%w: %s x %s", errInternalPrecision, lvl.price, qty)
			}

			maker.Remaining = maker.Remaining.Sub(qty)
			maker.FilledQty = maker.FilledQty.Add(qty)
			maker.FilledQuote = maker.FilledQuote.Add(quote)
			lvl.qty = lvl.qty.Sub(qty)
			t.filledQty = t.filledQty.Add(qty)
			t.filledQuote = t.filledQuote.Add(quote)
			if t.marketBuy() {
				t.quoteRem = t.quoteRem.Sub(quote)
			} else {
				t.remaining = t.remaining.Sub(qty)
			}

			events = append(events, Trade{
				Seq: cmd.Seq, Index: tradeIdx,
				MakerOrderID: maker.OrderID, TakerOrderID: o.OrderID,
				MakerAccountID: maker.AccountID, TakerAccountID: o.AccountID,
				TakerSide: o.Side, Price: lvl.price, Qty: qty, QuoteQty: quote, MakerRemaining: maker.Remaining,
			})
			tradeIdx++
			if maker.Remaining.IsZero() {
				events = append(events, Filled{Seq: cmd.Seq, OrderID: maker.OrderID, AccountID: maker.AccountID, FilledQty: maker.FilledQty, FilledQuote: maker.FilledQuote})
				delete(b.byID, maker.OrderID)
				opp.popHead(lvl) // may drop the level; the outer loop re-reads best()
			} else {
				events = append(events, Updated{Seq: cmd.Seq, OrderID: maker.OrderID, AccountID: maker.AccountID, FilledQty: maker.FilledQty, FilledQuote: maker.FilledQuote, RemainingQty: maker.Remaining})
			}
		}
		if stopped {
			break
		}
	}
	return events, nil
}

func (b *Book) applyCancel(cmd Command) []Event {
	c := cmd.Cancel
	o, ok := b.byID[c.OrderID]
	if !ok {
		return []Event{CancelRejected{Seq: cmd.Seq, OrderID: c.OrderID, Reason: CancelRejectUnknownOrder}}
	}
	if c.AccountID != "" && c.AccountID != o.AccountID {
		return []Event{CancelRejected{Seq: cmd.Seq, OrderID: c.OrderID, Reason: CancelRejectNotOwner}}
	}
	b.remove(o)
	return []Event{Cancelled{
		Seq: cmd.Seq, OrderID: o.OrderID, AccountID: o.AccountID, Reason: CancelByUser,
		FilledQty: o.FilledQty, FilledQuote: o.FilledQuote, RemainingQty: o.Remaining, RemainingQuote: money.Zero,
	}}
}

// crosses reports whether a limit order at limit may trade at price.
// Equal prices trade (docs/domain.md §9 Q1: standard, price-taker friendly).
func crosses(side Side, limit, price money.Amount) bool {
	if side == Buy {
		return price.Cmp(limit) <= 0
	}
	return price.Cmp(limit) >= 0
}

// outsideBand reports whether a level price is beyond the market order's
// protection bound.
func outsideBand(side Side, bound, price money.Amount) bool {
	if side == Buy {
		return price.Cmp(bound) > 0
	}
	return price.Cmp(bound) < 0
}

// slippageBound is the worst price a market order accepts, relative to the
// best opposite price when it arrives: best × (1 ± bps/10000). The bound
// is computed exactly (18 decimals) and need not be a tick multiple.
func slippageBound(best money.Amount, bps int32, side Side) (money.Amount, error) {
	factor := int64(10000) + int64(bps)
	if side == Sell {
		factor = int64(10000) - int64(bps)
	}
	return best.Mul(money.FromInt64(factor)).DivRoundDown(money.FromInt64(10000), money.MaxScale)
}

// quotient returns quote / price truncated to the maximum scale; callers
// floor it to the quantity step. price is a positive level price.
func quotient(quote, price money.Amount) money.Amount {
	q, err := quote.DivRoundDown(price, money.MaxScale)
	if err != nil {
		// price is a validated positive tick multiple; unreachable
		return money.Zero
	}
	return q
}

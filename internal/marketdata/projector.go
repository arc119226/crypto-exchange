package marketdata

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Errors of the projector. ErrGap and ErrInconsistent both mean "rebuild":
// the events cannot be applied to the book the projector holds.
var (
	// ErrGap is a sequence the projector cannot continue from: events were
	// missed, or the buffered events do not join the snapshot.
	ErrGap = errors.New("marketdata: sequence gap")
	// ErrInconsistent is an event that contradicts the book (a trade against
	// an order the book does not hold, a remaining quantity that does not
	// match).
	ErrInconsistent = errors.New("marketdata: book inconsistent with events")
	// ErrBufferOverflow is a rebuild that took longer than the buffer could
	// hold events for; the buffer is dropped and the rebuild starts over.
	ErrBufferOverflow = errors.New("marketdata: rebuild buffer overflow")
)

// State of a projector.
type State int

// States.
const (
	// Cold ignores events: nobody has asked for a book yet.
	Cold State = iota
	// Rebuilding buffers events while a snapshot is being read.
	Rebuilding
	// Live applies events and emits deltas.
	Live
)

func (s State) String() string {
	switch s {
	case Cold:
		return "cold"
	case Rebuilding:
		return "rebuilding"
	case Live:
		return "live"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// DefaultRebuildBuffer bounds how many events wait while a snapshot is read.
const DefaultRebuildBuffer = 10_000

// BookProjector maintains the shadow book of one market from order.* and
// trade.* events and emits one Delta per engine command (docs/plan-v1.0.md
// §7.5).
//
// Events of one command share a seq and arrive contiguously (the relay
// publishes in outbox order, and one market commits one command at a time),
// but interleaved with other markets' and accounts' events. A command is
// closed -- its events applied to the book, its delta emitted -- only from
// the market's own events, never from a timer or a foreign event:
//
//   - order.rejected with a seq is the only event of its command: closed at
//     once, with an empty delta so clients see a contiguous seq;
//   - order.cancelled that opens a command is a cancel command: closed at
//     once;
//   - order.accepted opens a place command. The runner always emits the
//     taker's own terminal or resting event last (order.filled,
//     order.cancelled, or order.updated for a partial fill that rests), so
//     that event closes the command. A GTC limit that cannot cross the
//     opposite best price produces nothing after order.accepted (matching
//     never enters the loop), so it is closed at the accept itself. Market
//     and IOC orders always end in a terminal event.
//
// Whatever is still open when Flush is called is closed as a last resort;
// the feed does that from a timer so a defect in the rules above degrades
// to a late delta rather than a stuck book. An event for a command that was
// already closed is ErrInconsistent.
type BookProjector struct {
	market    string
	book      *Book
	state     State
	buffer    []eventbus.Envelope
	maxBuffer int
	open      *batch
	log       *slog.Logger
}

// batch is one command being collected.
type batch struct {
	seq        uint64
	at         time.Time
	takerID    string
	awaitTaker bool
	events     []eventbus.Envelope
	openedAt   time.Time
}

// NewBookProjector returns a Cold projector for market.
func NewBookProjector(market string, log *slog.Logger) *BookProjector {
	if log == nil {
		log = slog.Default()
	}
	return &BookProjector{
		market: market, book: NewBook(market), state: Cold, maxBuffer: DefaultRebuildBuffer,
		log: log.With(slog.String("market", market)),
	}
}

// WithRebuildBuffer bounds the events buffered during a rebuild.
func (p *BookProjector) WithRebuildBuffer(n int) *BookProjector {
	if n > 0 {
		p.maxBuffer = n
	}
	return p
}

// Market is the symbol.
func (p *BookProjector) Market() string { return p.market }

// State reports the projector's state.
func (p *BookProjector) State() State { return p.state }

// Seq is the last command applied to the book.
func (p *BookProjector) Seq() uint64 { return p.book.seq }

// Depth is the book at its last closed command: events of an open command
// are held until it closes, so a snapshot and the next delta always agree.
func (p *BookProjector) Depth(n int) Depth { return p.book.Depth(n) }

// OpenSince reports when the currently open command was opened, if any.
func (p *BookProjector) OpenSince() (time.Time, bool) {
	if p.open == nil {
		return time.Time{}, false
	}
	return p.open.openedAt, true
}

// MarkRebuilding empties the book and starts buffering events until Restore.
// Call it before reading the snapshot, so nothing committed between the two
// is lost.
func (p *BookProjector) MarkRebuilding() {
	p.book = NewBook(p.market)
	p.open = nil
	p.buffer = p.buffer[:0]
	p.state = Rebuilding
}

// Restore loads the snapshot and replays what was buffered meanwhile: events
// at or before the snapshot's seq are dropped, the rest applied in order.
// ErrGap or ErrInconsistent leave the projector Rebuilding with an empty
// buffer; call MarkRebuilding and read a fresh snapshot.
func (p *BookProjector) Restore(snap BookSnapshot) ([]Delta, error) {
	if p.state != Rebuilding {
		return nil, fmt.Errorf("%w: restore while %s", ErrInconsistent, p.state)
	}
	book := NewBook(p.market)
	if err := book.Restore(snap); err != nil {
		p.buffer = p.buffer[:0]
		return nil, err
	}
	p.book = book
	p.state = Live
	buffered := p.buffer
	p.buffer = nil
	var out []Delta
	for _, e := range buffered {
		deltas, err := p.Apply(e)
		if err != nil {
			p.state = Rebuilding
			p.open = nil
			return out, err
		}
		out = append(out, deltas...)
	}
	return out, nil
}

// Apply feeds one envelope. It returns the deltas of the commands it closed
// (none, one, or two when a lagging command had to be closed first). Events
// without a seq, of other markets, or already applied are ignored.
func (p *BookProjector) Apply(e eventbus.Envelope) ([]Delta, error) {
	if e.Seq == nil || e.MarketID == nil || *e.MarketID != p.market {
		return nil, nil
	}
	switch p.state {
	case Cold:
		return nil, nil
	case Rebuilding:
		if len(p.buffer) >= p.maxBuffer {
			p.buffer = p.buffer[:0]
			return nil, ErrBufferOverflow
		}
		p.buffer = append(p.buffer, e)
		return nil, nil
	}
	seq := *e.Seq
	if seq <= p.book.seq {
		return nil, nil // duplicate or already applied
	}
	var out []Delta
	if p.open != nil && seq != p.open.seq {
		if seq < p.open.seq {
			return nil, nil
		}
		// A later command arrived while one was open: the rules below
		// missed the open one's last event. Close it late rather than
		// never, and say so.
		p.log.Warn("closing an open command on a later seq", slog.Uint64("open_seq", p.open.seq), slog.Uint64("seq", seq))
		d, err := p.close()
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if p.open == nil && seq != p.book.seq+1 {
		return out, fmt.Errorf("%w: have %d, got %d", ErrGap, p.book.seq, seq)
	}
	deltas, err := p.dispatch(e, seq)
	if err != nil {
		return out, err
	}
	return append(out, deltas...), nil
}

func (p *BookProjector) dispatch(e eventbus.Envelope, seq uint64) ([]Delta, error) {
	switch e.EventType {
	case EventOrderAccepted:
		if p.open != nil {
			return nil, fmt.Errorf("%w: order.accepted inside command %d", ErrInconsistent, seq)
		}
		acc, err := decode[orderAccepted](e)
		if err != nil {
			return nil, err
		}
		p.open = &batch{seq: seq, at: e.OccurredAt, takerID: acc.OrderID, events: []eventbus.Envelope{e}, openedAt: time.Now()}
		p.open.awaitTaker = p.expectsTakerEvent(acc)
		if p.open.awaitTaker {
			return nil, nil
		}
		return p.closeOne()
	case EventOrderRejected:
		if p.open != nil {
			return nil, fmt.Errorf("%w: order.rejected inside command %d", ErrInconsistent, seq)
		}
		p.open = &batch{seq: seq, at: e.OccurredAt, openedAt: time.Now()}
		return p.closeOne()
	case EventOrderCancelled:
		prog, err := decode[orderProgress](e)
		if err != nil {
			return nil, err
		}
		if p.open == nil {
			// a cancel command: one event, one delta
			p.open = &batch{seq: seq, at: e.OccurredAt, events: []eventbus.Envelope{e}, openedAt: time.Now()}
			return p.closeOne()
		}
		p.open.events = append(p.open.events, e)
		if prog.OrderID == p.open.takerID {
			return p.closeOne()
		}
		return nil, nil
	case EventTradeExecuted:
		if p.open == nil {
			return nil, fmt.Errorf("%w: trade.executed outside a command (seq %d)", ErrInconsistent, seq)
		}
		p.open.events = append(p.open.events, e)
		return nil, nil
	case EventOrderUpdated, EventOrderFilled:
		if p.open == nil {
			return nil, fmt.Errorf("%w: %s outside a command (seq %d)", ErrInconsistent, e.EventType, seq)
		}
		prog, err := decode[orderProgress](e)
		if err != nil {
			return nil, err
		}
		p.open.events = append(p.open.events, e)
		if prog.OrderID == p.open.takerID {
			return p.closeOne()
		}
		return nil, nil
	}
	return nil, nil
}

// expectsTakerEvent decides whether the runner will emit anything after
// order.accepted for this order (see the type comment).
func (p *BookProjector) expectsTakerEvent(acc orderAccepted) bool {
	if acc.Type != "limit" || acc.TimeInForce != "gtc" || acc.Price == nil {
		return true
	}
	best, ok := p.book.Best(acc.Side.Opposite())
	if !ok {
		return false
	}
	return crosses(acc.Side, *acc.Price, best)
}

// Flush closes the open command, if any. The feed calls it from a timer as
// the last resort described on the type.
func (p *BookProjector) Flush() (*Delta, error) {
	if p.open == nil || p.state != Live {
		return nil, nil //nolint:nilnil // nothing to flush is not an error
	}
	p.log.Warn("closing an open command from the flush timer", slog.Uint64("seq", p.open.seq), slog.Bool("await_taker", p.open.awaitTaker))
	d, err := p.close()
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (p *BookProjector) closeOne() ([]Delta, error) {
	d, err := p.close()
	if err != nil {
		return nil, err
	}
	return []Delta{d}, nil
}

// close applies the open command to the book and returns its delta. On an
// inconsistency the book is left as it is and the caller rebuilds.
func (p *BookProjector) close() (Delta, error) {
	b := p.open
	p.open = nil
	before := map[string]levelState{}
	var touched []touchedLevel
	touch := func(side Side, price money.Amount) {
		key := string(side) + "|" + price.String()
		if _, seen := before[key]; seen {
			return
		}
		before[key] = p.book.levelAt(side, price)
		touched = append(touched, touchedLevel{key: key, side: side, price: price})
	}
	for _, e := range b.events {
		if err := p.applyEvent(b, e, touch); err != nil {
			return Delta{}, err
		}
	}
	d := Delta{Market: p.market, Seq: b.seq, At: b.at, Bids: []PriceLevel{}, Asks: []PriceLevel{}}
	for _, t := range touched {
		now := p.book.levelAt(t.side, t.price)
		if now.qty.Equal(before[t.key].qty) {
			continue
		}
		lvl := PriceLevel{Price: t.price, Qty: now.qty}
		if t.side == Buy {
			d.Bids = append(d.Bids, lvl)
		} else {
			d.Asks = append(d.Asks, lvl)
		}
	}
	sort.Slice(d.Bids, func(i, j int) bool { return d.Bids[i].Price.Cmp(d.Bids[j].Price) > 0 })
	sort.Slice(d.Asks, func(i, j int) bool { return d.Asks[i].Price.Cmp(d.Asks[j].Price) < 0 })
	p.book.seq = b.seq
	return d, nil
}

type touchedLevel struct {
	key   string
	side  Side
	price money.Amount
}

func (p *BookProjector) applyEvent(b *batch, e eventbus.Envelope, touch func(Side, money.Amount)) error {
	switch e.EventType {
	case EventOrderAccepted:
		acc, err := decode[orderAccepted](e)
		if err != nil {
			return err
		}
		if acc.Type != "limit" || acc.Price == nil || acc.Qty == nil {
			return nil // market orders never rest
		}
		touch(acc.Side, *acc.Price)
		return p.book.add(RestingOrder{ID: acc.OrderID, Side: acc.Side, Price: *acc.Price, Remaining: *acc.Qty})
	case EventTradeExecuted:
		tr, err := decode[tradeExecuted](e)
		if err != nil {
			return err
		}
		maker, ok := p.book.orders[tr.MakerOrderID]
		if !ok {
			return fmt.Errorf("%w: trade %s against unknown maker %s", ErrInconsistent, tr.TradeID, tr.MakerOrderID)
		}
		touch(maker.side, maker.price)
		if err := p.book.reduce(tr.MakerOrderID, tr.Qty); err != nil {
			return err
		}
		if taker, ok := p.book.orders[tr.TakerOrderID]; ok {
			touch(taker.side, taker.price)
			if err := p.book.reduce(tr.TakerOrderID, tr.Qty); err != nil {
				return err
			}
		}
		return nil
	case EventOrderUpdated:
		prog, err := decode[orderProgress](e)
		if err != nil {
			return err
		}
		o, ok := p.book.orders[prog.OrderID]
		if !ok {
			return fmt.Errorf("%w: order.updated for %s, not in the book", ErrInconsistent, prog.OrderID)
		}
		if !o.remaining.Equal(prog.RemainingQty) {
			return fmt.Errorf("%w: order %s remaining %s, event says %s", ErrInconsistent, prog.OrderID, o.remaining, prog.RemainingQty)
		}
		return nil
	case EventOrderFilled:
		prog, err := decode[orderProgress](e)
		if err != nil {
			return err
		}
		if _, ok := p.book.orders[prog.OrderID]; ok {
			return fmt.Errorf("%w: order.filled for %s, still in the book", ErrInconsistent, prog.OrderID)
		}
		return nil
	case EventOrderCancelled:
		prog, err := decode[orderProgress](e)
		if err != nil {
			return err
		}
		o, ok := p.book.orders[prog.OrderID]
		if !ok {
			if prog.OrderID == b.takerID {
				return nil // a market order's remainder: it never rested
			}
			return fmt.Errorf("%w: order.cancelled for %s, not in the book", ErrInconsistent, prog.OrderID)
		}
		if !o.remaining.Equal(prog.RemainingQty) {
			return fmt.Errorf("%w: order %s remaining %s, cancel says %s", ErrInconsistent, prog.OrderID, o.remaining, prog.RemainingQty)
		}
		touch(o.side, o.price)
		return p.book.remove(prog.OrderID)
	}
	return nil
}

// ApplyDelta applies a delta to a depth map (price string -> qty), the way a
// client maintains its copy. It is used by tests and by exchangectl.
func ApplyDelta(bids, asks map[string]money.Amount, d Delta) {
	apply := func(m map[string]money.Amount, levels []PriceLevel) {
		for _, l := range levels {
			if l.Qty.IsZero() {
				delete(m, l.Price.String())
			} else {
				m[l.Price.String()] = l.Qty
			}
		}
	}
	apply(bids, d.Bids)
	apply(asks, d.Asks)
}

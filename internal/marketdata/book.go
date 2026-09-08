package marketdata

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Level is one aggregated price level, the REST depth shape.
type Level struct {
	Price  money.Amount `json:"price"`
	Qty    money.Amount `json:"qty"`
	Orders int          `json:"orders"`
}

// PriceLevel is a level on the WebSocket: ["price", "qty"] (docs/plan-v1.0.md
// §7.5). A qty of "0" in a delta removes the level.
type PriceLevel struct {
	Price money.Amount
	Qty   money.Amount
}

// MarshalJSON writes the two-element array form.
func (l PriceLevel) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]string{l.Price.String(), l.Qty.String()})
}

// UnmarshalJSON reads the two-element array form.
func (l *PriceLevel) UnmarshalJSON(b []byte) error {
	var pair [2]string
	if err := json.Unmarshal(b, &pair); err != nil {
		return err
	}
	p, err := money.ParseAmount(pair[0])
	if err != nil {
		return fmt.Errorf("price: %w", err)
	}
	q, err := money.ParseAmount(pair[1])
	if err != nil {
		return fmt.Errorf("qty: %w", err)
	}
	l.Price, l.Qty = p, q
	return nil
}

// Depth is the aggregated book at one sequence.
type Depth struct {
	Market string  `json:"market"`
	Seq    uint64  `json:"seq"`
	Bids   []Level `json:"bids"`
	Asks   []Level `json:"asks"`
}

// Delta is what one engine command changed: every touched level with its
// new total, best price first. Applying the deltas after a snapshot in seq
// order reproduces the book.
type Delta struct {
	Market string       `json:"market"`
	Seq    uint64       `json:"seq"`
	At     time.Time    `json:"at"`
	Bids   []PriceLevel `json:"bids"`
	Asks   []PriceLevel `json:"asks"`
}

// RestingOrder is what the shadow book keeps per open order: enough to move
// the right level when the order trades or leaves.
type RestingOrder struct {
	ID        string
	Side      Side
	Price     money.Amount
	Remaining money.Amount
}

// BookSnapshot is a consistent read of one market's open orders and the
// sequence they are current at (Store.OpenOrders).
type BookSnapshot struct {
	Market string
	Seq    uint64
	Orders []RestingOrder
}

// Book is the shadow order book: price levels plus the orders behind them.
// Not safe for concurrent use; the projector serialises access.
type Book struct {
	market string
	seq    uint64
	orders map[string]*bookOrder
	bids   *bookSide
	asks   *bookSide
}

type bookOrder struct {
	id        string
	side      Side
	price     money.Amount
	remaining money.Amount
	level     *bookLevel
}

type bookLevel struct {
	price  money.Amount
	qty    money.Amount
	orders int
}

type bookSide struct {
	side   Side
	levels map[string]*bookLevel
	prices []money.Amount // best first: bids descending, asks ascending
}

// NewBook returns an empty book at seq 0.
func NewBook(market string) *Book {
	return &Book{
		market: market,
		orders: map[string]*bookOrder{},
		bids:   &bookSide{side: Buy, levels: map[string]*bookLevel{}},
		asks:   &bookSide{side: Sell, levels: map[string]*bookLevel{}},
	}
}

// Restore loads a snapshot into an empty book.
func (b *Book) Restore(snap BookSnapshot) error {
	if len(b.orders) != 0 || b.seq != 0 {
		return fmt.Errorf("%w: restore into a non-empty book", ErrInconsistent)
	}
	for _, o := range snap.Orders {
		if err := b.add(o); err != nil {
			return err
		}
	}
	b.seq = snap.Seq
	return nil
}

// Market is the symbol.
func (b *Book) Market() string { return b.market }

// Seq is the last command applied.
func (b *Book) Seq() uint64 { return b.seq }

// Len is the number of resting orders.
func (b *Book) Len() int { return len(b.orders) }

// Best returns the best price of a side, if any.
func (b *Book) Best(s Side) (money.Amount, bool) {
	side := b.sideOf(s)
	if len(side.prices) == 0 {
		return money.Zero, false
	}
	return side.prices[0], true
}

// Depth returns up to n levels per side (n <= 0: all).
func (b *Book) Depth(n int) Depth {
	return Depth{Market: b.market, Seq: b.seq, Bids: b.bids.depth(n), Asks: b.asks.depth(n)}
}

func (b *Book) sideOf(s Side) *bookSide {
	if s == Buy {
		return b.bids
	}
	return b.asks
}

func (b *Book) add(o RestingOrder) error {
	switch {
	case o.ID == "":
		return fmt.Errorf("%w: order without id", ErrInconsistent)
	case !o.Side.valid():
		return fmt.Errorf("%w: order %s side %q", ErrInconsistent, o.ID, o.Side)
	case !o.Price.IsPositive() || !o.Remaining.IsPositive():
		return fmt.Errorf("%w: order %s price %s remaining %s", ErrInconsistent, o.ID, o.Price, o.Remaining)
	}
	if _, dup := b.orders[o.ID]; dup {
		return fmt.Errorf("%w: order %s already in the book", ErrInconsistent, o.ID)
	}
	bo := &bookOrder{id: o.ID, side: o.Side, price: o.Price, remaining: o.Remaining}
	b.sideOf(o.Side).insert(bo)
	b.orders[o.ID] = bo
	return nil
}

// reduce takes qty off an order; at zero the order leaves the book.
func (b *Book) reduce(id string, qty money.Amount) error {
	o, ok := b.orders[id]
	if !ok {
		return fmt.Errorf("%w: order %s is not in the book", ErrInconsistent, id)
	}
	if !qty.IsPositive() || qty.Cmp(o.remaining) > 0 {
		return fmt.Errorf("%w: order %s reduce %s of %s", ErrInconsistent, id, qty, o.remaining)
	}
	o.remaining = o.remaining.Sub(qty)
	o.level.qty = o.level.qty.Sub(qty)
	if o.remaining.IsZero() {
		b.sideOf(o.side).drop(o)
		delete(b.orders, id)
	}
	return nil
}

func (b *Book) remove(id string) error {
	o, ok := b.orders[id]
	if !ok {
		return fmt.Errorf("%w: order %s is not in the book", ErrInconsistent, id)
	}
	o.level.qty = o.level.qty.Sub(o.remaining)
	b.sideOf(o.side).drop(o)
	delete(b.orders, id)
	return nil
}

// levelState is a level's totals; zero when the level is absent.
type levelState struct {
	qty    money.Amount
	orders int
}

func (b *Book) levelAt(s Side, price money.Amount) levelState {
	lvl, ok := b.sideOf(s).levels[price.String()]
	if !ok {
		return levelState{qty: money.Zero}
	}
	return levelState{qty: lvl.qty, orders: lvl.orders}
}

// --- bookSide ---

func (s *bookSide) better(a, b money.Amount) bool {
	if s.side == Buy {
		return a.Cmp(b) > 0
	}
	return a.Cmp(b) < 0
}

func (s *bookSide) insert(o *bookOrder) {
	key := o.price.String()
	lvl, ok := s.levels[key]
	if !ok {
		lvl = &bookLevel{price: o.price, qty: money.Zero}
		s.levels[key] = lvl
		i := sort.Search(len(s.prices), func(i int) bool { return s.better(o.price, s.prices[i]) })
		s.prices = append(s.prices, money.Zero)
		copy(s.prices[i+1:], s.prices[i:])
		s.prices[i] = o.price
	}
	lvl.qty = lvl.qty.Add(o.remaining)
	lvl.orders++
	o.level = lvl
}

// drop removes an order whose quantity has already been taken off its level.
func (s *bookSide) drop(o *bookOrder) {
	lvl := o.level
	lvl.orders--
	o.level = nil
	if lvl.orders > 0 {
		return
	}
	delete(s.levels, lvl.price.String())
	i := sort.Search(len(s.prices), func(i int) bool { return !s.better(s.prices[i], lvl.price) })
	if i < len(s.prices) && s.prices[i].Equal(lvl.price) {
		copy(s.prices[i:], s.prices[i+1:])
		s.prices[len(s.prices)-1] = money.Zero
		s.prices = s.prices[:len(s.prices)-1]
	}
}

func (s *bookSide) depth(n int) []Level {
	if n <= 0 || n > len(s.prices) {
		n = len(s.prices)
	}
	out := make([]Level, 0, n)
	for _, p := range s.prices[:n] {
		lvl := s.levels[p.String()]
		out = append(out, Level{Price: lvl.price, Qty: lvl.qty, Orders: lvl.orders})
	}
	return out
}

// crosses reports whether a limit order at limit would trade against the
// opposite best price. Equal prices trade, as in matching.crosses.
func crosses(side Side, limit, best money.Amount) bool {
	if side == Buy {
		return best.Cmp(limit) <= 0
	}
	return best.Cmp(limit) >= 0
}

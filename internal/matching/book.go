package matching

import (
	"fmt"
	"sort"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Book is one market's order book. It is not safe for concurrent use: the
// engine owns exactly one goroutine per market (docs/plan-v1.0.md §5.2).
type Book struct {
	cfg       MarketConfig
	tickScale int32
	bids      *bookSide
	asks      *bookSide
	byID      map[OrderID]*order
	lastSeq   uint64
}

type order struct {
	RestingOrder
	level *level
}

type level struct {
	price  money.Amount
	orders []*order // FIFO: ascending seq
	qty    money.Amount
}

type bookSide struct {
	side      Side
	tickScale int32
	levels    map[string]*level
	prices    []money.Amount // best first: bids descending, asks ascending
	count     int
}

// New creates an empty book for the market.
func New(cfg MarketConfig) (*Book, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ts := cfg.PriceTick.Scale()
	return &Book{
		cfg:       cfg,
		tickScale: ts,
		bids:      &bookSide{side: Buy, tickScale: ts, levels: map[string]*level{}},
		asks:      &bookSide{side: Sell, tickScale: ts, levels: map[string]*level{}},
		byID:      map[OrderID]*order{},
	}, nil
}

// Config returns the market configuration.
func (b *Book) Config() MarketConfig { return b.cfg }

// LastSeq is the seq of the last applied (or restored) command.
func (b *Book) LastSeq() uint64 { return b.lastSeq }

// Len is the number of resting orders.
func (b *Book) Len() int { return len(b.byID) }

// Order returns a resting order by id.
func (b *Book) Order(id OrderID) (RestingOrder, bool) {
	o, ok := b.byID[id]
	if !ok {
		return RestingOrder{}, false
	}
	return o.RestingOrder, true
}

// BestBid returns the highest bid price, if any.
func (b *Book) BestBid() (money.Amount, bool) { return b.bids.bestPrice() }

// BestAsk returns the lowest ask price, if any.
func (b *Book) BestAsk() (money.Amount, bool) { return b.asks.bestPrice() }

// Restore loads resting orders into an empty book (engine start-up:
// docs/plan-v1.0.md §5.1 engine role, ADR-0002). lastSeq is the last seq
// the engine processed; the book's LastSeq becomes max(lastSeq, orders'
// seq). Orders are inserted by (price, seq), whatever the input order.
func (b *Book) Restore(lastSeq uint64, orders []RestingOrder) error {
	if len(b.byID) != 0 || b.lastSeq != 0 {
		return fmt.Errorf("%w: book is not empty", ErrInvalidResting)
	}
	sorted := make([]RestingOrder, len(orders))
	copy(sorted, orders)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	b.lastSeq = lastSeq
	for _, r := range sorted {
		if err := b.validateResting(r); err != nil {
			return err
		}
		if _, dup := b.byID[r.OrderID]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateOrderID, r.OrderID)
		}
		o := &order{RestingOrder: r}
		b.sideOf(r.Side).insert(o)
		b.byID[r.OrderID] = o
		if r.Seq > b.lastSeq {
			b.lastSeq = r.Seq
		}
	}
	return nil
}

func (b *Book) validateResting(r RestingOrder) error {
	switch {
	case r.OrderID == "" || r.AccountID == "":
		return fmt.Errorf("%w: missing ids", ErrInvalidResting)
	case r.Side != Buy && r.Side != Sell:
		return fmt.Errorf("%w: %s side", ErrInvalidResting, r.OrderID)
	case !r.Price.IsPositive() || !r.Price.IsMultipleOf(b.cfg.PriceTick):
		return fmt.Errorf("%w: %s price %s", ErrInvalidResting, r.OrderID, r.Price)
	case !r.Remaining.IsPositive() || !r.Remaining.IsMultipleOf(b.cfg.QtyStep):
		return fmt.Errorf("%w: %s remaining %s", ErrInvalidResting, r.OrderID, r.Remaining)
	case !r.Qty.Equal(r.Remaining.Add(r.FilledQty)):
		return fmt.Errorf("%w: %s qty != remaining + filled", ErrInvalidResting, r.OrderID)
	case r.FilledQty.IsNegative() || r.FilledQuote.IsNegative():
		return fmt.Errorf("%w: %s negative filled", ErrInvalidResting, r.OrderID)
	}
	return nil
}

// Snapshot returns every resting order, best price first, FIFO per level.
func (b *Book) Snapshot() Snapshot {
	return Snapshot{Symbol: b.cfg.Symbol, LastSeq: b.lastSeq, Bids: b.bids.orders(), Asks: b.asks.orders()}
}

// Depth returns up to n aggregated levels per side (n <= 0: all).
func (b *Book) Depth(n int) Depth {
	return Depth{Symbol: b.cfg.Symbol, LastSeq: b.lastSeq, Bids: b.bids.depth(n), Asks: b.asks.depth(n)}
}

func (b *Book) sideOf(s Side) *bookSide {
	if s == Buy {
		return b.bids
	}
	return b.asks
}

// remove deletes a resting order from its level and the index.
func (b *Book) remove(o *order) {
	b.sideOf(o.Side).remove(o)
	delete(b.byID, o.OrderID)
}

// --- bookSide ---

func (s *bookSide) key(p money.Amount) string { return p.StringFixed(s.tickScale) }

// better reports whether price a is more aggressive than b on this side.
func (s *bookSide) better(a, b money.Amount) bool {
	if s.side == Buy {
		return a.Cmp(b) > 0
	}
	return a.Cmp(b) < 0
}

func (s *bookSide) best() *level {
	if len(s.prices) == 0 {
		return nil
	}
	return s.levels[s.key(s.prices[0])]
}

func (s *bookSide) bestPrice() (money.Amount, bool) {
	if len(s.prices) == 0 {
		return money.Zero, false
	}
	return s.prices[0], true
}

func (s *bookSide) insert(o *order) {
	k := s.key(o.Price)
	lvl, ok := s.levels[k]
	if !ok {
		lvl = &level{price: o.Price, qty: money.Zero}
		s.levels[k] = lvl
		// first index whose price is worse than o.Price
		i := sort.Search(len(s.prices), func(i int) bool { return s.better(o.Price, s.prices[i]) })
		s.prices = append(s.prices, money.Zero)
		copy(s.prices[i+1:], s.prices[i:])
		s.prices[i] = o.Price
	}
	// FIFO by seq: Apply always appends; Restore feeds seq-sorted input.
	lvl.orders = append(lvl.orders, o)
	lvl.qty = lvl.qty.Add(o.Remaining)
	o.level = lvl
	s.count++
}

func (s *bookSide) remove(o *order) {
	lvl := o.level
	for i, x := range lvl.orders {
		if x == o {
			copy(lvl.orders[i:], lvl.orders[i+1:])
			lvl.orders[len(lvl.orders)-1] = nil
			lvl.orders = lvl.orders[:len(lvl.orders)-1]
			break
		}
	}
	lvl.qty = lvl.qty.Sub(o.Remaining)
	o.level = nil
	s.count--
	if len(lvl.orders) == 0 {
		s.dropLevel(lvl)
	}
}

// popHead removes the first order of a level after it filled completely.
func (s *bookSide) popHead(lvl *level) {
	o := lvl.orders[0]
	lvl.orders[0] = nil
	lvl.orders = lvl.orders[1:]
	o.level = nil
	s.count--
	if len(lvl.orders) == 0 {
		s.dropLevel(lvl)
	}
}

func (s *bookSide) dropLevel(lvl *level) {
	delete(s.levels, s.key(lvl.price))
	i := sort.Search(len(s.prices), func(i int) bool { return !s.better(s.prices[i], lvl.price) })
	if i < len(s.prices) && s.prices[i].Equal(lvl.price) {
		copy(s.prices[i:], s.prices[i+1:])
		s.prices[len(s.prices)-1] = money.Zero
		s.prices = s.prices[:len(s.prices)-1]
	}
}

func (s *bookSide) orders() []RestingOrder {
	out := make([]RestingOrder, 0, s.count)
	for _, p := range s.prices {
		for _, o := range s.levels[s.key(p)].orders {
			out = append(out, o.RestingOrder)
		}
	}
	return out
}

func (s *bookSide) depth(n int) []Level {
	if n <= 0 || n > len(s.prices) {
		n = len(s.prices)
	}
	out := make([]Level, 0, n)
	for _, p := range s.prices[:n] {
		lvl := s.levels[s.key(p)]
		out = append(out, Level{Price: lvl.price, Qty: lvl.qty, Orders: len(lvl.orders)})
	}
	return out
}

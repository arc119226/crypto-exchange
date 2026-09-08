package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// Store is what the feed reads from Postgres: *marketdata.Store.
type Store interface {
	OpenOrders(ctx context.Context, marketID, symbol string) (marketdata.BookSnapshot, error)
	RingSeed(ctx context.Context, marketID, symbol string, now time.Time) (marketdata.RingSeed, error)
}

// MarketSource lists the markets to follow: *registry.Cache.
type MarketSource interface {
	Markets() []registry.Market
}

// SnapshotWriter is where depth snapshots go for the api role:
// *marketdata.SnapshotCache. nil disables the cache.
type SnapshotWriter interface {
	PutDepth(ctx context.Context, d marketdata.Depth, at time.Time) error
}

// ErrUnknownMarket is a subscription to a market the feed does not follow.
var ErrUnknownMarket = errors.New("stream: unknown market")

// Feed owns one shadow book and one candle ring per market and turns the
// event stream into channel messages. Every book mutation and every public
// publish happens under one mutex, so deltas leave in seq order and a
// snapshot handed to a new subscriber is followed by exactly the deltas
// after it.
type Feed struct {
	cfg     Config
	store   Store
	markets MarketSource
	hub     *Hub
	cache   SnapshotWriter
	metrics *Metrics
	md      *marketdata.Metrics
	log     *slog.Logger
	buffer  int

	mu     sync.Mutex
	states map[string]*marketState
	ctx    context.Context
	// private forwards account-scoped events; the server installs it
	private func(e eventbus.Envelope)
}

// marketState is one market's derived data, guarded by Feed.mu.
type marketState struct {
	id, symbol string
	proj       *marketdata.BookProjector
	rebuilding bool
	gen        int

	ring          *marketdata.CandleRing
	ringReady     bool
	ringSeq       int64
	pendingTrades []marketdata.TradeTick

	tickerDirty bool
	lastTicker  time.Time
	klineDirty  map[marketdata.Interval]bool
	lastKline   map[marketdata.Interval]time.Time
	lastAt      time.Time

	cacheSeq  uint64
	lastCache time.Time
}

// NewFeed builds a feed; Start begins following markets.
func NewFeed(cfg Config, store Store, markets MarketSource, hub *Hub, cache SnapshotWriter, m *Metrics, md *marketdata.Metrics, rebuildBuffer int, log *slog.Logger) *Feed {
	if log == nil {
		log = slog.Default()
	}
	return &Feed{
		cfg: cfg.withDefaults(), store: store, markets: markets, hub: hub, cache: cache, metrics: m, md: md, log: log,
		buffer: rebuildBuffer, states: map[string]*marketState{},
	}
}

// Start begins the rebuild of every known market. Call it after the event
// subscriptions exist, so nothing committed in between is missed.
func (f *Feed) Start(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctx = ctx
	f.syncMarketsLocked("start")
}

// syncMarketsLocked adds a state for every market the registry lists that
// the feed does not follow yet.
func (f *Feed) syncMarketsLocked(reason string) {
	if f.ctx == nil {
		return
	}
	for _, m := range f.markets.Markets() {
		if _, ok := f.states[m.Symbol]; ok {
			continue
		}
		st := &marketState{
			id: m.ID, symbol: m.Symbol,
			proj:       marketdata.NewBookProjector(m.Symbol, f.log).WithRebuildBuffer(f.buffer),
			ring:       marketdata.NewCandleRing(m.Symbol),
			klineDirty: map[marketdata.Interval]bool{}, lastKline: map[marketdata.Interval]time.Time{},
		}
		f.states[m.Symbol] = st
		f.startRebuildLocked(st, reason)
	}
}

// startRebuildLocked marks the book rebuilding and reads a fresh snapshot in
// the background. A rebuild already running for this state is superseded.
func (f *Feed) startRebuildLocked(st *marketState, reason string) {
	st.proj.MarkRebuilding()
	st.rebuilding = true
	st.gen++
	f.md.ObserveRebuild(st.symbol, reason)
	gen := st.gen
	go f.rebuild(st, gen)
}

func (f *Feed) rebuild(st *marketState, gen int) {
	backoff := 100 * time.Millisecond
	log := f.log.With(slog.String("market", st.symbol))
	for f.ctx.Err() == nil {
		snap, err := f.store.OpenOrders(f.ctx, st.id, st.symbol)
		var (
			seed     marketdata.RingSeed
			needSeed bool
		)
		if err == nil {
			f.mu.Lock()
			needSeed = !st.ringReady
			f.mu.Unlock()
			if needSeed {
				seed, err = f.store.RingSeed(f.ctx, st.id, st.symbol, time.Now().UTC())
			}
		}
		f.mu.Lock()
		if st.gen != gen {
			f.mu.Unlock()
			return // a newer rebuild took over
		}
		if err == nil {
			var deltas []marketdata.Delta
			deltas, err = st.proj.Restore(snap)
			if err != nil {
				st.proj.MarkRebuilding()
				f.md.ObserveRebuild(st.symbol, "gap")
			} else {
				st.rebuilding = false
				if needSeed {
					f.seedRingLocked(st, seed)
				}
				f.md.ObserveBookSeq(st.symbol, st.proj.Seq())
				f.publishSnapshotLocked(st)
				for _, d := range deltas {
					f.publishDeltaLocked(st, d)
				}
				log.Info("shadow book live", slog.Uint64("seq", st.proj.Seq()), slog.Int("orders", len(snap.Orders)), slog.Int("replayed", len(deltas)))
				f.mu.Unlock()
				return
			}
		}
		f.mu.Unlock()
		if f.ctx.Err() != nil {
			return
		}
		log.Warn("shadow book rebuild failed, retrying", slog.String("err", err.Error()), slog.Duration("in", backoff))
		select {
		case <-f.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (f *Feed) seedRingLocked(st *marketState, seed marketdata.RingSeed) {
	st.ring.Seed(seed.Candles)
	for _, t := range seed.Trades {
		st.ring.AddTrade(t)
	}
	st.ringSeq = seed.Seq
	st.ringReady = true
	for _, t := range st.pendingTrades {
		if int64(t.Seq) > st.ringSeq { //nolint:gosec // engine seq
			st.ring.AddTrade(t)
			st.markCandlesDirty(t.At)
		}
	}
	st.pendingTrades = nil
}

func (st *marketState) markCandlesDirty(at time.Time) {
	st.tickerDirty = true
	for _, iv := range marketdata.Intervals() {
		st.klineDirty[iv] = true
	}
	if at.After(st.lastAt) {
		st.lastAt = at
	}
}

// Handle feeds one event. Market events drive the book and the candles;
// account events go to the private channels.
func (f *Feed) Handle(_ context.Context, e eventbus.Envelope) {
	if e.MarketID != nil && *e.MarketID != "" {
		switch e.Domain() {
		case "order", "trade":
			f.handleMarket(e)
		}
	}
	if e.AccountID != nil && *e.AccountID != "" || e.EventType == marketdata.EventTradeExecuted {
		if f.private != nil {
			f.private(e)
		}
	}
}

func (f *Feed) handleMarket(e eventbus.Envelope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[*e.MarketID]
	if !ok {
		return // a market listed after start: picked up by the next sync
	}
	if e.EventType == marketdata.EventTradeExecuted {
		f.onTradeLocked(st, e)
	}
	deltas, err := st.proj.Apply(e)
	switch {
	case errors.Is(err, marketdata.ErrBufferOverflow):
		f.log.Warn("rebuild buffer overflowed, starting over", slog.String("market", st.symbol))
		f.startRebuildLocked(st, "overflow")
	case errors.Is(err, marketdata.ErrGap):
		f.log.Warn("sequence gap in the shadow book", slog.String("market", st.symbol), slog.String("err", err.Error()))
		f.startRebuildLocked(st, "gap")
	case errors.Is(err, marketdata.ErrInconsistent):
		f.log.Error("shadow book inconsistent with events", slog.String("market", st.symbol), slog.String("err", err.Error()))
		f.startRebuildLocked(st, "inconsistent")
	case err != nil:
		f.log.Error("shadow book event failed", slog.String("market", st.symbol), slog.String("err", err.Error()))
	default:
		for _, d := range deltas {
			f.publishDeltaLocked(st, d)
		}
	}
}

func (f *Feed) onTradeLocked(st *marketState, e eventbus.Envelope) {
	t, err := marketdata.TradeFromEnvelope(e)
	if err != nil {
		f.log.Error("bad trade event", slog.String("event_id", e.EventID), slog.String("err", err.Error()))
		return
	}
	switch {
	case !st.ringReady:
		st.pendingTrades = append(st.pendingTrades, t)
	case int64(t.Seq) > st.ringSeq: //nolint:gosec // engine seq
		st.ring.AddTrade(t)
		st.markCandlesDirty(t.At)
	}
	msg := tradeMessage{Channel: ChannelTrades, Type: "update", Market: st.symbol, Seq: t.Seq, At: t.At, Trade: publicTrade{
		TradeID: t.TradeID, Price: t.Price.String(), Qty: t.Qty.String(), QuoteQty: t.QuoteQty.String(), TakerSide: t.TakerSide, ExecutedAt: t.At,
	}}
	if n := f.hub.Publish(topic(ChannelTrades, st.symbol), ChannelTrades, mustJSON(msg)); n > 0 {
		f.metrics.delay(ChannelTrades, t.At)
	}
}

func (f *Feed) publishDeltaLocked(st *marketState, d marketdata.Delta) {
	f.md.ObserveBookSeq(st.symbol, d.Seq)
	if d.At.After(st.lastAt) {
		st.lastAt = d.At
	}
	msg := depthMessage{Channel: ChannelDepth, Type: "delta", Market: st.symbol, Seq: d.Seq, At: d.At, Bids: d.Bids, Asks: d.Asks}
	if n := f.hub.Publish(topic(ChannelDepth, st.symbol), ChannelDepth, mustJSON(msg)); n > 0 {
		f.metrics.delay(ChannelDepth, d.At)
	}
}

func (f *Feed) snapshotMessageLocked(st *marketState) []byte {
	d := st.proj.Depth(f.cfg.DepthLevels)
	msg := depthMessage{Channel: ChannelDepth, Type: "snapshot", Market: st.symbol, Seq: d.Seq, At: st.lastAt, Bids: tuples(d.Bids), Asks: tuples(d.Asks)}
	if msg.At.IsZero() {
		msg.At = time.Now().UTC()
	}
	return mustJSON(msg)
}

func (f *Feed) publishSnapshotLocked(st *marketState) {
	f.hub.Publish(topic(ChannelDepth, st.symbol), ChannelDepth, f.snapshotMessageLocked(st))
}

func tuples(levels []marketdata.Level) []marketdata.PriceLevel {
	out := make([]marketdata.PriceLevel, 0, len(levels))
	for _, l := range levels {
		out = append(out, marketdata.PriceLevel{Price: l.Price, Qty: l.Qty})
	}
	return out
}

func (f *Feed) tickerMessageLocked(st *marketState, now time.Time) []byte {
	return mustJSON(tickerMessage{Channel: ChannelTicker, Type: "update", Market: st.symbol, At: now, Ticker: st.ring.Ticker(now)})
}

func (f *Feed) klineMessageLocked(st *marketState, iv marketdata.Interval, now time.Time) ([]byte, bool) {
	c, ok := st.ring.Current(iv, st.lastAt)
	if !ok {
		return nil, false
	}
	return mustJSON(klineMessage{Channel: klineChannel(iv), Type: "update", Market: st.symbol, At: now, Candle: c}), true
}

// Subscribe registers c on a channel of a market and sends the first
// message a new subscriber needs: the depth snapshot, the current ticker or
// candle. Done under the feed lock so the snapshot is followed by exactly
// the deltas after it.
func (f *Feed) Subscribe(c *conn, ch channel, market string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[market]
	if !ok {
		return ErrUnknownMarket
	}
	if !f.hub.subscribe(c, topic(ch.name, market)) {
		return nil // already subscribed: nothing to repeat
	}
	now := time.Now().UTC()
	switch {
	case ch.name == ChannelDepth && !st.rebuilding:
		c.send(f.snapshotMessageLocked(st))
	case ch.name == ChannelTicker && st.ringReady:
		if tk := st.ring.Ticker(now); tk.Last != nil {
			c.send(mustJSON(tickerMessage{Channel: ChannelTicker, Type: "update", Market: st.symbol, At: now, Ticker: tk}))
		}
	case ch.interval != "" && st.ringReady:
		if b, ok := f.klineMessageLocked(st, ch.interval, now); ok {
			c.send(b)
		}
	}
	return nil
}

// Unsubscribe removes c from a channel of a market.
func (f *Feed) Unsubscribe(c *conn, ch channel, market string) bool {
	return f.hub.unsubscribe(c, topic(ch.name, market))
}

// Depth is a market's shadow book, for tests and the loadgen.
func (f *Feed) Depth(market string, n int) (marketdata.Depth, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.states[market]
	if !ok || st.rebuilding {
		return marketdata.Depth{}, false
	}
	return st.proj.Depth(n), true
}

// Ready reports nil once every followed market has a live book and a
// seeded ring.
func (f *Feed) Ready() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx == nil {
		return errors.New("stream: feed not started")
	}
	for _, st := range f.states {
		if st.rebuilding || !st.ringReady {
			return fmt.Errorf("stream: %s is still rebuilding", st.symbol)
		}
	}
	return nil
}

// Run drives the clocks: the projector flush, the coalesced ticker and
// candle pushes, the depth cache, market discovery and ring pruning.
func (f *Feed) Run(ctx context.Context) error {
	tick := time.NewTicker(f.cfg.FlushInterval)
	defer tick.Stop()
	lastSync, lastPrune := time.Now(), time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		now := time.Now().UTC()
		writes := f.tickLocked(now)
		for _, w := range writes {
			if err := f.cache.PutDepth(ctx, w.depth, w.at); err != nil {
				f.log.Warn("depth cache write failed", slog.String("market", w.depth.Market), slog.String("err", err.Error()))
			} else {
				f.md.ObserveSnapshotWrite()
			}
		}
		if now.Sub(lastSync) >= time.Second {
			lastSync = now
			f.mu.Lock()
			f.syncMarketsLocked("listed")
			f.mu.Unlock()
		}
		if now.Sub(lastPrune) >= time.Hour {
			lastPrune = now
			f.mu.Lock()
			for _, st := range f.states {
				st.ring.Prune(now)
			}
			f.mu.Unlock()
		}
	}
}

type cacheWrite struct {
	depth marketdata.Depth
	at    time.Time
}

// tickLocked runs one pass of the clocks and returns the cache writes to
// perform outside the lock.
func (f *Feed) tickLocked(now time.Time) []cacheWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	var writes []cacheWrite
	for _, st := range f.states {
		if st.rebuilding {
			continue
		}
		if since, open := st.proj.OpenSince(); open && now.Sub(since) >= f.cfg.FlushInterval {
			d, err := st.proj.Flush()
			switch {
			case err != nil:
				f.log.Error("shadow book flush failed", slog.String("market", st.symbol), slog.String("err", err.Error()))
				f.startRebuildLocked(st, "inconsistent")
				continue
			case d != nil:
				f.publishDeltaLocked(st, *d)
			}
		}
		if st.tickerDirty && now.Sub(st.lastTicker) >= f.cfg.TickerInterval {
			st.tickerDirty, st.lastTicker = false, now
			f.hub.Publish(topic(ChannelTicker, st.symbol), ChannelTicker, f.tickerMessageLocked(st, now))
		}
		for iv, dirty := range st.klineDirty {
			if !dirty || now.Sub(st.lastKline[iv]) < f.cfg.KlineInterval {
				continue
			}
			st.klineDirty[iv], st.lastKline[iv] = false, now
			if b, ok := f.klineMessageLocked(st, iv, now); ok {
				f.hub.Publish(topic(klineChannel(iv), st.symbol), klineChannel(iv), b)
			}
		}
		if f.cache != nil && st.proj.Seq() != st.cacheSeq && now.Sub(st.lastCache) >= f.cfg.SnapshotInterval {
			st.cacheSeq, st.lastCache = st.proj.Seq(), now
			writes = append(writes, cacheWrite{depth: st.proj.Depth(f.cfg.DepthLevels), at: now})
		}
	}
	return writes
}

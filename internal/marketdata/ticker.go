package marketdata

import (
	"sort"
	"time"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// TickerWindow is how far back the ticker looks.
const TickerWindow = 24 * time.Hour

// Ticker is the 24-hour summary of a market. The price fields are nil when
// the window holds no trade.
type Ticker struct {
	Market      string        `json:"market"`
	Last        *money.Amount `json:"last_price"`
	Open        *money.Amount `json:"open"`
	High        *money.Amount `json:"high"`
	Low         *money.Amount `json:"low"`
	Change      *money.Amount `json:"change"`
	ChangePct   *money.Amount `json:"change_pct"`
	Volume      money.Amount  `json:"volume"`
	QuoteVolume money.Amount  `json:"quote_volume"`
	Trades      int64         `json:"trades"`
	At          time.Time     `json:"at"`
}

// TickerFrom summarises the 1m candles whose bucket starts inside the 24 h
// before now. candles need not be sorted; synthesized (empty) candles are
// ignored.
func TickerFrom(market string, candles []Candle, now time.Time) Ticker {
	t := Ticker{Market: market, Volume: money.Zero, QuoteVolume: money.Zero, At: now.UTC()}
	since := now.Add(-TickerWindow)
	in := make([]Candle, 0, len(candles))
	for _, c := range candles {
		if c.Trades == 0 || c.Start.Before(since) || !c.Start.Before(now) {
			continue
		}
		in = append(in, c)
	}
	if len(in) == 0 {
		return t
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].Start.Before(in[j].Start) })
	open, last, high, low := in[0].Open, in[len(in)-1].Close, in[0].High, in[0].Low
	for _, c := range in {
		if c.High.Cmp(high) > 0 {
			high = c.High
		}
		if c.Low.Cmp(low) < 0 {
			low = c.Low
		}
		t.Volume = t.Volume.Add(c.Volume)
		t.QuoteVolume = t.QuoteVolume.Add(c.QuoteVolume)
		t.Trades += c.Trades
	}
	change := last.Sub(open)
	t.Open, t.Last, t.High, t.Low, t.Change = &open, &last, &high, &low, &change
	if open.IsPositive() {
		// truncated to two decimals, symmetric around zero
		pct, err := change.Abs().Mul(money.FromInt64(100)).DivRoundDown(open, 2)
		if err == nil {
			if change.IsNegative() {
				pct = pct.Neg()
			}
			t.ChangePct = &pct
		}
	}
	return t
}

// CandleRing keeps the last 24 h of 1m candles of one market and derives
// every wider interval from them, so the stream holds one structure per
// market rather than five.
type CandleRing struct {
	market  string
	candles []Candle // 1m, ascending by Start, at most one per bucket
}

// NewCandleRing starts empty.
func NewCandleRing(market string) *CandleRing { return &CandleRing{market: market} }

// Seed installs stored 1m candles (any order; empty ones are dropped).
func (r *CandleRing) Seed(candles []Candle) {
	for _, c := range candles {
		if c.Interval == Interval1m && c.Trades > 0 {
			r.put(c)
		}
	}
}

// AddTrade folds one trade into its 1m candle and returns the current
// candle of every interval, shortest first.
func (r *CandleRing) AddTrade(t TradeTick) []Candle {
	start := Interval1m.BucketStart(t.At)
	i := r.index(start)
	if i < 0 {
		c := NewCandle(r.market, Interval1m, t.At)
		c.Add(t)
		r.put(c)
	} else {
		r.candles[i].Add(t)
	}
	out := make([]Candle, 0, len(intervals))
	for _, iv := range intervals {
		if c, ok := r.Current(iv, t.At); ok {
			out = append(out, c)
		}
	}
	return out
}

// Current is the candle of iv containing at, rolled up from the ring.
func (r *CandleRing) Current(iv Interval, at time.Time) (Candle, bool) {
	start := iv.BucketStart(at)
	end := start.Add(iv.Duration())
	c := NewCandle(r.market, iv, at)
	for _, one := range r.candles {
		if one.Start.Before(start) || !one.Start.Before(end) {
			continue
		}
		c.Merge(one)
	}
	if c.Trades == 0 {
		return Candle{}, false
	}
	return c, true
}

// Prune drops candles older than the ticker window.
func (r *CandleRing) Prune(now time.Time) {
	since := Interval1m.BucketStart(now.Add(-TickerWindow))
	i := 0
	for i < len(r.candles) && r.candles[i].Start.Before(since) {
		i++
	}
	if i > 0 {
		r.candles = append([]Candle(nil), r.candles[i:]...)
	}
}

// Ticker summarises the ring.
func (r *CandleRing) Ticker(now time.Time) Ticker { return TickerFrom(r.market, r.candles, now) }

// Candles returns the 1m candles, ascending.
func (r *CandleRing) Candles() []Candle { return append([]Candle(nil), r.candles...) }

// LastClose is the most recent close in the ring, if any.
func (r *CandleRing) LastClose() (money.Amount, bool) {
	if len(r.candles) == 0 {
		return money.Zero, false
	}
	return r.candles[len(r.candles)-1].Close, true
}

func (r *CandleRing) index(start time.Time) int {
	i := sort.Search(len(r.candles), func(i int) bool { return !r.candles[i].Start.Before(start) })
	if i < len(r.candles) && r.candles[i].Start.Equal(start) {
		return i
	}
	return -1
}

func (r *CandleRing) put(c Candle) {
	i := sort.Search(len(r.candles), func(i int) bool { return !r.candles[i].Start.Before(c.Start) })
	if i < len(r.candles) && r.candles[i].Start.Equal(c.Start) {
		r.candles[i] = c
		return
	}
	r.candles = append(r.candles, Candle{})
	copy(r.candles[i+1:], r.candles[i:])
	r.candles[i] = c
}

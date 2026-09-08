package marketdata

import (
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Interval is a candle width.
type Interval string

// Intervals (docs/plan-v1.0.md §7.5 kline.{1m|5m|15m|1h|1d}).
const (
	Interval1m  Interval = "1m"
	Interval5m  Interval = "5m"
	Interval15m Interval = "15m"
	Interval1h  Interval = "1h"
	Interval1d  Interval = "1d"
)

var intervals = []Interval{Interval1m, Interval5m, Interval15m, Interval1h, Interval1d}

// Intervals lists every interval, shortest first.
func Intervals() []Interval { return append([]Interval(nil), intervals...) }

// ParseInterval accepts the five wire names.
func ParseInterval(s string) (Interval, error) {
	for _, iv := range intervals {
		if string(iv) == s {
			return iv, nil
		}
	}
	return "", fmt.Errorf("marketdata: unknown interval %q", s)
}

// Duration of one bucket.
func (iv Interval) Duration() time.Duration {
	switch iv {
	case Interval1m:
		return time.Minute
	case Interval5m:
		return 5 * time.Minute
	case Interval15m:
		return 15 * time.Minute
	case Interval1h:
		return time.Hour
	case Interval1d:
		return 24 * time.Hour
	}
	return 0
}

// BucketStart is the start of the bucket t falls in. Buckets are aligned to
// UTC: a day starts at 00:00 UTC (time.Truncate counts from the zero time,
// which is midnight UTC, so every width here aligns).
func (iv Interval) BucketStart(t time.Time) time.Time {
	return t.UTC().Truncate(iv.Duration())
}

// Candle is one OHLCV bucket. Trades == 0 marks a synthesized candle that
// carries the previous close through an empty bucket.
type Candle struct {
	Market      string       `json:"market"`
	Interval    Interval     `json:"interval"`
	Start       time.Time    `json:"start"`
	Open        money.Amount `json:"open"`
	High        money.Amount `json:"high"`
	Low         money.Amount `json:"low"`
	Close       money.Amount `json:"close"`
	Volume      money.Amount `json:"volume"`
	QuoteVolume money.Amount `json:"quote_volume"`
	Trades      int64        `json:"trades"`
}

// NewCandle starts a bucket for market/interval containing t.
func NewCandle(market string, iv Interval, t time.Time) Candle {
	return Candle{Market: market, Interval: iv, Start: iv.BucketStart(t), Volume: money.Zero, QuoteVolume: money.Zero}
}

// Add folds one trade in. The trade must fall in the candle's bucket; that
// is the caller's contract (Aggregator enforces it).
func (c *Candle) Add(t TradeTick) {
	if c.Trades == 0 {
		c.Open, c.High, c.Low = t.Price, t.Price, t.Price
	} else {
		if t.Price.Cmp(c.High) > 0 {
			c.High = t.Price
		}
		if t.Price.Cmp(c.Low) < 0 {
			c.Low = t.Price
		}
	}
	c.Close = t.Price
	c.Volume = c.Volume.Add(t.Qty)
	c.QuoteVolume = c.QuoteVolume.Add(t.QuoteQty)
	c.Trades++
}

// Merge folds another candle of the same market and bucket in (a database
// upsert in memory). o may come from a wider or equal interval alignment;
// the caller aligns the bucket.
func (c *Candle) Merge(o Candle) {
	if o.Trades == 0 {
		return
	}
	if c.Trades == 0 {
		c.Open, c.High, c.Low = o.Open, o.High, o.Low
	} else {
		if o.High.Cmp(c.High) > 0 {
			c.High = o.High
		}
		if o.Low.Cmp(c.Low) < 0 {
			c.Low = o.Low
		}
	}
	c.Close = o.Close
	c.Volume = c.Volume.Add(o.Volume)
	c.QuoteVolume = c.QuoteVolume.Add(o.QuoteVolume)
	c.Trades += o.Trades
}

// Flat returns an empty candle at price for the bucket containing t.
func Flat(market string, iv Interval, t time.Time, price money.Amount) Candle {
	c := NewCandle(market, iv, t)
	c.Open, c.High, c.Low, c.Close = price, price, price, price
	return c
}

// Aggregator folds trades of one market into the current candle of every
// interval. Apply returns the candles the trade changed (one per interval),
// which is what the stream pushes and the writer upserts.
type Aggregator struct {
	market  string
	current map[Interval]*Candle
}

// NewAggregator starts with no candles.
func NewAggregator(market string) *Aggregator {
	return &Aggregator{market: market, current: map[Interval]*Candle{}}
}

// Apply folds one trade into every interval and returns the updated candles
// in interval order. A trade before the current bucket of an interval is
// folded into a fresh candle for its own bucket, so out-of-order input is
// tolerated but its earlier bucket becomes the current one; feed trades in
// seq order.
func (a *Aggregator) Apply(t TradeTick) []Candle {
	out := make([]Candle, 0, len(intervals))
	for _, iv := range intervals {
		start := iv.BucketStart(t.At)
		c, ok := a.current[iv]
		if !ok || !c.Start.Equal(start) {
			nc := NewCandle(a.market, iv, t.At)
			c = &nc
			a.current[iv] = c
		}
		c.Add(t)
		out = append(out, *c)
	}
	return out
}

// Seed installs a known current candle for an interval (a restart reading
// what the writer persisted).
func (a *Aggregator) Seed(c Candle) {
	cc := c
	a.current[c.Interval] = &cc
}

// Current returns the current candle of an interval, if any.
func (a *Aggregator) Current(iv Interval) (Candle, bool) {
	c, ok := a.current[iv]
	if !ok {
		return Candle{}, false
	}
	return *c, true
}

// FillGaps returns one candle per bucket in [from, to): the stored candles
// where there are any, and flat candles carrying the previous close through
// empty buckets. Buckets before the first known close are skipped (there is
// no price to carry). candles must be ascending by Start and all of the
// same interval; prevClose is the close of the last candle before from,
// when the caller knows it.
func FillGaps(candles []Candle, market string, iv Interval, from, to time.Time, prevClose *money.Amount) []Candle {
	d := iv.Duration()
	if d <= 0 || !to.After(from) {
		return nil
	}
	out := make([]Candle, 0, len(candles))
	last := prevClose
	i := 0
	for start := iv.BucketStart(from); start.Before(to); start = start.Add(d) {
		for i < len(candles) && candles[i].Start.Before(start) {
			// a candle before the range still sets the carried close
			if candles[i].Trades > 0 {
				cl := candles[i].Close
				last = &cl
			}
			i++
		}
		if i < len(candles) && candles[i].Start.Equal(start) {
			c := candles[i]
			out = append(out, c)
			cl := c.Close
			last = &cl
			i++
			continue
		}
		if last == nil {
			continue
		}
		out = append(out, Flat(market, iv, start, *last))
	}
	return out
}

// Rollup folds 1m candles into the candles of a wider interval, one per
// bucket, ascending. It is how the stream derives every other interval from
// the one ring it keeps.
func Rollup(ones []Candle, market string, iv Interval) []Candle {
	var out []Candle
	for _, c := range ones {
		if c.Trades == 0 {
			continue
		}
		start := iv.BucketStart(c.Start)
		if len(out) == 0 || !out[len(out)-1].Start.Equal(start) {
			out = append(out, NewCandle(market, iv, c.Start))
		}
		out[len(out)-1].Merge(c)
	}
	return out
}

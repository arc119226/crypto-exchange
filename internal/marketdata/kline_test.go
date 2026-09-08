package marketdata_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/money"
)

func tick(seq uint64, at time.Time, price, qty string) marketdata.TradeTick {
	p, q := amt(price), amt(qty)
	return marketdata.TradeTick{TradeID: "t", Market: market, Seq: seq, Price: p, Qty: q, QuoteQty: p.Mul(q), TakerSide: marketdata.Buy, At: at}
}

func TestIntervals(t *testing.T) {
	assert.Equal(t, []marketdata.Interval{"1m", "5m", "15m", "1h", "1d"}, marketdata.Intervals())
	for _, iv := range marketdata.Intervals() {
		got, err := marketdata.ParseInterval(string(iv))
		require.NoError(t, err)
		assert.Equal(t, iv, got)
		assert.Positive(t, iv.Duration())
	}
	_, err := marketdata.ParseInterval("2m")
	assert.Error(t, err)
	assert.Zero(t, marketdata.Interval("2m").Duration())

	// buckets align to UTC, days at midnight UTC, whatever the zone of the input
	at := time.Date(2026, 9, 5, 13, 47, 59, 0, time.FixedZone("tw", 8*3600))
	assert.Equal(t, "2026-09-05T05:47:00Z", marketdata.Interval1m.BucketStart(at).Format(time.RFC3339))
	assert.Equal(t, "2026-09-05T05:45:00Z", marketdata.Interval5m.BucketStart(at).Format(time.RFC3339))
	assert.Equal(t, "2026-09-05T05:45:00Z", marketdata.Interval15m.BucketStart(at).Format(time.RFC3339))
	assert.Equal(t, "2026-09-05T05:00:00Z", marketdata.Interval1h.BucketStart(at).Format(time.RFC3339))
	assert.Equal(t, "2026-09-05T00:00:00Z", marketdata.Interval1d.BucketStart(at).Format(time.RFC3339))
}

func TestCandleAddAndMerge(t *testing.T) {
	at := t0.Add(90 * time.Second)
	c := marketdata.NewCandle(market, marketdata.Interval1m, at)
	assert.Equal(t, t0.Add(time.Minute), c.Start)
	c.Add(tick(1, at, "2000", "0.5"))
	c.Add(tick(2, at, "2010", "0.25"))
	c.Add(tick(3, at, "1990", "1"))
	assert.Equal(t, "2000", c.Open.String())
	assert.Equal(t, "2010", c.High.String())
	assert.Equal(t, "1990", c.Low.String())
	assert.Equal(t, "1990", c.Close.String())
	assert.Equal(t, "1.75", c.Volume.String())
	assert.Equal(t, "3492.5", c.QuoteVolume.String(), "1000 + 502.5 + 1990")
	assert.EqualValues(t, 3, c.Trades)

	wide := marketdata.NewCandle(market, marketdata.Interval5m, at)
	wide.Merge(c)
	next := marketdata.NewCandle(market, marketdata.Interval1m, at.Add(time.Minute))
	next.Add(tick(4, at.Add(time.Minute), "2020", "1"))
	wide.Merge(next)
	wide.Merge(marketdata.NewCandle(market, marketdata.Interval1m, at)) // empty: no effect
	assert.Equal(t, "2000", wide.Open.String())
	assert.Equal(t, "2020", wide.High.String())
	assert.Equal(t, "2020", wide.Close.String())
	assert.Equal(t, "2.75", wide.Volume.String())
	assert.EqualValues(t, 4, wide.Trades)
}

func TestAggregatorBucketsEveryInterval(t *testing.T) {
	a := marketdata.NewAggregator(market)
	at := time.Date(2026, 9, 5, 23, 59, 30, 0, time.UTC)
	out := a.Apply(tick(1, at, "2000", "1"))
	require.Len(t, out, 5)
	for i, iv := range marketdata.Intervals() {
		assert.Equal(t, iv, out[i].Interval)
		assert.Equal(t, iv.BucketStart(at), out[i].Start)
		assert.EqualValues(t, 1, out[i].Trades)
	}
	// one minute later: a new day, so every interval rolls over
	out = a.Apply(tick(2, at.Add(time.Minute), "2100", "1"))
	for _, c := range out {
		assert.EqualValues(t, 1, c.Trades, "%s rolled over at midnight", c.Interval)
		assert.Equal(t, "2100", c.Open.String())
	}
	cur, ok := a.Current(marketdata.Interval1d)
	require.True(t, ok)
	assert.Equal(t, time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), cur.Start)
	_, ok = marketdata.NewAggregator(market).Current(marketdata.Interval1m)
	assert.False(t, ok)

	a.Seed(marketdata.Candle{Market: market, Interval: marketdata.Interval1h, Start: at.Add(time.Minute).Truncate(time.Hour), Open: amt("1"), High: amt("9"), Low: amt("1"), Close: amt("5"), Volume: amt("10"), QuoteVolume: amt("50"), Trades: 7})
	out = a.Apply(tick(3, at.Add(2*time.Minute), "2050", "1"))
	for _, c := range out {
		if c.Interval == marketdata.Interval1h {
			assert.EqualValues(t, 8, c.Trades, "a seeded candle continues")
			assert.Equal(t, "2050", c.High.String())
		}
	}
}

func TestFillGapsAndRollup(t *testing.T) {
	m1 := marketdata.NewCandle(market, marketdata.Interval1m, t0)
	m1.Add(tick(1, t0, "2000", "1"))
	m3 := marketdata.NewCandle(market, marketdata.Interval1m, t0.Add(3*time.Minute))
	m3.Add(tick(2, t0.Add(3*time.Minute), "2020", "2"))
	m3.Add(tick(3, t0.Add(3*time.Minute), "1980", "1"))

	out := marketdata.FillGaps([]marketdata.Candle{m1, m3}, market, marketdata.Interval1m, t0, t0.Add(5*time.Minute), nil)
	require.Len(t, out, 5)
	assert.EqualValues(t, 1, out[0].Trades)
	assert.EqualValues(t, 0, out[1].Trades, "synthesized")
	assert.Equal(t, "2000", out[1].Open.String())
	assert.Equal(t, "2000", out[1].Close.String())
	assert.True(t, out[1].Volume.IsZero())
	assert.EqualValues(t, 2, out[3].Trades)
	assert.Equal(t, "1980", out[4].Close.String(), "the last real close carries on")

	// no price to carry before the first candle: those buckets are skipped
	out = marketdata.FillGaps([]marketdata.Candle{m3}, market, marketdata.Interval1m, t0, t0.Add(5*time.Minute), nil)
	require.Len(t, out, 2)
	assert.Equal(t, t0.Add(3*time.Minute), out[0].Start)
	// unless the caller knows the close before the range
	prev := amt("1970")
	out = marketdata.FillGaps([]marketdata.Candle{m3}, market, marketdata.Interval1m, t0, t0.Add(5*time.Minute), &prev)
	require.Len(t, out, 5)
	assert.Equal(t, "1970", out[0].Close.String())
	// a candle before the range sets the carried close as well
	out = marketdata.FillGaps([]marketdata.Candle{m1, m3}, market, marketdata.Interval1m, t0.Add(time.Minute), t0.Add(3*time.Minute), nil)
	require.Len(t, out, 2)
	assert.Equal(t, "2000", out[0].Close.String())
	assert.Nil(t, marketdata.FillGaps(nil, market, marketdata.Interval1m, t0, t0, nil), "empty range")

	roll := marketdata.Rollup([]marketdata.Candle{m1, out[0], m3}, market, marketdata.Interval5m)
	require.Len(t, roll, 1)
	assert.Equal(t, t0, roll[0].Start)
	assert.Equal(t, "2000", roll[0].Open.String())
	assert.Equal(t, "2020", roll[0].High.String())
	assert.Equal(t, "1980", roll[0].Low.String())
	assert.Equal(t, "4", roll[0].Volume.String())
	assert.EqualValues(t, 3, roll[0].Trades, "synthesized candles do not count")
}

// TestGoldenKlines folds a fixed trade tape into every interval and compares
// with testdata/kline/tape.golden.json (`-update` rewrites it).
func TestGoldenKlines(t *testing.T) {
	// 40 trades over ~3 hours with a quiet hour in the middle, crossing a
	// day boundary so the 1d bucket rolls over
	start := time.Date(2026, 9, 5, 22, 58, 0, 0, time.UTC)
	var ticks []marketdata.TradeTick
	price := 2000
	for i := 0; i < 40; i++ {
		at := start.Add(time.Duration(i) * 3 * time.Minute)
		if i >= 20 && i < 30 {
			at = at.Add(time.Hour) // the gap
		}
		price += (i%7 - 3) * 5
		q := (i%5 + 1) * 25
		ticks = append(ticks, tick(uint64(i+1), at, money.FromInt64(int64(price)).String(), amt("0.0001").Mul(money.FromInt64(int64(q))).String()))
	}
	type golden struct {
		Candles []marketdata.Candle `json:"candles"`
		Ticker  marketdata.Ticker   `json:"ticker_at_end"`
	}
	agg := marketdata.NewAggregator(market)
	seen := map[marketdata.Interval]map[time.Time]marketdata.Candle{}
	for _, tk := range ticks {
		for _, c := range agg.Apply(tk) {
			if seen[c.Interval] == nil {
				seen[c.Interval] = map[time.Time]marketdata.Candle{}
			}
			seen[c.Interval][c.Start] = c
		}
	}
	var got golden
	for _, iv := range marketdata.Intervals() {
		var cs []marketdata.Candle
		for _, c := range seen[iv] {
			cs = append(cs, c)
		}
		first, last := ticks[0].At, ticks[len(ticks)-1].At
		got.Candles = append(got.Candles, marketdata.FillGaps(sortedCandles(cs), market, iv, first, last.Add(iv.Duration()), nil)...)
	}
	ring := marketdata.NewCandleRing(market)
	for _, tk := range ticks {
		ring.AddTrade(tk)
	}
	got.Ticker = ring.Ticker(ticks[len(ticks)-1].At.Add(time.Second))

	path := filepath.Join("testdata", "kline", "tape.golden.json")
	b, err := json.MarshalIndent(got, "", "  ")
	require.NoError(t, err)
	b = append(b, '\n')
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, b, 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden; run with -update")
	assert.Equal(t, string(want), string(b))
}

func sortedCandles(cs []marketdata.Candle) []marketdata.Candle {
	out := append([]marketdata.Candle(nil), cs...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Start.Before(out[j-1].Start); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

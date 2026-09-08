package marketdata_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/marketdata"
)

func TestTickerFrom(t *testing.T) {
	now := t0.Add(48 * time.Hour)
	empty := marketdata.TickerFrom(market, nil, now)
	assert.Nil(t, empty.Last)
	assert.Nil(t, empty.ChangePct)
	assert.True(t, empty.Volume.IsZero())
	assert.Equal(t, now, empty.At)

	old := marketdata.NewCandle(market, marketdata.Interval1m, now.Add(-25*time.Hour))
	old.Add(tick(1, now.Add(-25*time.Hour), "1000", "100"))
	a := marketdata.NewCandle(market, marketdata.Interval1m, now.Add(-23*time.Hour))
	a.Add(tick(2, now.Add(-23*time.Hour), "2000", "1"))
	a.Add(tick(3, now.Add(-23*time.Hour), "2100", "1"))
	b := marketdata.NewCandle(market, marketdata.Interval1m, now.Add(-time.Hour))
	b.Add(tick(4, now.Add(-time.Hour), "1900", "2"))
	b.Add(tick(5, now.Add(-time.Hour), "1950", "1"))
	future := marketdata.NewCandle(market, marketdata.Interval1m, now.Add(time.Minute))
	future.Add(tick(6, now.Add(time.Minute), "1", "1"))
	flat := marketdata.Flat(market, marketdata.Interval1m, now.Add(-2*time.Hour), amt("5"))

	tk := marketdata.TickerFrom(market, []marketdata.Candle{b, flat, old, a, future}, now)
	require.NotNil(t, tk.Last)
	assert.Equal(t, "1950", tk.Last.String())
	assert.Equal(t, "2000", tk.Open.String(), "the oldest candle inside the window opens it")
	assert.Equal(t, "2100", tk.High.String())
	assert.Equal(t, "1900", tk.Low.String())
	assert.Equal(t, "-50", tk.Change.String())
	assert.Equal(t, "-2.5", tk.ChangePct.String())
	assert.Equal(t, "5", tk.Volume.String(), "candles outside the window, in the future, or synthesized are ignored")
	assert.EqualValues(t, 4, tk.Trades)

	up := marketdata.NewCandle(market, marketdata.Interval1m, now.Add(-time.Minute))
	up.Add(tick(7, now.Add(-time.Minute), "3", "1"))
	up.Add(tick(8, now.Add(-time.Minute), "4", "1"))
	tk = marketdata.TickerFrom(market, []marketdata.Candle{up}, now)
	assert.Equal(t, "33.33", tk.ChangePct.String(), "truncated to two decimals")
}

func TestCandleRing(t *testing.T) {
	r := marketdata.NewCandleRing(market)
	_, ok := r.LastClose()
	assert.False(t, ok)

	at := time.Date(2026, 9, 5, 10, 12, 30, 0, time.UTC)
	out := r.AddTrade(tick(1, at, "2000", "1"))
	require.Len(t, out, 5, "every interval has a current candle now")
	out = r.AddTrade(tick(2, at.Add(time.Minute), "2020", "1"))
	assert.EqualValues(t, 1, out[0].Trades, "1m: a new minute")
	assert.EqualValues(t, 2, out[1].Trades, "5m: 10:10-10:15 holds both")
	assert.EqualValues(t, 2, out[2].Trades, "15m")
	assert.EqualValues(t, 2, out[4].Trades, "1d")
	out = r.AddTrade(tick(3, at.Add(time.Minute), "1990", "1"))
	assert.Equal(t, "2020", out[1].High.String())
	assert.Equal(t, "1990", out[1].Low.String())
	assert.Equal(t, "2000", out[1].Open.String())
	assert.Len(t, r.Candles(), 2)

	// rollup over a bucket boundary: 10:15 starts a new 5m and 15m candle
	// while the hour keeps everything
	r.AddTrade(tick(4, at.Add(time.Minute), "1995", "1"))
	out = r.AddTrade(tick(5, at.Add(3*time.Minute), "2005", "1"))
	assert.EqualValues(t, 1, out[1].Trades, "5m rolled over at 10:15")
	assert.EqualValues(t, 1, out[2].Trades, "15m rolled over at 10:15")
	assert.EqualValues(t, 5, out[3].Trades, "1h still holds all five")
	c, ok := r.Current(marketdata.Interval1h, at)
	require.True(t, ok)
	assert.EqualValues(t, 5, c.Trades)
	_, ok = r.Current(marketdata.Interval1h, at.Add(2*time.Hour))
	assert.False(t, ok)

	last, ok := r.LastClose()
	require.True(t, ok)
	assert.Equal(t, "2005", last.String())
	tk := r.Ticker(at.Add(4 * time.Minute))
	assert.Equal(t, "2000", tk.Open.String())
	assert.Equal(t, "5", tk.Volume.String())

	// seeding replaces a bucket, ignores empties and other intervals
	r.Seed([]marketdata.Candle{
		marketdata.Flat(market, marketdata.Interval1m, at, amt("1")),
		{Market: market, Interval: marketdata.Interval5m, Start: at.Truncate(5 * time.Minute), Trades: 9},
		{Market: market, Interval: marketdata.Interval1m, Start: at.Truncate(time.Minute), Open: amt("1"), High: amt("1"), Low: amt("1"), Close: amt("1"), Volume: amt("1"), QuoteVolume: amt("1"), Trades: 1},
	})
	assert.Len(t, r.Candles(), 3)
	assert.Equal(t, "1", r.Candles()[0].Close.String())

	// pruning keeps the window
	r.Prune(at.Add(24*time.Hour + 2*time.Minute))
	assert.Len(t, r.Candles(), 1)
	r.Prune(at.Add(48 * time.Hour))
	assert.Empty(t, r.Candles())
}

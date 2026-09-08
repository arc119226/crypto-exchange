package marketdata_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/marketdata"
)

func TestBookRestoreAndDepth(t *testing.T) {
	b := marketdata.NewBook(market)
	require.NoError(t, b.Restore(marketdata.BookSnapshot{Market: market, Seq: 7, Orders: []marketdata.RestingOrder{
		{ID: "b1", Side: marketdata.Buy, Price: amt("1990"), Remaining: amt("0.5")},
		{ID: "b2", Side: marketdata.Buy, Price: amt("1995.5"), Remaining: amt("0.25")},
		{ID: "b3", Side: marketdata.Buy, Price: amt("1990.00"), Remaining: amt("0.1")},
		{ID: "a1", Side: marketdata.Sell, Price: amt("2005"), Remaining: amt("1")},
		{ID: "a2", Side: marketdata.Sell, Price: amt("2001"), Remaining: amt("2")},
	}}))
	assert.Equal(t, uint64(7), b.Seq())
	assert.Equal(t, 5, b.Len())

	d := b.Depth(0)
	require.Len(t, d.Bids, 2)
	require.Len(t, d.Asks, 2)
	assert.Equal(t, "1995.5", d.Bids[0].Price.String(), "bids best first")
	assert.Equal(t, "1990", d.Bids[1].Price.String(), "1990 and 1990.00 are one level")
	assert.True(t, d.Bids[1].Qty.Equal(amt("0.6")))
	assert.Equal(t, 2, d.Bids[1].Orders)
	assert.Equal(t, "2001", d.Asks[0].Price.String(), "asks lowest first")
	assert.Equal(t, "2005", d.Asks[1].Price.String())

	one := b.Depth(1)
	assert.Len(t, one.Bids, 1)
	assert.Len(t, one.Asks, 1)

	best, ok := b.Best(marketdata.Buy)
	require.True(t, ok)
	assert.Equal(t, "1995.5", best.String())
	best, ok = b.Best(marketdata.Sell)
	require.True(t, ok)
	assert.Equal(t, "2001", best.String())

	require.ErrorIs(t, b.Restore(marketdata.BookSnapshot{Market: market}), marketdata.ErrInconsistent, "restore twice")
}

func TestBookRestoreRejectsBadOrders(t *testing.T) {
	cases := map[string]marketdata.RestingOrder{
		"no id":          {Side: marketdata.Buy, Price: amt("1"), Remaining: amt("1")},
		"bad side":       {ID: "x", Side: "long", Price: amt("1"), Remaining: amt("1")},
		"zero price":     {ID: "x", Side: marketdata.Buy, Price: amt("0"), Remaining: amt("1")},
		"zero remaining": {ID: "x", Side: marketdata.Buy, Price: amt("1"), Remaining: amt("0")},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			b := marketdata.NewBook(market)
			require.ErrorIs(t, b.Restore(marketdata.BookSnapshot{Orders: []marketdata.RestingOrder{o}}), marketdata.ErrInconsistent)
		})
	}
	b := marketdata.NewBook(market)
	dup := marketdata.RestingOrder{ID: "x", Side: marketdata.Buy, Price: amt("1"), Remaining: amt("1")}
	require.ErrorIs(t, b.Restore(marketdata.BookSnapshot{Orders: []marketdata.RestingOrder{dup, dup}}), marketdata.ErrInconsistent)
}

func TestPriceLevelJSON(t *testing.T) {
	b, err := json.Marshal(marketdata.PriceLevel{Price: amt("1990.50"), Qty: amt("0.4000")})
	require.NoError(t, err)
	assert.Equal(t, `["1990.5","0.4"]`, string(b), "canonical decimal strings, never numbers")
	var l marketdata.PriceLevel
	require.NoError(t, json.Unmarshal([]byte(`["1990.5","0"]`), &l))
	assert.True(t, l.Qty.IsZero())
	assert.Error(t, json.Unmarshal([]byte(`[1990.5, 0]`), &l), "numbers are refused")
	assert.Error(t, json.Unmarshal([]byte(`["x","0"]`), &l))
}

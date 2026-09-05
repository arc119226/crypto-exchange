package matching_test

import (
	"flag"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// TestMain raises rapid's default case count to the Phase 1 DoD (>= 1,000
// per property) unless -rapid.checks was given explicitly. Note that rapid
// divides the count by five under -short (`make test`); CI runs the property
// tests once more without -short to enforce the full count.
func TestMain(m *testing.M) {
	flag.Parse()
	if f := flag.Lookup("rapid.checks"); f != nil && f.Value.String() == f.DefValue {
		_ = f.Value.Set("1000")
	}
	os.Exit(m.Run())
}

func amt(s string) money.Amount { return money.MustParse(s) }

func ptrAmt(s string) *money.Amount { a := amt(s); return &a }

func ptrI32(v int32) *int32 { return &v }

var t0 = time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)

func ts(seq uint64) time.Time { return t0.Add(time.Duration(seq) * time.Second) }

// ethUSDC mirrors the Phase 0 seed: tick 0.01, step 0.0001, min notional 5.
func ethUSDC(stp matching.SelfTradePolicy) matching.MarketConfig {
	return matching.MarketConfig{
		Symbol: "ETH-USDC", PriceTick: amt("0.01"), QtyStep: amt("0.0001"), MinNotional: amt("5"),
		BaseScale: 18, QuoteScale: 6, SelfTradePolicy: stp,
	}
}

func newBook(t *testing.T, cfg matching.MarketConfig) *matching.Book {
	t.Helper()
	b, err := matching.New(cfg)
	require.NoError(t, err)
	return b
}

func limit(seq uint64, id, acct string, side matching.Side, price, qty string) matching.Command {
	return matching.Command{Seq: seq, Timestamp: ts(seq), New: &matching.NewOrder{
		OrderID: matching.OrderID(id), AccountID: matching.AccountID(acct), Side: side, Type: matching.Limit,
		TimeInForce: matching.GTC, Price: amt(price), Qty: amt(qty),
	}}
}

func limitIOC(seq uint64, id, acct string, side matching.Side, price, qty string) matching.Command {
	c := limit(seq, id, acct, side, price, qty)
	c.New.TimeInForce = matching.IOC
	return c
}

func marketBuy(seq uint64, id, acct, quote string) matching.Command {
	return matching.Command{Seq: seq, Timestamp: ts(seq), New: &matching.NewOrder{
		OrderID: matching.OrderID(id), AccountID: matching.AccountID(acct), Side: matching.Buy, Type: matching.Market, QuoteQty: amt(quote),
	}}
}

func marketSell(seq uint64, id, acct, qty string) matching.Command {
	return matching.Command{Seq: seq, Timestamp: ts(seq), New: &matching.NewOrder{
		OrderID: matching.OrderID(id), AccountID: matching.AccountID(acct), Side: matching.Sell, Type: matching.Market, Qty: amt(qty),
	}}
}

func cancel(seq uint64, id string) matching.Command {
	return matching.Command{Seq: seq, Timestamp: ts(seq), Cancel: &matching.Cancel{OrderID: matching.OrderID(id)}}
}

func apply(t *testing.T, b *matching.Book, cmd matching.Command) []matching.Event {
	t.Helper()
	evs, err := b.Apply(cmd)
	require.NoError(t, err, "seq %d", cmd.Seq)
	require.NotEmpty(t, evs, "seq %d produced no events", cmd.Seq)
	return evs
}

func kinds(evs []matching.Event) []matching.EventKind {
	out := make([]matching.EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind()
	}
	return out
}

func eq(t *testing.T, want string, got money.Amount, msg ...any) {
	t.Helper()
	require.Truef(t, amt(want).Equal(got), "want %s got %s %v", want, got, msg)
}

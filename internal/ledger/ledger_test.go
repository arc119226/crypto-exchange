package ledger_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
)

func amt(s string) money.Amount { return money.MustParse(s) }

func eq(t *testing.T, want string, got money.Amount, msg ...any) {
	t.Helper()
	require.Truef(t, amt(want).Equal(got), "want %s got %s %v", want, got, msg)
}

var fees = ledger.FeeParams{MakerBps: 10, TakerBps: 20, BaseScale: 18, QuoteScale: 6}

func TestComputeFee(t *testing.T) {
	cases := []struct {
		amount string
		bps    int32
		scale  int32
		want   string
	}{
		{"0.4", 20, 18, "0.0008"},       // docs/domain.md 1.3 (b) taker fee in ETH
		{"796", 10, 6, "0.796"},         // maker fee in USDC
		{"402.99", 10, 6, "0.40299"},    // domain.md 1.4 second trade, exact at 6 decimals
		{"0.000001", 1, 6, "0.000001"},  // 1e-10 exact → ceil to one unit of the scale
		{"1", 1, 6, "0.0001"},           // exact
		{"1.000001", 1, 6, "0.000101"},  // 0.0001000001 → rounds up, never down
		{"123.456789", 7, 6, "0.08642"}, // 0.0864197523 → 0.086420
		{"5", 0, 6, "0"},                // zero bps
		{"0", 20, 6, "0"},
		{"1000000", 10000, 6, "1000000"}, // 100 % edge
	}
	for _, c := range cases {
		got, err := ledger.ComputeFee(amt(c.amount), c.bps, c.scale)
		require.NoError(t, err, c)
		eq(t, c.want, got, c)
		assert.LessOrEqual(t, got.Scale(), c.scale)
	}
	_, err := ledger.ComputeFee(amt("-1"), 10, 6)
	assert.ErrorIs(t, err, ledger.ErrInvalidSettlement)
}

func TestEntryValidate(t *testing.T) {
	good := ledger.Entry{IdempotencyKey: "k", Kind: ledger.KindHold, Postings: []ledger.Posting{
		{AccountID: "a", Asset: "USDC", Bucket: ledger.BucketAvailable, Direction: ledger.Debit, Amount: amt("5")},
		{AccountID: "a", Asset: "USDC", Bucket: ledger.BucketHold, Direction: ledger.Credit, Amount: amt("5")},
	}}
	require.NoError(t, good.Validate())

	mut := func(f func(e *ledger.Entry)) ledger.Entry {
		e := good
		e.Postings = append([]ledger.Posting(nil), good.Postings...)
		f(&e)
		return e
	}
	cases := map[string]struct {
		e    ledger.Entry
		want error
	}{
		"no key":          {mut(func(e *ledger.Entry) { e.IdempotencyKey = "" }), ledger.ErrInvalidEntry},
		"no kind":         {mut(func(e *ledger.Entry) { e.Kind = "" }), ledger.ErrInvalidEntry},
		"one posting":     {mut(func(e *ledger.Entry) { e.Postings = e.Postings[:1] }), ledger.ErrInvalidEntry},
		"zero amount":     {mut(func(e *ledger.Entry) { e.Postings[0].Amount = money.Zero }), ledger.ErrInvalidEntry},
		"negative amount": {mut(func(e *ledger.Entry) { e.Postings[0].Amount = amt("-5") }), ledger.ErrInvalidEntry},
		"bad bucket":      {mut(func(e *ledger.Entry) { e.Postings[0].Bucket = "spare" }), ledger.ErrInvalidEntry},
		"bad direction":   {mut(func(e *ledger.Entry) { e.Postings[0].Direction = "sideways" }), ledger.ErrInvalidEntry},
		"missing account": {mut(func(e *ledger.Entry) { e.Postings[1].AccountID = "" }), ledger.ErrInvalidEntry},
		"unbalanced":      {mut(func(e *ledger.Entry) { e.Postings[1].Amount = amt("4.999999") }), ledger.ErrUnbalanced},
		"cross-asset":     {mut(func(e *ledger.Entry) { e.Postings[1].Asset = "ETH" }), ledger.ErrUnbalanced},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, c.e.Validate(), c.want)
		})
	}
	// multi-asset entries balance per asset
	multi := ledger.Entry{IdempotencyKey: "k", Kind: ledger.KindSettle, Postings: []ledger.Posting{
		{AccountID: "b", Asset: "USDC", Bucket: ledger.BucketHold, Direction: ledger.Debit, Amount: amt("796")},
		{AccountID: "s", Asset: "USDC", Bucket: ledger.BucketAvailable, Direction: ledger.Credit, Amount: amt("796")},
		{AccountID: "s", Asset: "ETH", Bucket: ledger.BucketHold, Direction: ledger.Debit, Amount: amt("0.4")},
		{AccountID: "b", Asset: "ETH", Bucket: ledger.BucketAvailable, Direction: ledger.Credit, Amount: amt("0.4")},
	}}
	require.NoError(t, multi.Validate())
}

func sumBy(postings []ledger.Posting, asset string, dir ledger.Direction) money.Amount {
	total := money.Zero
	for _, p := range postings {
		if p.Asset == asset && p.Direction == dir {
			total = total.Add(p.Amount)
		}
	}
	return total
}

func find(postings []ledger.Posting, account, asset string, bucket ledger.Bucket, dir ledger.Direction) money.Amount {
	total := money.Zero
	for _, p := range postings {
		if p.AccountID == account && p.Asset == asset && p.Bucket == bucket && p.Direction == dir {
			total = total.Add(p.Amount)
		}
	}
	return total
}

// docs/plan-v1.0.md §6.1.4 (b) / docs/domain.md §1.3 (b): B (taker, limit 2000) buys 0.4 @ 1990 from S (maker).
func TestBuildSettleEntry_PlanExample(t *testing.T) {
	entry, res, err := ledger.BuildSettleEntry(ledger.SettleParams{
		TradeID: "t1", BuyerAccountID: "B", SellerAccountID: "S", BuyerIsTaker: true,
		BaseAsset: "ETH", QuoteAsset: "USDC", Price: amt("1990"), Qty: amt("0.4"), QuoteQty: amt("796"),
		BuyerLimitPrice: amt("2000"), Fees: fees,
	}, "FEE")
	require.NoError(t, err)
	assert.Equal(t, "settle:trade:t1", entry.IdempotencyKey)
	assert.Equal(t, ledger.KindSettle, entry.Kind)
	assert.Equal(t, "trade", entry.RefType)
	eq(t, "0.0008", res.BuyerFee, "taker fee in ETH")
	eq(t, "0.796", res.SellerFee, "maker fee in USDC")
	eq(t, "0.0008", res.TakerFee)
	eq(t, "0.796", res.MakerFee)
	eq(t, "4", res.Release, "(2000 − 1990) × 0.4")

	eq(t, "800", sumBy(entry.Postings, "USDC", ledger.Debit), "796 + 4")
	eq(t, "800", sumBy(entry.Postings, "USDC", ledger.Credit), "795.204 + 0.796 + 4")
	eq(t, "0.4", sumBy(entry.Postings, "ETH", ledger.Debit))
	eq(t, "0.4", sumBy(entry.Postings, "ETH", ledger.Credit), "0.3992 + 0.0008")

	eq(t, "800", find(entry.Postings, "B", "USDC", ledger.BucketHold, ledger.Debit), "B hold USDC −796 −4")
	eq(t, "795.204", find(entry.Postings, "S", "USDC", ledger.BucketAvailable, ledger.Credit))
	eq(t, "0.796", find(entry.Postings, "FEE", "USDC", ledger.BucketHouse, ledger.Credit))
	eq(t, "0.4", find(entry.Postings, "S", "ETH", ledger.BucketHold, ledger.Debit))
	eq(t, "0.3992", find(entry.Postings, "B", "ETH", ledger.BucketAvailable, ledger.Credit))
	eq(t, "0.0008", find(entry.Postings, "FEE", "ETH", ledger.BucketHouse, ledger.Credit))
	eq(t, "4", find(entry.Postings, "B", "USDC", ledger.BucketAvailable, ledger.Credit), "price improvement back to available")
	assert.Len(t, entry.Postings, 8)
}

// docs/domain.md §1.4 trade 2: market buy (no release), maker S2 sells 0.2020 @ 1995.
func TestBuildSettleEntry_MarketBuyNoRelease(t *testing.T) {
	entry, res, err := ledger.BuildSettleEntry(ledger.SettleParams{
		TradeID: "t2", BuyerAccountID: "B", SellerAccountID: "S2", BuyerIsTaker: true,
		BaseAsset: "ETH", QuoteAsset: "USDC", Price: amt("1995"), Qty: amt("0.2020"), QuoteQty: amt("402.99"),
		Fees: fees,
	}, "FEE")
	require.NoError(t, err)
	eq(t, "0.000404", res.BuyerFee)
	eq(t, "0.40299", res.SellerFee)
	eq(t, "0", res.Release)
	assert.Len(t, entry.Postings, 6)
	eq(t, "402.58701", find(entry.Postings, "S2", "USDC", ledger.BucketAvailable, ledger.Credit))
	eq(t, "0.201596", find(entry.Postings, "B", "ETH", ledger.BucketAvailable, ledger.Credit))
}

func TestBuildSettleEntry_MakerBuyerAndZeroFees(t *testing.T) {
	// buyer is the maker: buyer pays maker bps in base, seller (taker) pays taker bps in quote
	_, res, err := ledger.BuildSettleEntry(ledger.SettleParams{
		TradeID: "t3", BuyerAccountID: "B", SellerAccountID: "S", BuyerIsTaker: false,
		BaseAsset: "ETH", QuoteAsset: "USDC", Price: amt("2000"), Qty: amt("1"), QuoteQty: amt("2000"),
		BuyerLimitPrice: amt("2000"), Fees: fees,
	}, "FEE")
	require.NoError(t, err)
	eq(t, "0.001", res.BuyerFee, "maker 10 bps of 1 ETH")
	eq(t, "4", res.SellerFee, "taker 20 bps of 2000 USDC")
	eq(t, "0.001", res.MakerFee)
	eq(t, "4", res.TakerFee)
	eq(t, "0", res.Release, "limit equals price")

	entry, res, err := ledger.BuildSettleEntry(ledger.SettleParams{
		TradeID: "t4", BuyerAccountID: "B", SellerAccountID: "S", BuyerIsTaker: true,
		BaseAsset: "ETH", QuoteAsset: "USDC", Price: amt("2000"), Qty: amt("1"), QuoteQty: amt("2000"),
		Fees: ledger.FeeParams{BaseScale: 18, QuoteScale: 6},
	}, "FEE")
	require.NoError(t, err)
	assert.Len(t, entry.Postings, 4, "no fee postings when fees are zero")
	eq(t, "0", res.BuyerFee)
	eq(t, "0", res.SellerFee)
}

func TestBuildSettleEntry_Errors(t *testing.T) {
	base := ledger.SettleParams{
		TradeID: "t", BuyerAccountID: "B", SellerAccountID: "S", BuyerIsTaker: true,
		BaseAsset: "ETH", QuoteAsset: "USDC", Price: amt("2000"), Qty: amt("1"), QuoteQty: amt("2000"), Fees: fees,
	}
	cases := map[string]func(p *ledger.SettleParams){
		"missing trade id":    func(p *ledger.SettleParams) { p.TradeID = "" },
		"missing buyer":       func(p *ledger.SettleParams) { p.BuyerAccountID = "" },
		"same asset":          func(p *ledger.SettleParams) { p.QuoteAsset = "ETH" },
		"quote mismatch":      func(p *ledger.SettleParams) { p.QuoteQty = amt("1999") },
		"zero qty":            func(p *ledger.SettleParams) { p.Qty = money.Zero; p.QuoteQty = money.Zero },
		"limit below price":   func(p *ledger.SettleParams) { p.BuyerLimitPrice = amt("1999") },
		"bps out of range":    func(p *ledger.SettleParams) { p.Fees.TakerBps = 10001 },
		"qty beyond scale":    func(p *ledger.SettleParams) { p.Fees.BaseScale = 0 },
		"fee eats the fill":   func(p *ledger.SettleParams) { p.Fees.TakerBps = 10000 },
		"missing fee account": nil,
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			p := base
			fee := "FEE"
			if f == nil {
				fee = ""
			} else {
				f(&p)
			}
			_, _, err := ledger.BuildSettleEntry(p, fee)
			assert.ErrorIs(t, err, ledger.ErrInvalidSettlement)
		})
	}
}

func TestHouseCodeTypes(t *testing.T) {
	assert.Equal(t, ledger.TypeRevenue, ledger.HouseFeeRevenue.Type())
	assert.Equal(t, ledger.TypeExpense, ledger.HouseGasExpense.Type())
	assert.Equal(t, ledger.TypeAsset, ledger.HouseCustodyHot.Type())
	assert.Equal(t, ledger.TypeAsset, ledger.HouseCustodyDepositAddresses.Type())
	assert.Equal(t, ledger.TypeLiability, ledger.HousePendingWithdrawal.Type())
	assert.Equal(t, ledger.TypeExternal, ledger.HouseExternal.Type())
	assert.True(t, ledger.TypeAsset.DebitNormal())
	assert.True(t, ledger.TypeExpense.DebitNormal())
	assert.False(t, ledger.TypeLiability.DebitNormal())
	assert.False(t, ledger.TypeRevenue.DebitNormal())
	for _, c := range ledger.AllHouseCodes {
		assert.True(t, c.Valid(), c)
	}
	assert.False(t, ledger.HouseCode("slush").Valid())
	b := ledger.Balance{Available: amt("1"), Hold: amt("2")}
	eq(t, "3", b.Total())
}

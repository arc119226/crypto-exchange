package ledger

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"github.com/arc119226/crypto-exchange/internal/ledger/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
)

func amt(s string) money.Amount { return money.MustParse(s) }

func house(id, code string) sqlcgen.LedgerAccount {
	return sqlcgen.LedgerAccount{ID: id, Kind: "house", HouseCode: &code}
}

func spot(id string) sqlcgen.LedgerAccount {
	return sqlcgen.LedgerAccount{ID: id, Kind: "spot"}
}

// The entry kind cannot say where a fee came from -- a trading fee rides
// inside a settle entry -- so the source label is read off the reference type.
// Anything unrecognised must land somewhere rather than being dropped: an
// unlabelled fee is revenue that silently is not in the metric.
func TestFeeSourceLabelsTheThreeDocumentedSourcesAndKeepsTheRest(t *testing.T) {
	for ref, want := range map[string]string{
		"trade": "trade", "withdrawal": "withdrawal", "deposit": "deposit",
		"sweep": "other", "nonce_fill": "other", "house_adjustment": "other", "": "other",
	} {
		assert.Equal(t, want, feeSource(ref), "ref type %q", ref)
	}
}

// Every asset ships at a zero rate, so the entry with no house posting is the
// path almost every entry takes. It must touch nothing: creating a zero series
// for every asset that ever traded would make the metric noise.
func TestObserveFeesIgnoresEntriesWithNoHousePosting(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	accounts := []sqlcgen.LedgerAccount{spot("user-a"), spot("user-b")}

	m.observeFees(Entry{
		Kind: KindHold, RefType: "order",
		Postings: []Posting{
			{AccountID: "user-a", Asset: "ETH", Bucket: BucketAvailable, Direction: Debit, Amount: amt("1")},
			{AccountID: "user-a", Asset: "ETH", Bucket: BucketHold, Direction: Credit, Amount: amt("1")},
		},
	}, accounts)

	assert.Equal(t, 0, testutil.CollectAndCount(m.feeRevenue))
	assert.Equal(t, 0, testutil.CollectAndCount(m.gasExpense))
}

// The three fee shapes are all different, and only one of them is an entry
// whose whole purpose is the fee. The deposit case is the sharp one: its fee
// is a third posting beside the user's credit and the custody debit, so
// counting the entry rather than the posting would book the whole deposit as
// revenue.
func TestObserveFeesCountsOnlyTheFeeRevenueCredit(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	accounts := []sqlcgen.LedgerAccount{
		house("fees", "fee_revenue"), house("custody", "custody_deposit_addresses"), spot("user-a"),
	}

	// A trade: the settle entry credits fee_revenue in each leg's own asset.
	m.observeFees(Entry{
		Kind: KindSettle, RefType: "trade",
		Postings: []Posting{
			{AccountID: "user-a", Asset: "USDC", Bucket: BucketAvailable, Direction: Credit, Amount: amt("795.204")},
			{AccountID: "fees", Asset: "USDC", Bucket: BucketHouse, Direction: Credit, Amount: amt("0.796")},
			{AccountID: "fees", Asset: "ETH", Bucket: BucketHouse, Direction: Credit, Amount: amt("0.0008")},
		},
	}, accounts)

	// A withdrawal fee: its own entry, moving the fee out of the user's hold.
	m.observeFees(Entry{
		Kind: KindFee, RefType: "withdrawal",
		Postings: []Posting{
			{AccountID: "user-a", Asset: "ETH", Bucket: BucketHold, Direction: Debit, Amount: amt("0.001")},
			{AccountID: "fees", Asset: "ETH", Bucket: BucketHouse, Direction: Credit, Amount: amt("0.001")},
		},
	}, accounts)

	// A deposit: three postings, one of which is the fee.
	m.observeFees(Entry{
		Kind: KindDeposit, RefType: "deposit",
		Postings: []Posting{
			{AccountID: "custody", Asset: "USDC", Bucket: BucketHouse, Direction: Debit, Amount: amt("1000")},
			{AccountID: "user-a", Asset: "USDC", Bucket: BucketAvailable, Direction: Credit, Amount: amt("990")},
			{AccountID: "fees", Asset: "USDC", Bucket: BucketHouse, Direction: Credit, Amount: amt("10")},
		},
	}, accounts)

	assert.InDelta(t, 0.796, testutil.ToFloat64(m.feeRevenue.WithLabelValues("USDC", "trade")), 1e-12)
	assert.InDelta(t, 0.0008, testutil.ToFloat64(m.feeRevenue.WithLabelValues("ETH", "trade")), 1e-12)
	assert.InDelta(t, 0.001, testutil.ToFloat64(m.feeRevenue.WithLabelValues("ETH", "withdrawal")), 1e-12)
	assert.InDelta(t, 10, testutil.ToFloat64(m.feeRevenue.WithLabelValues("USDC", "deposit")), 1e-12)
	assert.Equal(t, 4, testutil.CollectAndCount(m.feeRevenue),
		"the custody debit and the user's credit are not revenue")
	assert.Equal(t, 0, testutil.CollectAndCount(m.gasExpense))
}

// Gas reaches gas_expense from three callers -- a withdrawal, a sweep and a
// nonce fill -- and the counter has no source label, because §15 defines it as
// every gas_expense debit. The consequence is worth pinning: all three land on
// one series, which is what the alert compares withdrawal fees against.
func TestObserveFeesAccumulatesGasFromEveryCaller(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	accounts := []sqlcgen.LedgerAccount{house("gas", "gas_expense"), house("hot", "custody_hot")}

	for _, tc := range []struct {
		ref    string
		amount string
	}{{"withdrawal", "0.000021"}, {"sweep", "0.000042"}, {"nonce_fill", "0.000007"}} {
		m.observeFees(Entry{
			Kind: KindGas, RefType: tc.ref,
			Postings: []Posting{
				{AccountID: "gas", Asset: "ETH", Bucket: BucketHouse, Direction: Debit, Amount: amt(tc.amount)},
				{AccountID: "hot", Asset: "ETH", Bucket: BucketHouse, Direction: Credit, Amount: amt(tc.amount)},
			},
		}, accounts)
	}

	assert.InDelta(t, 0.00007, testutil.ToFloat64(m.gasExpense.WithLabelValues("ETH")), 1e-12)
	assert.Equal(t, 1, testutil.CollectAndCount(m.gasExpense), "one series: the counter has no source label")
	assert.Equal(t, 0, testutil.CollectAndCount(m.feeRevenue), "the custody_hot credit is not revenue")
}

// Direction matters: a reversing entry that debits fee_revenue is revenue
// leaving, and a counter cannot go down. Counting it as an increase would make
// the metric drift away from the report in the one case where somebody is
// already looking closely.
func TestObserveFeesIgnoresTheWrongDirection(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	accounts := []sqlcgen.LedgerAccount{house("fees", "fee_revenue"), house("gas", "gas_expense")}

	m.observeFees(Entry{
		Kind: KindAdjustment, RefType: "house_adjustment",
		Postings: []Posting{
			{AccountID: "fees", Asset: "ETH", Bucket: BucketHouse, Direction: Debit, Amount: amt("1")},
			{AccountID: "gas", Asset: "ETH", Bucket: BucketHouse, Direction: Credit, Amount: amt("1")},
		},
	}, accounts)

	assert.Equal(t, 0, testutil.CollectAndCount(m.feeRevenue))
	assert.Equal(t, 0, testutil.CollectAndCount(m.gasExpense))
}

func TestHouseCodeOfIgnoresSpotAccountsAndUnknownIDs(t *testing.T) {
	rows := []sqlcgen.LedgerAccount{spot("user-a"), house("fees", "fee_revenue")}
	assert.Equal(t, HouseFeeRevenue, houseCodeOf(rows, "fees"))
	assert.Equal(t, HouseCode(""), houseCodeOf(rows, "user-a"))
	assert.Equal(t, HouseCode(""), houseCodeOf(rows, "nobody"))
}

package withdrawal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// The withdrawal package's coverage is otherwise all behind the integration
// build tag, which needs Docker. cancellationPostings is the one pure builder
// in it, and it is on a fee refund path -- so it is worth the local signal.

// sum adds every posting that matches, or zero when none does.
func sum(ps []ledger.Posting, account, asset string, b ledger.Bucket, d ledger.Direction) money.Amount {
	out := money.Zero
	for _, p := range ps {
		if p.AccountID == account && p.Asset == asset && p.Bucket == b && p.Direction == d {
			out = out.Add(p.Amount)
		}
	}
	return out
}

// A displaced withdrawal has its amount in pending_withdrawal, where
// broadcasting put it, and its fee still in hold, which the fee never left.
// The refund has to reach into both, and getting that wrong is invisible in
// the totals: crediting the user the right amount out of the wrong bucket
// still balances.
func TestCancellationRefundsTheAmountFromPendingAndTheFeeFromHold(t *testing.T) {
	row := sqlcgen.ChainWithdrawal{ID: "w1", AccountID: "acct", Asset: "ETH"}
	amount, fee := money.MustParse("0.05"), money.MustParse("0.001125")

	ps := cancellationPostings(row, "pending", amount, fee)

	require.Len(t, ps, 4)
	assert.Equal(t, "0.05", sum(ps, "pending", "ETH", ledger.BucketHouse, ledger.Debit).String(),
		"the amount leaves pending_withdrawal, not the user's hold")
	assert.Equal(t, "0.001125", sum(ps, "acct", "ETH", ledger.BucketHold, ledger.Debit).String(),
		"the fee leaves hold, where it stayed through the broadcast")
	assert.Equal(t, "0.051125", sum(ps, "acct", "ETH", ledger.BucketAvailable, ledger.Credit).String(),
		"the user gets both back, in one entry")

	// The real guard: the ledger's own per-asset debit = credit check.
	require.NoError(t, ledger.Entry{
		IdempotencyKey: "withdrawal:cancelled:w1", Kind: ledger.KindWithdrawal,
		RefType: "withdrawal", RefID: "w1", Postings: ps,
	}.Validate())
}

// Every asset ships at a zero rate, so this is the path every cancellation
// takes today. The ledger refuses a posting of zero, which is why the builder
// has to leave the pair out rather than post it as nothing.
func TestCancellationWithNoFeeIsTwoPostings(t *testing.T) {
	row := sqlcgen.ChainWithdrawal{ID: "w1", AccountID: "acct", Asset: "ETH"}

	ps := cancellationPostings(row, "pending", money.MustParse("0.05"), money.Zero)

	require.Len(t, ps, 2)
	assert.True(t, sum(ps, "acct", "ETH", ledger.BucketHold, ledger.Debit).IsZero(),
		"nothing is taken out of hold when nothing was charged")
	require.NoError(t, ledger.Entry{
		IdempotencyKey: "withdrawal:cancelled:w1", Kind: ledger.KindWithdrawal,
		RefType: "withdrawal", RefID: "w1", Postings: ps,
	}.Validate())
}

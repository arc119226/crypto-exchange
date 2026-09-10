package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// usdc and eth stand in for the two seeded assets: six decimals and
// eighteen, which is the difference that makes the rounding interesting.
func usdc(flat string, wBps, dBps int32) Asset {
	return Asset{Symbol: "USDC", Scale: 6, WithdrawalFee: money.MustParse(flat), WithdrawalFeeBps: wBps, DepositFeeBps: dBps}
}

func eth(flat string, wBps, dBps int32) Asset {
	return Asset{Symbol: "ETH", Scale: 18, WithdrawalFee: money.MustParse(flat), WithdrawalFeeBps: wBps, DepositFeeBps: dBps}
}

// Every asset ships at zero for both rates, so this is the path every
// withdrawal and every deposit takes until an operator changes something.
// It has to produce exactly zero -- not a rounded zero, not an error.
func TestFeesAreZeroUntilSomebodySetsARate(t *testing.T) {
	for _, a := range []Asset{usdc("0", 0, 0), eth("0", 0, 0)} {
		for _, amount := range []string{"0.000001", "1", "12345.6789"} {
			amt := money.MustParse(amount)

			w, err := a.WithdrawalFeeFor(amt)
			require.NoError(t, err)
			assert.True(t, w.IsZero(), "%s withdrawal fee of %s is %s", a.Symbol, amount, w)

			d, err := a.DepositFeeFor(amt)
			require.NoError(t, err)
			assert.True(t, d.IsZero(), "%s deposit fee of %s is %s", a.Symbol, amount, d)
		}
	}
}

func TestWithdrawalFeeAddsTheFlatAndProportionalParts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		asset  Asset
		amount string
		want   string
	}{
		// The flat part alone is what covers gas, and it is what the seed
		// would set first.
		{"flat only", eth("0.001", 0, 0), "0.5", "0.001"},
		{"flat is charged even on a tiny withdrawal", eth("0.001", 0, 0), "0.000000000000000001", "0.001"},
		// 0.5% of 100 USDC.
		{"proportional only", usdc("0", 50, 0), "100", "0.5"},
		// 5 USDC flat plus 0.25% of 1000.
		{"both parts", usdc("5", 25, 0), "1000", "7.5"},
		// The ceiling bites: 0.01% of one micro-USDC is a hundred-millionth
		// of a micro-USDC, which USDC cannot represent, so it rounds up to
		// the smallest unit the asset has. Rounding down would let a
		// withdrawal escape the proportional fee entirely.
		{"rounds up to the asset's smallest unit", usdc("0", 1, 0), "0.000001", "0.000001"},
		// Same rate on an amount that divides cleanly needs no rounding.
		{"no rounding when it divides", usdc("0", 100, 0), "1", "0.01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.asset.WithdrawalFeeFor(money.MustParse(tc.amount))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.String())
		})
	}
}

// The fee is charged on top, so what leaves the user is amount + fee while
// what reaches the destination is amount. Stating it as a test because it is
// the property the whole withdrawal path is built on: the precheck, the hold
// and the postings all use amount + fee, and the chain sends amount.
func TestWithdrawalFeeIsChargedOnTopOfTheAmount(t *testing.T) {
	a := usdc("5", 25, 0)
	amount := money.MustParse("1000")

	fee, err := a.WithdrawalFeeFor(amount)
	require.NoError(t, err)

	assert.Equal(t, "1007.5", amount.Add(fee).String(), "what the user pays")
	assert.Equal(t, "1000", amount.String(), "what the destination receives")
}

func TestDepositFeeComesOutOfWhatArrived(t *testing.T) {
	a := usdc("0", 0, 100) // 1%
	arrived := money.MustParse("1000")

	fee, err := a.DepositFeeFor(arrived)
	require.NoError(t, err)
	assert.Equal(t, "10", fee.String())
	assert.Equal(t, "990", arrived.Sub(fee).String(), "what the user is credited")
}

// Rounding up means a small enough deposit rounds to the whole thing. The
// user would be credited zero and the exchange would book the entire deposit
// as revenue, which is not a fee. It must be refused rather than computed.
func TestDepositFeeRefusesToConsumeTheWholeDeposit(t *testing.T) {
	a := usdc("0", 0, 1) // 0.01%, the smallest non-zero rate

	_, err := a.DepositFeeFor(money.MustParse("0.000001"))
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "would leave nothing")

	// One unit more and the fee is representable without taking everything.
	fee, err := a.DepositFeeFor(money.MustParse("0.000002"))
	require.NoError(t, err)
	assert.Equal(t, "0.000001", fee.String())

	// And the refusal must not fire on the default path: at 0 bps the same
	// dust deposit is credited in full.
	zero, err := usdc("0", 0, 0).DepositFeeFor(money.MustParse("0.000001"))
	require.NoError(t, err)
	assert.True(t, zero.IsZero())
}

// The case the original implementation got wrong, and the reason it was wrong:
// it divided at scale 18 and rounded up afterwards, so for an 18-decimal asset
// the ceiling had nothing left to round. Every test above used amounts whose
// quotient fits in 18 digits, which is why they all passed while ETH fees were
// silently floored -- and a small enough withdrawal was charged nothing at all.
func TestTheCeilingBitesOnAnEighteenDecimalAsset(t *testing.T) {
	// One wei at one basis point is a ten-thousandth of a wei, which ETH
	// cannot represent. Rounding up is one wei; rounding down is nothing.
	fee, err := eth("0", 1, 0).WithdrawalFeeFor(money.MustParse("0.000000000000000001"))
	require.NoError(t, err)
	assert.Equal(t, "0.000000000000000001", fee.String(),
		"a fee that rounds to nothing is a fee that was not charged")

	d, err := eth("0", 0, 1).DepositFeeFor(money.MustParse("0.00000000000000001"))
	require.NoError(t, err)
	assert.Equal(t, "0.000000000000000001", d.String())

	// And a quotient that needs more than eighteen digits still rounds up
	// when the amount is ordinary rather than dust.
	fee, err = eth("0", 1, 0).WithdrawalFeeFor(money.MustParse("1.0000000000000001"))
	require.NoError(t, err)
	assert.Equal(t, "0.000100000000000001", fee.String())
}

// The three fee paths -- trading, withdrawal, deposit -- must round the same
// way, or the same rate charges different amounts depending on which one it
// came through.
func TestEveryFeePathRoundsTheSameWay(t *testing.T) {
	amount := money.MustParse("1.0000000000000001")
	trading, err := amount.Mul(money.FromInt64(7)).DivRoundUp(money.FromInt64(10000), 18)
	require.NoError(t, err)

	withdrawal, err := eth("0", 7, 0).WithdrawalFeeFor(amount)
	require.NoError(t, err)
	deposit, err := eth("0", 0, 7).DepositFeeFor(amount)
	require.NoError(t, err)

	assert.Equal(t, trading.String(), withdrawal.String())
	assert.Equal(t, trading.String(), deposit.String())
}

func TestFeesRefuseNonPositiveAmounts(t *testing.T) {
	a := usdc("1", 10, 10)
	for _, amount := range []string{"0", "-1"} {
		_, err := a.WithdrawalFeeFor(money.MustParse(amount))
		assert.ErrorIs(t, err, ErrInvalid, "withdrawal fee of %s", amount)
		_, err = a.DepositFeeFor(money.MustParse(amount))
		assert.ErrorIs(t, err, ErrInvalid, "deposit fee of %s", amount)
	}
}

// A fee that does not fit the asset's scale could not be posted to the
// ledger, and rounding it at posting time would make the charge differ from
// the snapshot the user was quoted.
func TestFeesAlwaysFitTheAssetScale(t *testing.T) {
	for _, a := range []Asset{usdc("0", 33, 33), eth("0", 33, 33)} {
		for _, amount := range []string{"1", "3.7", "0.000123", "98765.4321"} {
			w, err := a.WithdrawalFeeFor(money.MustParse(amount))
			require.NoError(t, err)
			assert.LessOrEqual(t, w.Scale(), a.Scale, "%s withdrawal fee %s", a.Symbol, w)

			d, err := a.DepositFeeFor(money.MustParse(amount))
			require.NoError(t, err)
			assert.LessOrEqual(t, d.Scale(), a.Scale, "%s deposit fee %s", a.Symbol, d)
		}
	}
}

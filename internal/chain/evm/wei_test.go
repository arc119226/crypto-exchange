package evm_test

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/money"
)

func max256() *big.Int {
	return new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
}

// docs/plan-v1.0.md §6.5 asks for a round trip covering 0 and 1 wei; the
// 2^256-1 end of the range is TestFromWeiRefusesValuesTheLedgerCannotHold,
// because it is not representable and must not pretend to be.
func TestWeiRoundTrip(t *testing.T) {
	// the largest value NUMERIC(36,18) holds: 36 significant digits
	maxLedger, ok := new(big.Int).SetString("999999999999999999999999999999999999", 10)
	require.True(t, ok)

	for _, tc := range []struct {
		name  string
		wei   *big.Int
		scale int32
	}{
		{"zero", big.NewInt(0), 18},
		{"one wei", big.NewInt(1), 18},
		{"one ether", new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), 18},
		{"one usdc", big.NewInt(1_000_000), 6},
		{"one micro usdc", big.NewInt(1), 6},
		{"largest the ledger can hold", maxLedger, 18},
	} {
		t.Run(tc.name, func(t *testing.T) {
			amount, err := evm.FromWei(tc.wei, tc.scale)
			require.NoError(t, err)
			back, err := evm.ToWei(amount, tc.scale)
			require.NoError(t, err)
			assert.Zero(t, tc.wei.Cmp(back), "%s -> %s -> %s", tc.wei, amount, back)
		})
	}
}

func TestFromWeiDoesNotAliasItsInput(t *testing.T) {
	wei := big.NewInt(1234)
	_, err := evm.FromWei(wei, 18)
	require.NoError(t, err)
	wei.SetInt64(9999) // must not change the amount that was already produced

	amount, err := evm.FromWei(big.NewInt(1234), 18)
	require.NoError(t, err)
	assert.Equal(t, "0.000000000000001234", amount.String())
}

func TestToWeiKnownValues(t *testing.T) {
	for _, tc := range []struct {
		amount string
		scale  int32
		want   string
	}{
		{"0", 18, "0"},
		{"1", 18, "1000000000000000000"},
		{"1.5", 18, "1500000000000000000"},
		{"0.000000000000000001", 18, "1"},
		{"1", 6, "1000000"},
		{"0.01", 6, "10000"},
		{"1990.5", 6, "1990500000"},
	} {
		t.Run(tc.amount+"@"+string(rune('0'+tc.scale%10)), func(t *testing.T) {
			got, err := evm.ToWei(money.MustParse(tc.amount), tc.scale)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.String())
		})
	}
}

// TestToWeiRefusesExcessPrecision is the reason ToWei returns an error rather
// than rounding: quietly dropping a digit would credit a different number than
// the chain moved. (A value with more than 18 decimals cannot be a
// money.Amount at all, so the only case reachable here is an amount finer than
// its own asset's scale.)
func TestToWeiRefusesExcessPrecision(t *testing.T) {
	_, err := evm.ToWei(money.MustParse("0.0000001"), 6) // 7 dp into a 6 dp asset
	require.ErrorContains(t, err, "precision")

	// exactly at the scale is fine
	_, err = evm.ToWei(money.MustParse("0.000001"), 6)
	require.NoError(t, err)
}

// TestToWeiAcceptsTrailingZerosBeyondTheScale pins the other half of that
// rule, which is not the same thing and was once wrong.
//
// Every amount read back from the ledger carries eighteen decimal places,
// because that is what NUMERIC(36,18) returns. Judging precision by the number
// of decimals rather than by their value refused every single ERC-20
// withdrawal: 50 USDC arrives from the database as 50.000000000000000000, and
// twelve zeros are not lost precision.
func TestToWeiAcceptsTrailingZerosBeyondTheScale(t *testing.T) {
	fromLedger, err := money.ParseAmount("50.000000000000000000")
	require.NoError(t, err)
	got, err := evm.ToWei(fromLedger, 6)
	require.NoError(t, err)
	require.Equal(t, "50000000", got.String())

	// And a digit that is not zero is still refused, at any distance.
	_, err = evm.ToWei(money.MustParse("50.000000000000000001"), 6)
	require.ErrorContains(t, err, "precision")
}

// TestFromWeiRefusesValuesTheLedgerCannotHold: the ledger stores
// NUMERIC(36,18) (ADR-0004), so a uint256 near its maximum has no
// representation. A token can emit such a Transfer — a broken or hostile
// contract will — and the only safe answer is a clean error the scanner can
// alert on. Truncating would credit an account with a number that never moved.
func TestFromWeiRefusesValuesTheLedgerCannotHold(t *testing.T) {
	for _, scale := range []int32{0, 6, 18} {
		_, err := evm.FromWei(max256(), scale)
		require.Error(t, err, "scale %d", scale)
	}
}

func TestWeiRejectsBadInput(t *testing.T) {
	_, err := evm.ToWei(money.MustParse("-1"), 18)
	assert.ErrorContains(t, err, "negative")

	_, err = evm.ToWei(money.MustParse("1"), 19)
	assert.ErrorContains(t, err, "scale")

	_, err = evm.FromWei(nil, 18)
	assert.ErrorContains(t, err, "nil")

	_, err = evm.FromWei(big.NewInt(-1), 18)
	assert.ErrorContains(t, err, "negative")

	_, err = evm.FromWei(big.NewInt(1), -1)
	assert.ErrorContains(t, err, "scale")
}

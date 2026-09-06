package evm

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The selector is derived from the signature rather than written down, so this
// pins it against the value every ERC-20 explorer shows. If the derivation
// ever drifts, every withdrawal would call some other function.
func TestTransferSelectorIsTheOneEveryERC20Uses(t *testing.T) {
	assert.Equal(t, "a9059cbb", common.Bytes2Hex(transferSelector))
}

// Same reasoning for the read side. A wrong selector here returns whatever
// some other function happens to answer, and the sweeper would decide how much
// money to move from it.
func TestBalanceOfSelectorIsTheOneEveryERC20Uses(t *testing.T) {
	assert.Equal(t, "70a08231", common.Bytes2Hex(balanceOfSelector))
}

func TestTransferCalldataLaysOutSelectorThenTwoWords(t *testing.T) {
	to := common.HexToAddress("0x70997970c51812dc3a010c7d01b50e0d17dc79c8")
	data, err := TransferCalldata(to, big.NewInt(250500000))
	require.NoError(t, err)
	require.Len(t, data, 4+32+32, "a transfer call is exactly a selector and two words")
	assert.Equal(t, "a9059cbb", common.Bytes2Hex(data[:4]))
	assert.Equal(t, to, common.BytesToAddress(data[4:36]), "the recipient is left-padded to a full word")
	assert.Equal(t, big.NewInt(250500000), new(big.Int).SetBytes(data[36:]))
}

func TestTransferCalldataRefusesWhatCannotBeEncoded(t *testing.T) {
	to := common.HexToAddress("0x70997970c51812dc3a010c7d01b50e0d17dc79c8")
	_, err := TransferCalldata(to, big.NewInt(-1))
	assert.Error(t, err, "a negative amount is not a uint256")
	_, err = TransferCalldata(to, nil)
	assert.Error(t, err)

	// 2^256 needs 257 bits and would silently truncate to zero if packed.
	huge := new(big.Int).Lsh(big.NewInt(1), 256)
	_, err = TransferCalldata(to, huge)
	assert.Error(t, err)

	// One less fits exactly, and must be accepted.
	max := new(big.Int).Sub(huge, big.NewInt(1))
	data, err := TransferCalldata(to, max)
	require.NoError(t, err)
	assert.Equal(t, max, new(big.Int).SetBytes(data[36:]))
}

// A replacement must raise the fee by at least 10% or the node refuses it, so
// the bump rounds up: one wei short is a rejection, not a near miss.
func TestFeesBumpAlwaysClearsTheReplacementThreshold(t *testing.T) {
	f := Fees{TipCap: big.NewInt(1), FeeCap: big.NewInt(1)}
	up := f.Bump(10)
	assert.Equal(t, "2", up.TipCap.String(), "1 + 10% rounds up to 2, not down to 1")
	assert.Equal(t, "2", up.FeeCap.String())

	f = Fees{TipCap: big.NewInt(1_000_000_000), FeeCap: big.NewInt(50_000_000_000)}
	up = f.Bump(10)
	assert.Equal(t, "1100000000", up.TipCap.String())
	assert.Equal(t, "55000000000", up.FeeCap.String())

	// Below the protocol minimum the argument is ignored rather than obeyed.
	assert.Equal(t, up.FeeCap.String(), f.Bump(0).FeeCap.String())
}

func TestFeesCapAtKeepsTheTipUnderTheCeiling(t *testing.T) {
	f := Fees{TipCap: big.NewInt(30), FeeCap: big.NewInt(100)}
	capped := f.CapAt(big.NewInt(20))
	assert.Equal(t, "20", capped.FeeCap.String())
	assert.Equal(t, "20", capped.TipCap.String(), "a tip above the fee cap is not a valid transaction")

	// The original is untouched: callers keep their own copy.
	assert.Equal(t, "100", f.FeeCap.String())
	// No ceiling means no change.
	assert.Equal(t, "100", f.CapAt(nil).FeeCap.String())
	assert.Equal(t, "100", f.CapAt(big.NewInt(0)).FeeCap.String())
}

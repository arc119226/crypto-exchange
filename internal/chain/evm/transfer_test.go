package evm_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
)

// The ERC-20 Transfer topic is the same well-known constant in every block
// explorer and every token contract; pinning it here means a change to how we
// compute it cannot quietly stop the scanner from seeing any ERC-20 deposit.
const publishedTransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

func TestTransferTopicMatchesTheStandard(t *testing.T) {
	assert.Equal(t, publishedTransferTopic, evm.TransferTopic.Hex())
}

func transferLog(from, to common.Address, value *big.Int) types.Log {
	return types.Log{
		Address: common.HexToAddress("0x5FbDB2315678afecb367f032d93F642f64180aa3"),
		Topics: []common.Hash{
			evm.TransferTopic,
			common.BytesToHash(from.Bytes()),
			common.BytesToHash(to.Bytes()),
		},
		Data: common.LeftPadBytes(value.Bytes(), 32),
	}
}

func TestDecodeTransfer(t *testing.T) {
	from := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	to := common.HexToAddress("0x70997970C51812dc3A010C7d01b50e0d17dc79C8")

	got, err := evm.DecodeTransfer(transferLog(from, to, big.NewInt(1_500_000)))
	require.NoError(t, err)
	assert.Equal(t, from, got.From)
	assert.Equal(t, to, got.To)
	assert.Equal(t, "1500000", got.Value.String())
	assert.Equal(t, "0x5FbDB2315678afecb367f032d93F642f64180aa3", got.Contract.Hex())

	t.Run("zero value decodes", func(t *testing.T) {
		got, err := evm.DecodeTransfer(transferLog(from, to, big.NewInt(0)))
		require.NoError(t, err)
		assert.Equal(t, "0", got.Value.String())
	})

	t.Run("a full uint256 decodes without wrapping", func(t *testing.T) {
		got, err := evm.DecodeTransfer(transferLog(from, to, max256()))
		require.NoError(t, err)
		assert.Equal(t, max256().String(), got.Value.String(),
			"decoding must not lose the value; refusing it is FromWei's job")
	})
}

// A token that indexes its value, or packs the addresses into data, is not the
// standard layout. Guessing would mean crediting an account from a number we
// are not sure of.
func TestDecodeTransferRefusesNonStandardLayouts(t *testing.T) {
	addr := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")

	t.Run("value indexed as a third topic", func(t *testing.T) {
		l := transferLog(addr, addr, big.NewInt(1))
		l.Topics = append(l.Topics, common.BytesToHash(big.NewInt(1).Bytes()))
		l.Data = nil
		_, err := evm.DecodeTransfer(l)
		assert.ErrorContains(t, err, "topics")
	})

	t.Run("everything in data", func(t *testing.T) {
		l := transferLog(addr, addr, big.NewInt(1))
		l.Topics = []common.Hash{evm.TransferTopic}
		l.Data = make([]byte, 96)
		_, err := evm.DecodeTransfer(l)
		assert.ErrorContains(t, err, "topics")
	})

	t.Run("short data", func(t *testing.T) {
		l := transferLog(addr, addr, big.NewInt(1))
		l.Data = l.Data[:16]
		_, err := evm.DecodeTransfer(l)
		assert.ErrorContains(t, err, "data bytes")
	})

	t.Run("a different event", func(t *testing.T) {
		l := transferLog(addr, addr, big.NewInt(1))
		l.Topics[0] = common.HexToHash("0xdeadbeef")
		_, err := evm.DecodeTransfer(l)
		assert.ErrorContains(t, err, "not a Transfer")
	})
}

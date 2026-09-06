//go:build integration

package integration

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
)

// fakeChain is a scripted EVM node.
//
// The anvil tests prove the scanner against a real node, but they need Docker
// and can only stage a reorg approximately. This drives the exact drill
// docs/domain.md §4 describes — a branch abandoned at a named height, with the
// same transaction reappearing at a different one — so the reorg and
// confirmation logic can be executed and read line by line.
type fakeChain struct {
	mu       sync.Mutex
	chainID  int64
	genesis  string
	blocks   []fakeBlock // index == height
	receipts map[common.Hash]uint64
}

type fakeBlock struct {
	hash string
	txs  []*types.Transaction
	logs []types.Log
}

var _ deposit.Chain = (*fakeChain)(nil)

func newFakeChain(chainID int64) *fakeChain {
	c := &fakeChain{chainID: chainID, genesis: hash32("genesis"), receipts: map[common.Hash]uint64{}}
	c.blocks = []fakeBlock{{hash: c.genesis}}
	return c
}

// transfer builds a native value transfer. The nonce is the transaction's
// identity: the same nonce, recipient and value give the same hash on any
// branch, which is exactly how a reorged transaction reappears.
func transfer(nonce uint64, to common.Address, wei *big.Int) *types.Transaction {
	return types.NewTx(&types.LegacyTx{Nonce: nonce, To: &to, Value: wei, Gas: 21000})
}

// mine appends a block. The label names the block, so a test can say "the same
// height, a different block" and mean it.
func (c *fakeChain) mine(label string, txs ...*types.Transaction) uint64 {
	return c.mineWith(label, txs, nil)
}

// mineFailed appends a block whose transactions all reverted: they appear in
// the block but moved nothing.
func (c *fakeChain) mineFailed(label string, txs ...*types.Transaction) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tx := range txs {
		c.receipts[tx.Hash()] = 0
	}
	c.blocks = append(c.blocks, fakeBlock{hash: hash32(label), txs: txs})
	return uint64(len(c.blocks) - 1)
}

// mineLogs appends a block carrying ERC-20 Transfer logs.
func (c *fakeChain) mineLogs(label string, logs ...types.Log) uint64 {
	return c.mineWith(label, nil, logs)
}

func (c *fakeChain) mineWith(label string, txs []*types.Transaction, logs []types.Log) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tx := range txs {
		c.receipts[tx.Hash()] = 1
	}
	h := hash32(label)
	n := uint64(len(c.blocks))
	for i := range logs {
		logs[i].BlockNumber = n
		logs[i].BlockHash = common.HexToHash(h)
	}
	c.blocks = append(c.blocks, fakeBlock{hash: h, txs: txs, logs: logs})
	return n
}

// rewind drops every block above height, modelling a branch losing.
func (c *fakeChain) rewind(height uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks = c.blocks[:height+1]
}

func (c *fakeChain) ChainID(context.Context) (int64, error)      { return c.chainID, nil }
func (c *fakeChain) GenesisHash(context.Context) (string, error) { return c.genesis, nil }

func (c *fakeChain) Head(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(len(c.blocks) - 1), nil
}

func (c *fakeChain) BlockByNumber(_ context.Context, number uint64) (evm.Block, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if number >= uint64(len(c.blocks)) {
		return evm.Block{}, fmt.Errorf("%w: block %d", evm.ErrNotFound, number)
	}
	b := c.blocks[number]
	parent := ""
	if number > 0 {
		parent = c.blocks[number-1].hash
	}
	return evm.Block{Number: number, Hash: b.hash, ParentHash: parent, Txs: b.txs}, nil
}

func (c *fakeChain) TransferLogs(_ context.Context, from, to uint64, contracts []common.Address) ([]types.Log, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	watched := map[common.Address]bool{}
	for _, a := range contracts {
		watched[a] = true
	}
	var out []types.Log
	for n := from; n <= to && n < uint64(len(c.blocks)); n++ {
		for _, l := range c.blocks[n].logs {
			if watched[l.Address] {
				out = append(out, l)
			}
		}
	}
	return out, nil
}

func (c *fakeChain) Receipt(_ context.Context, txHash string) (*types.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status, ok := c.receipts[common.HexToHash(txHash)]
	if !ok {
		return nil, fmt.Errorf("%w: receipt %s", evm.ErrNotFound, txHash)
	}
	return &types.Receipt{Status: status}, nil
}

// erc20Log builds a standard Transfer log, the shape evm.DecodeTransfer
// accepts.
func erc20Log(token, from, to common.Address, value *big.Int, index uint) types.Log {
	return types.Log{
		Address: token,
		Topics: []common.Hash{
			evm.TransferTopic,
			common.BytesToHash(from.Bytes()),
			common.BytesToHash(to.Bytes()),
		},
		Data:   common.LeftPadBytes(value.Bytes(), 32),
		Index:  index,
		TxHash: common.BytesToHash([]byte(fmt.Sprintf("erc20-tx-%d", index))),
	}
}

func hash32(label string) string {
	return strings.ToLower(common.BytesToHash([]byte(label)).Hex())
}

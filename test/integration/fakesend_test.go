//go:build integration

package integration

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/hotwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/sweep"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
)

// sendChain is a scripted node for the send half of the withdrawal machine.
//
// The deposit tests' fakeChain models blocks arriving; this one models
// transactions leaving, which needs the opposite things: a mempool, a nonce
// the chain believes in, and receipts a test can withhold. Everything the
// signer, the nonce manager and the broadcaster do that a real chain makes
// hard to reach — a transaction that sits unmined for exactly long enough, a
// node that refuses a send, a hot wallet whose nonce is ahead of ours — is one
// method call here.
type sendChain struct {
	mu      sync.Mutex
	chainID int64
	head    uint64
	// pool is what has been accepted and not yet mined, by hash.
	pool map[common.Hash]*types.Transaction
	// mined maps a hash to the receipt the node will return.
	mined map[common.Hash]*types.Receipt
	// minedNonce is what eth_getTransactionCount(latest) answers.
	minedNonce map[common.Address]uint64
	// foreignNonce is added to what this chain has actually seen, so a test
	// can say "somebody else has been sending from this address" without
	// having to forge their transactions.
	foreignNonce map[common.Address]uint64
	// ether and tokens are the balances this chain keeps. Mining a transaction
	// moves them, so a sweep can be checked by what the hot wallet ends up
	// holding rather than by what the code says it sent.
	ether   map[common.Address]*big.Int
	tokens  map[common.Address]map[common.Address]*big.Int
	baseFee *big.Int
	tip     *big.Int
	// sendErr fails the next SendRawTransaction, once.
	sendErr error
	// tokenErr fails every TokenBalance for one contract, standing in for a
	// registry row that names an address with no token behind it.
	tokenErr map[common.Address]error
	// sent records every raw transaction that reached the node, in order.
	sent []*types.Transaction
}

var (
	_ withdrawal.Chain = (*sendChain)(nil)
	_ hotwallet.Chain  = (*sendChain)(nil)
	_ sweep.Chain      = (*sendChain)(nil)
)

func newSendChain(chainID int64) *sendChain {
	return &sendChain{
		chainID: chainID, head: 1,
		pool:       map[common.Hash]*types.Transaction{},
		mined:      map[common.Hash]*types.Receipt{},
		minedNonce: map[common.Address]uint64{}, foreignNonce: map[common.Address]uint64{},
		ether:    map[common.Address]*big.Int{},
		tokens:   map[common.Address]map[common.Address]*big.Int{},
		tokenErr: map[common.Address]error{},
		baseFee:  big.NewInt(1_000_000_000), tip: big.NewInt(1_500_000_000),
	}
}

func (c *sendChain) Head(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head, nil
}

func (c *sendChain) NonceAt(_ context.Context, a common.Address) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.minedNonce[a] + c.foreignNonce[a], nil
}

// PendingNonceAt counts the mempool too, which is what makes the nonce
// manager's three startup cases distinguishable.
func (c *sendChain) PendingNonceAt(_ context.Context, a common.Address) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.minedNonce[a] + c.foreignNonce[a]
	for _, tx := range c.pool {
		if c.senderOf(tx) == a && tx.Nonce()+1 > next {
			next = tx.Nonce() + 1
		}
	}
	return next, nil
}

func (c *sendChain) SendRawTransaction(_ context.Context, raw []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return fmt.Errorf("sendChain: undecodable transaction: %w", err)
	}
	if err := c.sendErr; err != nil {
		c.sendErr = nil
		return err
	}
	if _, ok := c.mined[tx.Hash()]; ok {
		return evm.ErrKnownTransaction
	}
	// A replacement displaces the transaction it outbids, which is the whole
	// reason a bump works: same sender, same nonce, one of them survives.
	sender := c.senderOf(tx)
	for h, in := range c.pool {
		if c.senderOf(in) == sender && in.Nonce() == tx.Nonce() && h != tx.Hash() {
			delete(c.pool, h)
		}
	}
	c.pool[tx.Hash()] = tx
	c.sent = append(c.sent, tx)
	return nil
}

func (c *sendChain) SuggestFees(context.Context) (evm.Fees, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return evm.Fees{
		TipCap: new(big.Int).Set(c.tip),
		FeeCap: new(big.Int).Add(new(big.Int).Mul(c.baseFee, big.NewInt(2)), c.tip),
	}, nil
}

func (c *sendChain) EstimateGas(context.Context, common.Address, common.Address, *big.Int, []byte) (uint64, error) {
	return 60000, nil
}

func (c *sendChain) Balance(_ context.Context, a common.Address) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(c.balanceOf(a)), nil
}

func (c *sendChain) TokenBalance(_ context.Context, token, holder common.Address) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.tokenErr[token]; err != nil {
		return nil, err
	}
	return new(big.Int).Set(c.tokenOf(token, holder)), nil
}

func (c *sendChain) Receipt(_ context.Context, txHash string) (*types.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.mined[common.HexToHash(txHash)]
	if !ok {
		return nil, fmt.Errorf("%w: receipt %s", evm.ErrNotFound, txHash)
	}
	return r, nil
}

// --- test controls ---

// mineAll mines every pooled transaction into one new block, successfully.
func (c *sendChain) mineAll() uint64 { return c.mineBlock(types.ReceiptStatusSuccessful, nil) }

// mineReverted mines them into a block where they all reverted: the gas was
// spent, the transfer did not happen.
func (c *sendChain) mineReverted() uint64 { return c.mineBlock(types.ReceiptStatusFailed, nil) }

// mineOnly mines one named transaction and drops everything competing for its
// nonce. It settles a race, so it will mine a transaction this node's own
// mempool has already dropped: a replacement evicts the original here, but the
// original is still out in the world and another node may mine it. That is
// exactly the race cancel_nonce has to survive.
func (c *sendChain) mineOnly(h common.Hash) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx := c.pool[h]
	if tx == nil {
		for _, seen := range c.sent {
			if seen.Hash() == h {
				tx = seen
				break
			}
		}
	}
	if tx == nil {
		return c.head
	}
	c.head++
	c.record(tx)
	sender, nonce := c.senderOf(tx), tx.Nonce()
	for other, in := range c.pool {
		if c.senderOf(in) == sender && in.Nonce() == nonce {
			delete(c.pool, other)
		}
	}
	return c.head
}

func (c *sendChain) mineBlock(status uint64, only *common.Hash) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.head++
	for h, tx := range c.pool {
		if only != nil && *only != h {
			continue
		}
		if status == types.ReceiptStatusSuccessful {
			c.record(tx)
			continue
		}
		// A reverted transaction still burned its gas and still spent its
		// nonce; nothing else moved.
		price := new(big.Int).Add(c.baseFee, tx.GasTipCap())
		c.mined[h] = &types.Receipt{
			Status: status, TxHash: h, BlockNumber: new(big.Int).SetUint64(c.head),
			GasUsed: tx.Gas(), EffectiveGasPrice: price,
		}
		sender := c.senderOf(tx)
		if tx.Nonce()+1 > c.minedNonce[sender] {
			c.minedNonce[sender] = tx.Nonce() + 1
		}
		c.debitEther(sender, new(big.Int).Mul(new(big.Int).SetUint64(tx.Gas()), price))
		delete(c.pool, h)
	}
	return c.head
}

// record writes a successful receipt, moves the balances and advances the
// sender's nonce. Called with c.mu held.
func (c *sendChain) record(tx *types.Transaction) {
	price := new(big.Int).Add(c.baseFee, tx.GasTipCap())
	c.mined[tx.Hash()] = &types.Receipt{
		Status: types.ReceiptStatusSuccessful, TxHash: tx.Hash(),
		BlockNumber: new(big.Int).SetUint64(c.head),
		GasUsed:     tx.Gas(), EffectiveGasPrice: price,
	}
	sender := c.senderOf(tx)
	if tx.Nonce()+1 > c.minedNonce[sender] {
		c.minedNonce[sender] = tx.Nonce() + 1
	}
	c.settle(tx, sender, price)
	delete(c.pool, tx.Hash())
}

// settle applies a mined transaction's effects. Called with c.mu held.
//
// Gas is charged at gas limit x price rather than at what was used, because
// this chain has no execution to measure. That makes it strictly harsher than
// a real node, which is the safe direction for a sweeper that has to leave
// enough behind to pay.
func (c *sendChain) settle(tx *types.Transaction, sender common.Address, price *big.Int) {
	fee := new(big.Int).Mul(new(big.Int).SetUint64(tx.Gas()), price)
	c.debitEther(sender, new(big.Int).Add(tx.Value(), fee))
	if tx.To() != nil {
		c.creditEther(*tx.To(), tx.Value())
	}
	// An ERC-20 transfer: selector, recipient, amount.
	data := tx.Data()
	if tx.To() == nil || len(data) != 4+32+32 || common.Bytes2Hex(data[:4]) != "a9059cbb" {
		return
	}
	token := *tx.To()
	to := common.BytesToAddress(data[4+12 : 4+32])
	amount := new(big.Int).SetBytes(data[4+32:])
	held := c.tokenOf(token, sender)
	if held.Cmp(amount) < 0 {
		// A real token would revert. Mark the receipt failed rather than
		// invent balance, so the sweeper meets the failure it would really see.
		c.mined[tx.Hash()].Status = types.ReceiptStatusFailed
		return
	}
	c.setToken(token, sender, new(big.Int).Sub(held, amount))
	c.setToken(token, to, new(big.Int).Add(c.tokenOf(token, to), amount))
}

// balanceOf, tokenOf, setToken and the credit/debit helpers all assume c.mu.
func (c *sendChain) balanceOf(a common.Address) *big.Int {
	if v, ok := c.ether[a]; ok {
		return v
	}
	return new(big.Int)
}

func (c *sendChain) creditEther(a common.Address, v *big.Int) {
	c.ether[a] = new(big.Int).Add(c.balanceOf(a), v)
}

func (c *sendChain) debitEther(a common.Address, v *big.Int) {
	out := new(big.Int).Sub(c.balanceOf(a), v)
	if out.Sign() < 0 {
		out = new(big.Int)
	}
	c.ether[a] = out
}

func (c *sendChain) tokenOf(token, holder common.Address) *big.Int {
	if m, ok := c.tokens[token]; ok {
		if v, ok := m[holder]; ok {
			return v
		}
	}
	return new(big.Int)
}

func (c *sendChain) setToken(token, holder common.Address, v *big.Int) {
	if c.tokens[token] == nil {
		c.tokens[token] = map[common.Address]*big.Int{}
	}
	c.tokens[token][holder] = v
}

// fund gives an address ether out of thin air, the way a test faucet does.
func (c *sendChain) fund(a common.Address, wei *big.Int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creditEther(a, wei)
}

// fundToken does the same for an ERC-20.
func (c *sendChain) fundToken(token, holder common.Address, units *big.Int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setToken(token, holder, new(big.Int).Add(c.tokenOf(token, holder), units))
}

// advance moves the head on without mining anything, which is how a test buys
// confirmations.
func (c *sendChain) advance(blocks uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.head += blocks
}

// failTokenBalance makes every balanceOf on this contract fail, the way a
// registry row pointing at an address with no code does: eth_call answers with
// nothing, and reading that as zero would say "nothing to collect" when the
// truth is that we asked the wrong thing.
func (c *sendChain) failTokenBalance(token common.Address, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenErr[token] = err
}

// failNextSend makes the next broadcast fail, once.
func (c *sendChain) failNextSend(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendErr = err
}

// pretendForeignSends makes the chain report n transactions from this address
// that we never sent — someone else holding the same key.
func (c *sendChain) pretendForeignSends(a common.Address, n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.foreignNonce[a] += n
}

// pooled reports how many transactions are waiting to be mined.
func (c *sendChain) pooled() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pool)
}

// lastSent is the most recent transaction the node accepted.
func (c *sendChain) lastSent() *types.Transaction {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return nil
	}
	return c.sent[len(c.sent)-1]
}

// sentCount is how many transactions have reached the node in total.
func (c *sendChain) sentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// senderOf recovers the signer. Called with c.mu held.
func (c *sendChain) senderOf(tx *types.Transaction) common.Address {
	from, err := types.Sender(types.LatestSignerForChainID(big.NewInt(c.chainID)), tx)
	if err != nil {
		return common.Address{}
	}
	return from
}

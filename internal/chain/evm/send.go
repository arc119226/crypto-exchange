package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// transferSelector is the first four bytes of
// keccak256("transfer(address,uint256)") — 0xa9059cbb, the ERC-20 transfer
// entry point. Computed rather than written out, so it cannot drift from the
// signature it claims to be; scripts/e2e.sh asserts the literal value against
// this, which is what makes the derivation trustworthy.
var transferSelector = crypto.Keccak256([]byte("transfer(address,uint256)"))[:4]

// ErrKnownTransaction reports that the node already has this transaction, or
// one with the same nonce that it prefers. It is not a failure: the
// transaction is on its way, and the caller should treat it as broadcast
// rather than allocate a second nonce (docs/plan-v1.0.md §6.4.2).
var ErrKnownTransaction = errors.New("evm: transaction already known")

// ErrUnderpriced reports that a replacement was refused for not raising the
// fee enough. The caller must bump further, not give up.
var ErrUnderpriced = errors.New("evm: replacement transaction underpriced")

// ErrFeeCeiling reports that the fees a transaction would need are above
// ETH_MAX_FEE_PER_GAS. It is a decision, not a failure: the work is still
// there, it is simply not worth this price yet. Callers wrap it with the
// numbers and leave whatever they were about to send exactly as it was.
var ErrFeeCeiling = errors.New("evm: fees are above the configured ceiling")

// Fees are the EIP-1559 parameters of one transaction.
type Fees struct {
	// TipCap is the priority fee paid to the proposer.
	TipCap *big.Int
	// FeeCap is the most the sender will pay per gas, base fee included.
	FeeCap *big.Int
}

// SuggestFees reads the node's tip suggestion and the head's base fee, and
// builds a fee cap that survives a few blocks of base-fee growth.
//
// The cap is 2×baseFee + tip, the usual headroom: the base fee can rise at
// most 12.5% per block, so this covers roughly six blocks of continuous
// increase. Whatever is not spent is refunded — a fee cap is a ceiling, not a
// price — so erring high costs nothing but the balance that must be free.
func (c *Client) SuggestFees(ctx context.Context) (Fees, error) {
	tip, err := c.rpc.SuggestGasTipCap(ctx)
	if err != nil {
		return Fees{}, fmt.Errorf("evm: suggest tip: %w", err)
	}
	head, err := c.rpc.HeaderByNumber(ctx, nil)
	if err != nil {
		return Fees{}, fmt.Errorf("evm: head header: %w", err)
	}
	base := new(big.Int)
	if head.BaseFee != nil {
		base.Set(head.BaseFee)
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), tip)
	return Fees{TipCap: new(big.Int).Set(tip), FeeCap: feeCap}, nil
}

// Bump raises both fee components by the given percentage, at least the 10%
// the protocol requires before a node will accept a replacement (§6.4.2).
// Rounding is upward, because a replacement that misses the threshold by one
// wei is refused outright.
func (f Fees) Bump(percent int64) Fees {
	if percent < 10 {
		percent = 10
	}
	mul := func(v *big.Int) *big.Int {
		if v == nil {
			return new(big.Int)
		}
		n := new(big.Int).Mul(v, big.NewInt(100+percent))
		// +99 then divide by 100 rounds up.
		return n.Add(n, big.NewInt(99)).Div(n, big.NewInt(100))
	}
	return Fees{TipCap: mul(f.TipCap), FeeCap: mul(f.FeeCap)}
}

// Over reports whether these fees breach the operator's ceiling. A nil or
// non-positive limit means there is no ceiling.
//
// This used to be CapAt, which lowered FeeCap to the limit and let the caller
// send anyway. Both its own comment and ETH_MAX_FEE_PER_GAS's said a
// transaction over the ceiling "waits instead", and neither was true: what it
// actually did was send at a price the market had already left behind, which
// on a real fee market means the transaction sits in the mempool and the
// replacement ladder spends itself trying to fix it. Nothing noticed on anvil,
// where the base fee is zero and the ceiling is never reached.
//
// A stop-loss that quietly turns into a worse price is not a stop-loss. So the
// decision moves to the caller, who is the only one that knows what waiting
// means for the work in hand -- a withdrawal stays where it is, a sweep waits
// for the next tick, a nonce fill is simply not sent yet.
func (f Fees) Over(limit *big.Int) bool {
	if limit == nil || limit.Sign() <= 0 || f.FeeCap == nil {
		return false
	}
	return f.FeeCap.Cmp(limit) > 0
}

// PendingNonceAt is the nonce the node would give the next transaction from
// this address, counting what is in its mempool.
func (c *Client) PendingNonceAt(ctx context.Context, address common.Address) (uint64, error) {
	n, err := c.rpc.PendingNonceAt(ctx, address)
	if err != nil {
		return 0, fmt.Errorf("evm: pending nonce of %s: %w", address, err)
	}
	return n, nil
}

// NonceAt is the nonce as of the latest mined block, ignoring the mempool.
// NonceManager compares the two: what is pending but not mined is exactly the
// set of transactions still in flight (§6.4.2).
func (c *Client) NonceAt(ctx context.Context, address common.Address) (uint64, error) {
	n, err := c.rpc.NonceAt(ctx, address, nil)
	if err != nil {
		return 0, fmt.Errorf("evm: nonce of %s: %w", address, err)
	}
	return n, nil
}

// Balance is the address's native balance in wei, at the head.
func (c *Client) Balance(ctx context.Context, address common.Address) (*big.Int, error) {
	return c.BalanceAt(ctx, address, nil)
}

// BalanceAt is the address's native balance in wei at one block, or at the
// head when block is nil.
//
// Reconciliation needs the block: asking for thirty balances takes long enough
// that the chain moves underneath the answers, and a sweep mined halfway
// through would be counted as having left one address without having arrived
// at the other. Pinning them all to one height costs nothing and makes the set
// a snapshot rather than a sequence of moments.
func (c *Client) BalanceAt(ctx context.Context, address common.Address, block *big.Int) (*big.Int, error) {
	b, err := c.rpc.BalanceAt(ctx, address, block)
	if err != nil {
		return nil, fmt.Errorf("evm: balance of %s: %w", address, err)
	}
	return new(big.Int).Set(b), nil
}

// EstimateGas asks the node what the call would cost.
func (c *Client) EstimateGas(ctx context.Context, from, to common.Address, value *big.Int, data []byte) (uint64, error) {
	gas, err := c.rpc.EstimateGas(ctx, ethereum.CallMsg{
		From: from, To: &to, Value: new(big.Int).Set(value), Data: data,
	})
	if err != nil {
		return 0, fmt.Errorf("evm: estimate gas: %w", err)
	}
	return gas, nil
}

// SendRawTransaction broadcasts an already-signed transaction.
//
// Two node answers are not failures. "Already known" means this exact
// transaction is in the mempool, which is what a rebroadcast after a restart
// is supposed to produce. "Nonce too low" means it or a replacement is already
// mined. Both map to ErrKnownTransaction so the caller records a broadcast
// rather than allocating a second nonce for money that is already moving.
func (c *Client) SendRawTransaction(ctx context.Context, raw []byte) error {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return fmt.Errorf("evm: decode signed tx: %w", err)
	}
	err := c.rpc.SendTransaction(ctx, tx)
	if err == nil {
		return nil
	}
	switch msg := strings.ToLower(err.Error()); {
	case strings.Contains(msg, "already known"),
		strings.Contains(msg, "nonce too low"),
		strings.Contains(msg, "already imported"):
		return fmt.Errorf("%w: %s", ErrKnownTransaction, err)
	case strings.Contains(msg, "underpriced"), strings.Contains(msg, "replacement transaction"):
		return fmt.Errorf("%w: %s", ErrUnderpriced, err)
	}
	return fmt.Errorf("evm: send raw tx: %w", err)
}

// TransferCalldata builds the ERC-20 `transfer(address,uint256)` call: the
// four-byte selector, the recipient left-padded to 32 bytes, and the amount.
//
// Hand-packed rather than generated from an ABI: this is the only contract
// call the exchange makes in v1, and pulling abigen and go-ethereum's ABI
// machinery into the toolchain for two words would cost more than it saves.
// The shape is asserted against a real MockUSDC transfer in the anvil tests.
func TransferCalldata(to common.Address, amount *big.Int) ([]byte, error) {
	if amount == nil || amount.Sign() < 0 {
		return nil, errors.New("evm: transfer amount must not be negative")
	}
	if amount.BitLen() > 256 {
		return nil, fmt.Errorf("evm: transfer amount does not fit uint256")
	}
	data := make([]byte, 0, 4+32+32)
	data = append(data, transferSelector...)
	data = append(data, common.LeftPadBytes(to.Bytes(), 32)...)
	return append(data, common.LeftPadBytes(amount.Bytes(), 32)...), nil
}

// balanceOfSelector is keccak256("balanceOf(address)")[:4] — 0x70a08231.
// Derived for the same reason transferSelector is, and pinned by the same
// kind of test.
var balanceOfSelector = crypto.Keccak256([]byte("balanceOf(address)"))[:4]

// TokenBalance reads an ERC-20 balance with eth_call.
//
// The sweeper needs it and the deposit scanner does not: scanning follows
// Transfer logs, which say what moved, while sweeping has to know what is
// actually sitting there — including anything the logs did not account for.
func (c *Client) TokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error) {
	return c.TokenBalanceAt(ctx, token, holder, nil)
}

// TokenBalanceAt reads an ERC-20 balance at one block, or at the head when
// block is nil. Same reason as BalanceAt: one height for the whole set.
func (c *Client) TokenBalanceAt(ctx context.Context, token, holder common.Address, block *big.Int) (*big.Int, error) {
	data := make([]byte, 0, 4+32)
	data = append(data, balanceOfSelector...)
	data = append(data, common.LeftPadBytes(holder.Bytes(), 32)...)
	out, err := c.rpc.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, block)
	if err != nil {
		return nil, fmt.Errorf("evm: balanceOf %s on %s: %w", lower(holder), lower(token), err)
	}
	// A contract that is not a token, or an address with no code, answers with
	// empty data rather than an error. Reading that as zero would tell the
	// sweeper there is nothing to collect when the truth is that we asked the
	// wrong thing.
	if len(out) != 32 {
		return nil, fmt.Errorf("evm: balanceOf %s on %s returned %d bytes, not 32", lower(holder), lower(token), len(out))
	}
	return new(big.Int).SetBytes(out), nil
}

func lower(a common.Address) string { return strings.ToLower(a.Hex()) }

// Package signer is the only place in the system that touches a private key
// (docs/plan-v1.0.md §5.1, §6.6, ADR-0007).
//
// It runs as its own role in a split deployment. Everything else asks it to
// sign a *named intent* — "the transaction for withdrawal X, attempt 0" — and
// the signer decides what bytes that means. It never signs a transaction
// handed to it: it builds the transaction itself from fields it has checked
// against the database, which is a deliberate deviation from the §6.6 sketch
// (where the caller passes an unsigned types.Transaction). The reason is
// ERC-20: in a token withdrawal the transaction's `to` is the token contract
// and the real recipient is buried in the calldata, so verifying a
// pre-built transaction would mean decoding calldata and trusting the decode.
// Building it here removes that step entirely.
package signer

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Kind is what a signature is for. The signer applies a different check to
// each, and (kind, ref_id, attempt) is what chain.signing_log makes unique.
type Kind string

// Signing kinds (docs/plan-v1.0.md §6.6). Sweep and gas funding arrive with
// 4c; they are named here because the signing log's CHECK already accepts
// them and a partial enum would be more confusing than a complete one.
const (
	KindWithdrawal Kind = "withdrawal"
	KindSweep      Kind = "sweep"
	KindGasFund    Kind = "gas_fund"
	KindNonceFill  Kind = "nonce_fill"
)

// Errors the caller distinguishes.
var (
	// ErrRefused means the request failed the signer's own policy check: the
	// withdrawal is not in a state that may be signed, or it does not say what
	// the request claims. It is never retried — retrying a refusal is how a
	// bug becomes a second transaction.
	ErrRefused = errors.New("signer: refused")
	// ErrAlreadySigned means this (kind, ref_id, attempt) has a signature. The
	// caller must use the one already recorded rather than make another.
	ErrAlreadySigned = errors.New("signer: already signed")
	// ErrUnsupported is a kind this build cannot sign yet.
	ErrUnsupported = errors.New("signer: unsupported kind")
)

// Request is an intent to sign, not a transaction. Value is in the asset's own
// units; the signer converts to wei or token base units itself, because that
// conversion is exactly where a caller could lose or gain a factor of 10^18.
type Request struct {
	Kind  Kind
	RefID string
	// Attempt distinguishes a replacement from the original. It is part of
	// the signing log's unique key, so attempt 0 can only ever be signed once.
	Attempt int32
	ChainID int64
	// To is the ultimate recipient, even for a token transfer where the
	// transaction's own `to` will be the contract.
	To     common.Address
	Asset  string
	Value  money.Amount
	Nonce  uint64
	Gas    uint64
	TipCap *big.Int
	FeeCap *big.Int
}

// Result is what came back. RawTx is the RLP-encoded signed transaction, ready
// for eth_sendRawTransaction and for storing so a restart re-sends the same
// bytes rather than signing again.
type Result struct {
	RawTx  []byte
	TxHash string
	// From and Nonce are echoed so the caller can record what was actually
	// signed without re-deriving the hot wallet address.
	From  string
	Nonce uint64
}

// Signer signs one intent at a time.
type Signer interface {
	// Sign returns the signed transaction, or ErrRefused when the request does
	// not match what the database says.
	Sign(ctx context.Context, req Request) (Result, error)
	// HotWallet is the address the exchange sends withdrawals from.
	HotWallet(ctx context.Context) (common.Address, error)
}

func (k Kind) valid() bool {
	switch k {
	case KindWithdrawal, KindSweep, KindGasFund, KindNonceFill:
		return true
	}
	return false
}

// Validate rejects a request that is malformed before any database or key is
// touched. The signer calls it too: the transport checking first only saves a
// round trip, it is not what makes the request safe.
func (r Request) Validate() error {
	switch {
	case !r.Kind.valid():
		return fmt.Errorf("%w: kind %q", ErrRefused, r.Kind)
	case r.RefID == "":
		return fmt.Errorf("%w: a reference id is required", ErrRefused)
	case r.Attempt < 0:
		return fmt.Errorf("%w: attempt must not be negative", ErrRefused)
	case r.Gas == 0:
		return fmt.Errorf("%w: gas must be set", ErrRefused)
	case r.TipCap == nil || r.FeeCap == nil:
		return fmt.Errorf("%w: EIP-1559 fees are required", ErrRefused)
	case r.TipCap.Sign() < 0 || r.FeeCap.Sign() < 0:
		return fmt.Errorf("%w: fees must not be negative", ErrRefused)
	case r.TipCap.Cmp(r.FeeCap) > 0:
		return fmt.Errorf("%w: tip %s exceeds the fee cap %s", ErrRefused, r.TipCap, r.FeeCap)
	}
	return nil
}

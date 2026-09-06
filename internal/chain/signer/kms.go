package signer

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
)

// ErrNoKMS is what every KMSSigner method returns until one is implemented.
var ErrNoKMS = errors.New("signer: KMS signing is not implemented")

// KMSSigner is the seam for a key the process never holds: a cloud KMS, an
// HSM, or a hardware wallet (docs/plan-v1.0.md §6.6).
//
// It is deliberately a failing stub rather than an absent type. The Signer
// interface was designed around what a remote key can actually do — sign a
// named intent, and report its address — and keeping a second implementation
// compiled against it is what stops that interface from quietly growing a
// method only an in-memory key could satisfy.
type KMSSigner struct {
	// KeyRef names the key in the provider. It is kept so a deployment can be
	// configured, and rejected, before anyone tries to withdraw.
	KeyRef string
}

var _ Signer = (*KMSSigner)(nil)

// Sign implements Signer.
func (*KMSSigner) Sign(context.Context, Request) (Result, error) { return Result{}, ErrNoKMS }

// HotWallet implements Signer.
func (*KMSSigner) HotWallet(context.Context) (common.Address, error) {
	return common.Address{}, ErrNoKMS
}

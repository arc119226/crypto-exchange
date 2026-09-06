// Package evm is the boundary between the chain and the rest of the exchange
// (docs/plan-v1.0.md §6.5, §8): integers in wei on one side, money.Amount on
// the other, and a thin JSON-RPC client in between.
//
// Nothing above this package handles a *big.Int, and nothing in it handles a
// float — forbidigo enforces the second half.
package evm

import (
	"fmt"
	"math/big"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// MaxScale is the largest asset scale we accept. ETH is 18; anything larger
// would not survive NUMERIC(36,18) in the ledger anyway.
const MaxScale int32 = 18

// ToWei converts a decimal amount into the asset's smallest unit.
//
// It refuses an amount with more decimal places than the asset has, rather
// than rounding: silently dropping a digit here would mean crediting a
// different number than the chain moved.
func ToWei(amount money.Amount, scale int32) (*big.Int, error) {
	if scale < 0 || scale > MaxScale {
		return nil, fmt.Errorf("evm: scale %d out of range", scale)
	}
	if amount.IsNegative() {
		return nil, fmt.Errorf("evm: negative amount %s", amount)
	}
	// amount is coefficient x 10^exponent, so the value in the smallest unit
	// is coefficient x 10^(exponent+scale). A negative result means the amount
	// carries digits the asset cannot represent.
	shift := amount.Exponent() + scale
	if shift < 0 {
		return nil, fmt.Errorf("evm: %s has more precision than scale %d allows", amount, scale)
	}
	out := new(big.Int).Set(amount.Coefficient())
	if shift > 0 {
		out.Mul(out, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(shift)), nil))
	}
	return out, nil
}

// FromWei converts the asset's smallest unit into a decimal amount. The
// caller keeps ownership of wei; the value is copied before use.
func FromWei(wei *big.Int, scale int32) (money.Amount, error) {
	if scale < 0 || scale > MaxScale {
		return money.Amount{}, fmt.Errorf("evm: scale %d out of range", scale)
	}
	if wei == nil {
		return money.Amount{}, fmt.Errorf("evm: nil wei")
	}
	if wei.Sign() < 0 {
		return money.Amount{}, fmt.Errorf("evm: negative wei %s", wei)
	}
	return money.FromBigInt(new(big.Int).Set(wei), -scale)
}

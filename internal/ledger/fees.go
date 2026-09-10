package ledger

import (
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// FeeParams is what the ledger needs to charge a trade (docs/plan-v1.0.md
// §6.3): basis points for maker and taker and the scales to round to. The
// ledger does not import registry; trading converts the market's fee
// schedule into this struct.
type FeeParams struct {
	MakerBps   int32
	TakerBps   int32
	BaseScale  int32
	QuoteScale int32
}

// Validate checks bps and scales.
func (f FeeParams) Validate() error {
	if f.MakerBps < 0 || f.MakerBps > 10000 || f.TakerBps < 0 || f.TakerBps > 10000 {
		return fmt.Errorf("%w: bps out of range", ErrInvalidSettlement)
	}
	if f.BaseScale < 0 || f.BaseScale > money.MaxScale || f.QuoteScale < 0 || f.QuoteScale > money.MaxScale {
		return fmt.Errorf("%w: scale out of range", ErrInvalidSettlement)
	}
	return nil
}

// ComputeFee returns ceil(amount × bps / 10000) at the given scale: rounding
// is always in the exchange's favour (docs/plan-v1.0.md §6.5), and the same
// number appears on the payer's and on fee_revenue's posting, so conservation
// is exact regardless of rounding.
func ComputeFee(amount money.Amount, bps int32, scale int32) (money.Amount, error) {
	if bps == 0 || amount.IsZero() {
		return money.Zero, nil
	}
	if bps < 0 || amount.IsNegative() {
		return money.Zero, fmt.Errorf("%w: negative fee input", ErrInvalidSettlement)
	}
	// money.DivRoundUp is this rounding: the remainder has to be detected at
	// the division, because dividing first and rounding after loses the digits
	// the ceiling would look at. The withdrawal and deposit fees in
	// internal/registry go through the same primitive, so the three fee paths
	// cannot round differently from each other.
	return amount.Mul(money.FromInt64(int64(bps))).DivRoundUp(money.FromInt64(10000), scale)
}

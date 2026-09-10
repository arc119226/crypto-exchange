package registry

import (
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// bpsDivisor is one hundred percent in basis points.
const bpsDivisor = 10000

// WithdrawalFeeFor returns what a withdrawal of amount is charged (§23.3):
//
//	fee = withdrawal_fee + ceil(amount × withdrawal_fee_bps / 10000)
//
// The fee is charged on top of the amount, not taken out of it. The user says
// how much should arrive at the destination, and pays amount + fee -- so the
// flat part can be priced to cover gas without the recipient ever receiving
// less than the number they typed.
//
// Rounding is up, at the asset's own scale, because §6.5 rounds in the
// exchange's favour and a fee is money owed to the exchange. Up at the asset
// scale also guarantees the result is representable: a fee with more decimals
// than the asset has could not be posted to the ledger, and rounding it later
// would make the amount charged differ from the amount snapshotted.
//
// It returns an error rather than a zero fee when amount is not positive: a
// caller asking what to charge for a non-withdrawal is confused, and quietly
// answering zero would hide that.
func (a Asset) WithdrawalFeeFor(amount money.Amount) (money.Amount, error) {
	if !amount.IsPositive() {
		return money.Zero, fmt.Errorf("%w: withdrawal fee of a non-positive amount %s", ErrInvalid, amount)
	}
	proportional, err := a.proportional(amount, a.WithdrawalFeeBps)
	if err != nil {
		return money.Zero, err
	}
	// The flat part is added after the rounding, so the total is only
	// representable if the flat part is too. AssetInput.Validate refuses a
	// flat fee with more decimals than the asset has, which is what makes
	// that true rather than hoped for.
	return a.WithdrawalFee.Add(proportional), nil
}

// DepositFeeFor returns what a deposit of amount is charged (§23.4):
//
//	fee = ceil(amount × deposit_fee_bps / 10000)
//
// Unlike a withdrawal fee this one comes out of what arrived: the chain
// delivered amount, the user is credited amount − fee, and the difference is
// revenue. There is no flat part -- a flat deposit fee would make small
// deposits arbitrarily expensive, and the cost a deposit actually imposes
// (funding gas before an ERC-20 sweep) is bounded by min_deposit and
// sweep_threshold instead.
//
// The seed sets this to 0 for every asset and the plan expects it to stay
// there (§23.4): a deposit is the inlet, and charging at the inlet turns
// money away at the door. The column exists so an operator can respond if
// sweep gas ever exceeds what trading brings in.
//
// It refuses to return a fee that would consume the whole deposit. Rounding
// up means a small enough amount can round to the full amount even at a low
// rate -- one wei at 1 bp rounds up to one wei -- and crediting a user zero
// while booking their entire deposit as revenue is not a fee, it is
// confiscation. The caller decides what to do with such a deposit; refusing
// here means it cannot happen silently.
func (a Asset) DepositFeeFor(amount money.Amount) (money.Amount, error) {
	if !amount.IsPositive() {
		return money.Zero, fmt.Errorf("%w: deposit fee of a non-positive amount %s", ErrInvalid, amount)
	}
	fee, err := a.proportional(amount, a.DepositFeeBps)
	if err != nil {
		return money.Zero, err
	}
	if fee.Cmp(amount) >= 0 {
		return money.Zero, fmt.Errorf("%w: asset %s deposit fee %s would leave nothing of a %s deposit",
			ErrInvalid, a.Symbol, fee, amount)
	}
	return fee, nil
}

// proportional is ceil(amount × bps / 10000) at the asset's scale, and
// exactly zero when the rate is zero -- which matters more than it looks:
// every asset ships at 0 bps, so this is the path every deposit and every
// withdrawal takes until an operator changes a rate, and it must not turn a
// representable amount into a rounded one on the way past.
//
// The ceiling has to happen inside the division. Dividing first and rounding
// after does not work: a division truncates at the scale it is given, so the
// digits the ceiling would have looked at are gone before it runs. That is not
// hypothetical -- it is the bug this function shipped with. Dividing at scale
// 18 and then rounding up at an 18-decimal asset's scale made the rounding a
// no-op, so ETH fees were floored rather than ceiled and a small enough
// withdrawal was charged nothing at all. DivRoundUp detects the remainder at
// the division, which is the only place it still exists.
func (a Asset) proportional(amount money.Amount, bps int32) (money.Amount, error) {
	if bps == 0 {
		return money.Zero, nil
	}
	fee, err := amount.Mul(money.FromInt64(int64(bps))).DivRoundUp(money.FromInt64(bpsDivisor), a.Scale)
	if err != nil {
		return money.Zero, fmt.Errorf("%w: %s fee of %s: %w", ErrInvalid, a.Symbol, amount, err)
	}
	return fee, nil
}

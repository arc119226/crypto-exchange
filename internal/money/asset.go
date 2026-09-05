package money

import "fmt"

// Asset identifies a currency and its scale (number of fractional digits
// used on the ledger; ETH = 18, USDC = 6).
type Asset struct {
	Symbol string
	Scale  int32
}

// Validate checks the symbol is non-empty and the scale is within
// [0, MaxScale].
func (as Asset) Validate() error {
	if as.Symbol == "" {
		return fmt.Errorf("%w: empty symbol", ErrInvalidAsset)
	}
	if as.Scale < 0 || as.Scale > MaxScale {
		return fmt.Errorf("%w: %s scale %d out of range [0,%d]", ErrInvalidAsset, as.Symbol, as.Scale, MaxScale)
	}
	return nil
}

// RoundUp rounds towards +infinity to the asset scale.
func (as Asset) RoundUp(a Amount) Amount { return a.RoundUp(as.Scale) }

// RoundDown rounds towards −infinity to the asset scale.
func (as Asset) RoundDown(a Amount) Amount { return a.RoundDown(as.Scale) }

// Truncate rounds towards zero to the asset scale.
func (as Asset) Truncate(a Amount) Amount { return a.Truncate(as.Scale) }

// Fits reports whether a has no more fractional digits than the asset scale.
func (as Asset) Fits(a Amount) bool { return a.Scale() <= as.Scale }

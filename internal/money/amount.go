package money

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

const (
	// MaxIntegerDigits is the maximum number of significant integer digits
	// an Amount may carry (NUMERIC(36,18) → 36 − 18).
	MaxIntegerDigits = 18
	// MaxScale is the maximum number of fractional digits.
	MaxScale = 18
)

var amountPattern = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// Amount is an exact, immutable decimal number. The zero value is 0.
type Amount struct {
	d decimal.Decimal
}

// Zero is the Amount 0.
var Zero = Amount{}

// ParseAmount parses the canonical decimal form: an optional leading minus,
// one or more digits, optionally a dot followed by one or more digits.
// Exponents, leading plus, ".5", "5." and whitespace are rejected.
func ParseAmount(s string) (Amount, error) {
	if !amountPattern.MatchString(s) {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %q: %w", ErrInvalidAmount, s, err)
	}
	a := Amount{d: d}
	if err := a.Validate(); err != nil {
		return Amount{}, fmt.Errorf("%w: %q", err, s)
	}
	return a, nil
}

// MustParse is ParseAmount that panics on error. Use only for constants.
func MustParse(s string) Amount {
	a, err := ParseAmount(s)
	if err != nil {
		panic(err)
	}
	return a
}

// FromInt64 converts an integer.
func FromInt64(v int64) Amount {
	return Amount{d: decimal.NewFromInt(v)}
}

// FromBigInt builds coefficient × 10^exp. It is the bridge used by the
// Postgres NUMERIC helper and by the chain boundary (wei conversion). coef is
// copied; the caller keeps ownership. The result must fit NUMERIC(36,18).
func FromBigInt(coef *big.Int, exp int32) (Amount, error) {
	if coef == nil {
		return Amount{}, fmt.Errorf("%w: nil coefficient", ErrInvalidAmount)
	}
	a := Amount{d: decimal.NewFromBigInt(new(big.Int).Set(coef), exp)}
	if err := a.Validate(); err != nil {
		return Amount{}, err
	}
	return a, nil
}

// Validate reports ErrPrecision if the value does not fit NUMERIC(36,18).
func (a Amount) Validate() error {
	s := a.d.Abs().String()
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "0" {
		intPart = ""
	}
	if len(intPart) > MaxIntegerDigits || len(fracPart) > MaxScale {
		return ErrPrecision
	}
	return nil
}

// Add returns a + b.
func (a Amount) Add(b Amount) Amount { return Amount{d: a.d.Add(b.d)} }

// Sub returns a − b.
func (a Amount) Sub(b Amount) Amount { return Amount{d: a.d.Sub(b.d)} }

// Mul returns a × b (exact).
func (a Amount) Mul(b Amount) Amount { return Amount{d: a.d.Mul(b.d)} }

// Neg returns −a.
func (a Amount) Neg() Amount { return Amount{d: a.d.Neg()} }

// Abs returns |a|.
func (a Amount) Abs() Amount { return Amount{d: a.d.Abs()} }

// Cmp returns −1, 0 or +1.
func (a Amount) Cmp(b Amount) int { return a.d.Cmp(b.d) }

// Equal reports numeric equality (1.10 == 1.1).
func (a Amount) Equal(b Amount) bool { return a.d.Equal(b.d) }

// Sign returns −1, 0 or +1.
func (a Amount) Sign() int { return a.d.Sign() }

// IsZero reports a == 0.
func (a Amount) IsZero() bool { return a.d.IsZero() }

// IsNegative reports a < 0.
func (a Amount) IsNegative() bool { return a.d.IsNegative() }

// IsPositive reports a > 0.
func (a Amount) IsPositive() bool { return a.d.IsPositive() }

// RoundUp rounds towards +infinity to the given number of fractional digits.
// This is the rounding used for fees: it always favours the exchange.
func (a Amount) RoundUp(scale int32) Amount { return Amount{d: a.d.RoundCeil(scale)} }

// RoundDown rounds towards −infinity to the given number of fractional digits.
func (a Amount) RoundDown(scale int32) Amount { return Amount{d: a.d.RoundFloor(scale)} }

// Truncate rounds towards zero to the given number of fractional digits.
func (a Amount) Truncate(scale int32) Amount { return Amount{d: a.d.Truncate(scale)} }

// DivRoundDown returns a/b truncated toward zero to scale decimal places
// (0 <= scale <= 18). It never rounds away from zero, so converting a quote
// budget into a base quantity (market buys) can never over-buy, and dividing
// by a step with scale 0 yields the whole number of steps. It returns
// ErrDivisionByZero when b is zero.
func (a Amount) DivRoundDown(b Amount, scale int32) (Amount, error) {
	if b.IsZero() {
		return Amount{}, ErrDivisionByZero
	}
	if scale < 0 || scale > MaxScale {
		return Amount{}, fmt.Errorf("%w: scale %d out of range", ErrPrecision, scale)
	}
	q, _ := a.d.QuoRem(b.d, scale)
	return Amount{d: q}, nil
}

// DivRoundUp returns a/b rounded AWAY from zero to scale decimal places: the
// smallest multiple of 10^-scale that is at least a/b.
//
// It exists because rounding up cannot be done by dividing and then rounding.
// DivRoundDown truncates at the scale it is given, so a quotient that needs
// more digits than that has already lost them by the time a ceiling could be
// applied, and the ceiling silently becomes a no-op. Dividing at a wider scale
// is not available either -- 18 is the widest there is. So the remainder has
// to be detected at the division itself, which is what this does: divide down,
// multiply back, and add one unit if anything was lost.
//
// This is the rounding every fee uses, because rounding a fee up is what
// rounding in the exchange's favour means (docs/plan-v1.0.md §6.5).
//
// It refuses a negative dividend rather than guessing: away from zero and up
// are the same direction only for non-negative values, and every caller here
// is charging a fee.
func (a Amount) DivRoundUp(b Amount, scale int32) (Amount, error) {
	if a.IsNegative() {
		return Amount{}, fmt.Errorf("%w: rounding up a negative quotient is ambiguous", ErrPrecision)
	}
	if !b.IsPositive() {
		if b.IsZero() {
			return Amount{}, ErrDivisionByZero
		}
		return Amount{}, fmt.Errorf("%w: rounding up by a negative divisor is ambiguous", ErrPrecision)
	}
	q, err := a.DivRoundDown(b, scale)
	if err != nil {
		return Amount{}, err
	}
	if q.Mul(b).Equal(a) {
		return q, nil // exact: nothing was lost, so there is nothing to round
	}
	unit, err := FromBigInt(big.NewInt(1), -scale)
	if err != nil {
		return Amount{}, err
	}
	return q.Add(unit), nil
}

// IsMultipleOf reports whether a is an integer multiple of step. A step that
// is not strictly positive yields false.
func (a Amount) IsMultipleOf(step Amount) bool {
	if !step.IsPositive() {
		return false
	}
	return a.d.Mod(step.d).IsZero()
}

// Scale returns the number of fractional digits in the canonical form
// ("1.10" → 1, "5" → 0).
func (a Amount) Scale() int32 {
	s := a.d.String()
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return int32(len(s) - i - 1) //nolint:gosec // fractional digit count of a decimal string, far below int32 range
	}
	return 0
}

// Coefficient returns a copy of the unscaled integer such that
// a = Coefficient × 10^Exponent.
func (a Amount) Coefficient() *big.Int { return a.d.Coefficient() }

// Exponent returns the base-10 exponent paired with Coefficient.
func (a Amount) Exponent() int32 { return a.d.Exponent() }

// String returns the canonical form: fixed point, no exponent, no trailing
// zeros, "-0" normalised to "0".
func (a Amount) String() string { return a.d.String() }

// StringFixed returns the value with exactly scale fractional digits
// (rounded half away from zero), for display purposes only.
func (a Amount) StringFixed(scale int32) string { return a.d.StringFixed(scale) }

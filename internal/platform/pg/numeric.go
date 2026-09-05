package pg

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// ErrNullNumeric is returned when a NULL NUMERIC is converted to a
// non-nullable Amount.
var ErrNullNumeric = errors.New("pg: numeric is NULL")

// NumericFromAmount converts an Amount to the pgx NUMERIC representation.
func NumericFromAmount(a money.Amount) pgtype.Numeric {
	return pgtype.Numeric{Int: a.Coefficient(), Exp: a.Exponent(), Valid: true}
}

// AmountFromNumeric converts a pgx NUMERIC to an Amount. Postgres frequently
// returns values with Exp != 0 (1990.00 arrives as Int=199000, Exp=-2 and
// 100 may arrive as Int=1, Exp=2), which decimal handles exactly; never
// assume Exp == 0.
func AmountFromNumeric(n pgtype.Numeric) (money.Amount, error) {
	if !n.Valid {
		return money.Zero, ErrNullNumeric
	}
	if n.NaN {
		return money.Zero, fmt.Errorf("pg: numeric is NaN")
	}
	if n.InfinityModifier != pgtype.Finite {
		return money.Zero, fmt.Errorf("pg: numeric is infinite")
	}
	coef := n.Int
	if coef == nil {
		coef = new(big.Int)
	}
	return money.FromBigInt(coef, n.Exp)
}

// NullableAmountFromNumeric maps NULL to nil.
func NullableAmountFromNumeric(n pgtype.Numeric) (*money.Amount, error) {
	if !n.Valid {
		return nil, nil //nolint:nilnil // NULL is a legitimate "no value"
	}
	a, err := AmountFromNumeric(n)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// NullableNumericFromAmount maps nil to NULL.
func NullableNumericFromAmount(a *money.Amount) pgtype.Numeric {
	if a == nil {
		return pgtype.Numeric{}
	}
	return NumericFromAmount(*a)
}

package pg

import (
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/money"
)

func TestAmountFromNumeric(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   pgtype.Numeric
		want string
	}{
		{"exp zero", pgtype.Numeric{Int: big.NewInt(1990), Exp: 0, Valid: true}, "1990"},
		{"positive exp", pgtype.Numeric{Int: big.NewInt(1), Exp: 2, Valid: true}, "100"},
		{"negative exp", pgtype.Numeric{Int: big.NewInt(199000), Exp: -2, Valid: true}, "1990"},
		{"one wei", pgtype.Numeric{Int: big.NewInt(1), Exp: -18, Valid: true}, "0.000000000000000001"},
		{"negative", pgtype.Numeric{Int: big.NewInt(-5), Exp: -1, Valid: true}, "-0.5"},
		{"zero nil int", pgtype.Numeric{Valid: true}, "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := AmountFromNumeric(c.in)
			require.NoError(t, err)
			assert.Equal(t, c.want, got.String())
		})
	}
	_, err := AmountFromNumeric(pgtype.Numeric{})
	assert.ErrorIs(t, err, ErrNullNumeric)
	_, err = AmountFromNumeric(pgtype.Numeric{NaN: true, Valid: true})
	assert.Error(t, err)
	_, err = AmountFromNumeric(pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true})
	assert.Error(t, err)
	max256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	_, err = AmountFromNumeric(pgtype.Numeric{Int: max256, Valid: true})
	assert.ErrorIs(t, err, money.ErrPrecision)
}

func TestNumericRoundTrip(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"0", "1", "-1", "1990.5", "0.000000000000000001", "123456789012345678.123456789012345678"} {
		a := money.MustParse(s)
		back, err := AmountFromNumeric(NumericFromAmount(a))
		require.NoError(t, err, s)
		assert.True(t, a.Equal(back), "%s -> %s", s, back)
	}
}

func TestNullable(t *testing.T) {
	t.Parallel()
	got, err := NullableAmountFromNumeric(pgtype.Numeric{})
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = NullableAmountFromNumeric(pgtype.Numeric{Int: big.NewInt(7), Valid: true})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "7", got.String())
	assert.False(t, NullableNumericFromAmount(nil).Valid)
	a := money.MustParse("2.5")
	assert.True(t, NullableNumericFromAmount(&a).Valid)
	_, err = NullableAmountFromNumeric(pgtype.Numeric{NaN: true, Valid: true})
	assert.Error(t, err)
}

package money

import (
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAmount(t *testing.T) {
	t.Parallel()
	valid := map[string]string{ // input → canonical
		"0":                    "0",
		"-0":                   "0",
		"1":                    "1",
		"1.10":                 "1.1",
		"-0.5":                 "-0.5",
		"0.000000000000000001": "0.000000000000000001", // 1 wei
		"123456789012345678":   "123456789012345678",   // 18 integer digits
		"1990.00":              "1990",
		"000123.4500":          "123.45",
	}
	for in, want := range valid {
		a, err := ParseAmount(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, a.String(), in)
	}
	invalid := map[string]error{
		"":                       ErrInvalidAmount,
		" 1":                     ErrInvalidAmount,
		"+1":                     ErrInvalidAmount,
		".5":                     ErrInvalidAmount,
		"5.":                     ErrInvalidAmount,
		"1e5":                    ErrInvalidAmount,
		"1,000":                  ErrInvalidAmount,
		"NaN":                    ErrInvalidAmount,
		"0x10":                   ErrInvalidAmount,
		"1234567890123456789":    ErrPrecision, // 19 integer digits
		"0.0000000000000000001":  ErrPrecision, // 19 fractional digits
		"-1234567890123456789.5": ErrPrecision,
	}
	for in, want := range invalid {
		_, err := ParseAmount(in)
		require.Error(t, err, in)
		assert.True(t, errors.Is(err, want), "%q: got %v want %v", in, err, want)
	}
}

func TestMustParsePanics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { MustParse("abc") })
	assert.Equal(t, "2.5", MustParse("2.50").String())
}

func TestFromBigInt(t *testing.T) {
	t.Parallel()
	one := big.NewInt(1)
	a, err := FromBigInt(one, 0)
	require.NoError(t, err)
	assert.Equal(t, "1", a.String())

	a, err = FromBigInt(big.NewInt(199000), -2) // Postgres binary form of 1990.00
	require.NoError(t, err)
	assert.Equal(t, "1990", a.String())

	a, err = FromBigInt(big.NewInt(1), 2)
	require.NoError(t, err)
	assert.Equal(t, "100", a.String())

	a, err = FromBigInt(big.NewInt(1), -18)
	require.NoError(t, err)
	assert.Equal(t, "0.000000000000000001", a.String())

	// caller keeps ownership of coef
	coef := big.NewInt(42)
	a, err = FromBigInt(coef, 0)
	require.NoError(t, err)
	coef.SetInt64(7)
	assert.Equal(t, "42", a.String())

	// 2^256-1 does not fit NUMERIC(36,18)
	max256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	_, err = FromBigInt(max256, 0)
	assert.ErrorIs(t, err, ErrPrecision)
	_, err = FromBigInt(max256, -18)
	assert.ErrorIs(t, err, ErrPrecision)

	_, err = FromBigInt(nil, 0)
	assert.ErrorIs(t, err, ErrInvalidAmount)
}

func TestArithmeticAndComparison(t *testing.T) {
	t.Parallel()
	a, b := MustParse("1990.00"), MustParse("0.4")
	assert.Equal(t, "1990.4", a.Add(b).String())
	assert.Equal(t, "1989.6", a.Sub(b).String())
	assert.Equal(t, "796", a.Mul(b).String())
	assert.Equal(t, "-0.4", b.Neg().String())
	assert.Equal(t, "0.4", b.Neg().Abs().String())
	assert.Equal(t, 1, a.Cmp(b))
	assert.Equal(t, -1, b.Cmp(a))
	assert.Equal(t, 0, a.Cmp(MustParse("1990")))
	assert.True(t, MustParse("1.10").Equal(MustParse("1.1")))
	assert.False(t, a.Equal(b))
	assert.Equal(t, 1, a.Sign())
	assert.Equal(t, -1, b.Neg().Sign())
	assert.Equal(t, 0, Zero.Sign())
	assert.True(t, Zero.IsZero())
	assert.False(t, a.IsZero())
	assert.True(t, b.Neg().IsNegative())
	assert.False(t, b.IsNegative())
	assert.True(t, b.IsPositive())
	assert.False(t, Zero.IsPositive())
	assert.Equal(t, int64(3), FromInt64(3).Coefficient().Int64())
	assert.Equal(t, int32(0), FromInt64(3).Exponent())
}

func TestRounding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in                 string
		scale              int32
		up, down, truncate string
	}{
		{"0.0008", 6, "0.0008", "0.0008", "0.0008"},
		{"0.0008101", 6, "0.000811", "0.00081", "0.00081"},
		{"0.7961", 2, "0.8", "0.79", "0.79"},
		{"-0.7961", 2, "-0.79", "-0.8", "-0.79"},
		{"1990", 2, "1990", "1990", "1990"},
		{"2.5", 0, "3", "2", "2"},
		{"-2.5", 0, "-2", "-3", "-2"},
		{"0.000000000000000001", 6, "0.000001", "0", "0"},
		{"-0.000000000000000001", 6, "0", "-0.000001", "0"},
	}
	for _, c := range cases {
		a := MustParse(c.in)
		assert.Equal(t, c.up, a.RoundUp(c.scale).String(), "RoundUp %s", c.in)
		assert.Equal(t, c.down, a.RoundDown(c.scale).String(), "RoundDown %s", c.in)
		assert.Equal(t, c.truncate, a.Truncate(c.scale).String(), "Truncate %s", c.in)
	}
	// fee example from plan §6.1.4: 796 × 10 bps, ceil to 6 places
	fee := MustParse("796").Mul(MustParse("0.001")).RoundUp(6)
	assert.Equal(t, "0.796", fee.String())
	assert.Equal(t, "0.796000", fee.StringFixed(6))
}

func TestIsMultipleOf(t *testing.T) {
	t.Parallel()
	tick, step := MustParse("0.01"), MustParse("0.0001")
	assert.True(t, MustParse("1990.00").IsMultipleOf(tick))
	assert.True(t, MustParse("1990.01").IsMultipleOf(tick))
	assert.False(t, MustParse("1990.005").IsMultipleOf(tick))
	assert.True(t, MustParse("0.4000").IsMultipleOf(step))
	assert.False(t, MustParse("0.40005").IsMultipleOf(step))
	assert.True(t, Zero.IsMultipleOf(step))
	assert.True(t, MustParse("-0.0002").IsMultipleOf(step))
	assert.False(t, MustParse("1").IsMultipleOf(Zero))
	assert.False(t, MustParse("1").IsMultipleOf(MustParse("-0.5")))
}

func TestScale(t *testing.T) {
	t.Parallel()
	assert.Equal(t, int32(0), MustParse("5").Scale())
	assert.Equal(t, int32(0), MustParse("5.000").Scale())
	assert.Equal(t, int32(1), MustParse("1.10").Scale())
	assert.Equal(t, int32(18), MustParse("0.000000000000000001").Scale())
	assert.Equal(t, int32(0), Zero.Scale())
}

func TestJSON(t *testing.T) {
	t.Parallel()
	type payload struct {
		Price Amount  `json:"price"`
		Max   *Amount `json:"max,omitempty"`
	}
	out, err := json.Marshal(payload{Price: MustParse("1990.50")})
	require.NoError(t, err)
	assert.JSONEq(t, `{"price":"1990.5"}`, string(out))

	var in payload
	require.NoError(t, json.Unmarshal([]byte(`{"price":"0.01","max":"5"}`), &in))
	assert.Equal(t, "0.01", in.Price.String())
	require.NotNil(t, in.Max)
	assert.Equal(t, "5", in.Max.String())

	err = json.Unmarshal([]byte(`{"price":1990.5}`), &in)
	assert.ErrorIs(t, err, ErrInvalidAmount, "JSON numbers must be rejected")
	err = json.Unmarshal([]byte(`{"price":"1e3"}`), &in)
	assert.ErrorIs(t, err, ErrInvalidAmount)
	err = json.Unmarshal([]byte(`{"price":null}`), &in)
	assert.ErrorIs(t, err, ErrInvalidAmount)

	txt, err := MustParse("7.70").MarshalText()
	require.NoError(t, err)
	assert.Equal(t, "7.7", string(txt))
	var a Amount
	require.NoError(t, a.UnmarshalText([]byte("0.5")))
	assert.Equal(t, "0.5", a.String())
	assert.Error(t, a.UnmarshalText([]byte("x")))
}

func TestAsset(t *testing.T) {
	t.Parallel()
	eth := Asset{Symbol: "ETH", Scale: 18}
	usdc := Asset{Symbol: "USDC", Scale: 6}
	require.NoError(t, eth.Validate())
	require.NoError(t, usdc.Validate())
	assert.ErrorIs(t, Asset{Symbol: "", Scale: 6}.Validate(), ErrInvalidAsset)
	assert.ErrorIs(t, Asset{Symbol: "X", Scale: 19}.Validate(), ErrInvalidAsset)
	assert.ErrorIs(t, Asset{Symbol: "X", Scale: -1}.Validate(), ErrInvalidAsset)

	v := MustParse("0.7961234")
	assert.Equal(t, "0.796124", usdc.RoundUp(v).String())
	assert.Equal(t, "0.796123", usdc.RoundDown(v).String())
	assert.Equal(t, "0.796123", usdc.Truncate(v).String())
	assert.False(t, usdc.Fits(v))
	assert.True(t, usdc.Fits(MustParse("0.796123")))
	assert.True(t, eth.Fits(v))
}

func TestDivRoundDown(t *testing.T) {
	cases := []struct {
		a, b  string
		scale int32
		want  string
	}{
		{"1000", "1990", 4, "0.5025"},          // 0.50251... floors, never rounds up
		{"403", "1995", 4, "0.2020"},           // 0.202005...
		{"403", "1995", 0, "0"},                // whole steps
		{"0.5025", "0.0001", 0, "5025"},        // count of steps
		{"1990", "10000", 18, "0.199"},         // exact division keeps exact value
		{"796", "0.4", 2, "1990"},              // exact
		{"7", "3", 3, "2.333"},                 // truncation, not rounding (2.3333...)
		{"-7", "3", 3, "-2.333"},               // toward zero for negatives
		{"1", "3", 18, "0.333333333333333333"}, // max scale
		{"0", "5", 6, "0"},
	}
	for _, c := range cases {
		got, err := MustParse(c.a).DivRoundDown(MustParse(c.b), c.scale)
		require.NoError(t, err, "%s/%s", c.a, c.b)
		assert.True(t, got.Equal(MustParse(c.want)), "%s/%s @%d = %s, want %s", c.a, c.b, c.scale, got, c.want)
		assert.LessOrEqual(t, got.Scale(), c.scale)
	}
	_, err := MustParse("1").DivRoundDown(Zero, 2)
	assert.ErrorIs(t, err, ErrDivisionByZero)
	_, err = MustParse("1").DivRoundDown(MustParse("3"), 19)
	assert.ErrorIs(t, err, ErrPrecision)
	_, err = MustParse("1").DivRoundDown(MustParse("3"), -1)
	assert.ErrorIs(t, err, ErrPrecision)
}

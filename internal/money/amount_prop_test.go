package money

import (
	"math/big"
	"testing"

	"pgregory.net/rapid"
)

// genAmount draws an Amount within NUMERIC(36,18): up to 18 integer digits
// and up to 18 fractional digits.
func genAmount(t *rapid.T) Amount {
	intDigits := rapid.IntRange(0, 18).Draw(t, "intDigits")
	fracDigits := rapid.IntRange(0, 18).Draw(t, "fracDigits")
	digits := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + rapid.IntRange(0, 9).Draw(t, "d"))
		}
		return string(b)
	}
	s := digits(intDigits)
	if s == "" {
		s = "0"
	}
	if fracDigits > 0 {
		s += "." + digits(fracDigits)
	}
	if rapid.Bool().Draw(t, "neg") {
		s = "-" + s
	}
	return MustParse(s)
}

func TestPropParseStringRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a := genAmount(rt)
		b, err := ParseAmount(a.String())
		if err != nil {
			rt.Fatalf("re-parse %q: %v", a.String(), err)
		}
		if !a.Equal(b) {
			rt.Fatalf("%s != %s", a, b)
		}
	})
}

func TestPropAddSubInverse(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a, b := genAmount(rt), genAmount(rt)
		if !a.Add(b).Sub(b).Equal(a) {
			rt.Fatalf("(%s + %s) - %s != %s", a, b, b, a)
		}
		if a.Cmp(b) != -b.Cmp(a) {
			rt.Fatalf("Cmp not antisymmetric for %s, %s", a, b)
		}
	})
}

func TestPropMultipleOfStep(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		stepScale := rapid.IntRange(0, 8).Draw(rt, "stepScale")
		step, _ := FromBigInt(big.NewInt(1), int32(-stepScale))
		k := rapid.Int64Range(-1_000_000, 1_000_000).Draw(rt, "k")
		v := step.Mul(FromInt64(k))
		if !v.IsMultipleOf(step) {
			rt.Fatalf("%s should be a multiple of %s", v, step)
		}
		if k != 0 {
			half := step.Mul(MustParse("0.5"))
			if v.Add(half).IsMultipleOf(step) {
				rt.Fatalf("%s + half step should not be a multiple of %s", v, step)
			}
		}
	})
}

func TestPropRoundingBounds(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		a := genAmount(rt)
		n := int32(rapid.IntRange(0, 18).Draw(rt, "scale"))
		up, down, tr := a.RoundUp(n), a.RoundDown(n), a.Truncate(n)
		if up.Cmp(a) < 0 || down.Cmp(a) > 0 {
			rt.Fatalf("RoundUp/RoundDown bounds violated for %s @%d: up=%s down=%s", a, n, up, down)
		}
		if tr.Abs().Cmp(a.Abs()) > 0 {
			rt.Fatalf("Truncate grew magnitude for %s @%d: %s", a, n, tr)
		}
		for _, r := range []Amount{up, down, tr} {
			if r.Scale() > n {
				rt.Fatalf("scale %d > %d after rounding %s", r.Scale(), n, a)
			}
		}
	})
}

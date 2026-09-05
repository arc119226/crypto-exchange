// Package money provides the exact decimal Amount type used for every price,
// quantity, balance and fee in the exchange.
//
// Rules (see docs/plan-v1.0.md §6.5):
//   - Amounts are immutable values backed by shopspring/decimal; every
//     operation returns a new Amount.
//   - Floating point never appears on a money path. golangci-lint (forbidigo)
//     enforces this for the numeric-sensitive packages.
//   - The canonical textual form is a plain decimal string without exponent
//     and without trailing zeros ("1.1", "-0.5", "0"). JSON always encodes an
//     Amount as a string, never as a number.
//   - Storage precision is NUMERIC(36,18): at most 18 integer digits and at
//     most 18 fractional digits. ParseAmount and FromBigInt reject anything
//     wider; arithmetic itself is exact and unbounded.
//   - Rounding is explicit: RoundUp (towards +inf, used for fees so rounding
//     favours the exchange), RoundDown (towards -inf) and Truncate (towards
//     zero).
package money

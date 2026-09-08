// Decimal strings, never floats (docs/plan-v1.0.md §6.5 applies to the
// front end too): prices and quantities are compared, added and checked
// against tick and step sizes as scaled BigInts. Amounts stay strings all
// the way from the API to the screen.

export interface Dec {
  n: bigint // value = n / 10^s
  s: number
}

const DECIMAL = /^-?\d+(\.\d+)?$/

export function parseDec(v: string): Dec | null {
  const t = v.trim()
  if (!DECIMAL.test(t)) return null
  const neg = t.startsWith('-')
  const body = neg ? t.slice(1) : t
  const [i, f = ''] = body.split('.')
  const n = BigInt(i + f)
  return { n: neg ? -n : n, s: f.length }
}

export function mustDec(v: string): Dec {
  const d = parseDec(v)
  if (!d) throw new Error(`not a decimal: ${v}`)
  return d
}

function align(a: Dec, b: Dec): [bigint, bigint, number] {
  const s = Math.max(a.s, b.s)
  return [a.n * 10n ** BigInt(s - a.s), b.n * 10n ** BigInt(s - b.s), s]
}

export function cmp(a: Dec, b: Dec): number {
  const [x, y] = align(a, b)
  return x < y ? -1 : x > y ? 1 : 0
}

export function add(a: Dec, b: Dec): Dec {
  const [x, y, s] = align(a, b)
  return { n: x + y, s }
}

export function sub(a: Dec, b: Dec): Dec {
  const [x, y, s] = align(a, b)
  return { n: x - y, s }
}

export function mul(a: Dec, b: Dec): Dec {
  return { n: a.n * b.n, s: a.s + b.s }
}

export function isZero(d: Dec): boolean {
  return d.n === 0n
}

export function isPositive(d: Dec): boolean {
  return d.n > 0n
}

// isMultiple reports whether v is a whole number of steps (a price on the
// tick grid, a quantity on the step grid).
export function isMultiple(v: Dec, step: Dec): boolean {
  if (step.n === 0n) return true
  const [x, y] = align(v, step)
  return x % y === 0n
}

export function format(d: Dec): string {
  const neg = d.n < 0n
  let digits = (neg ? -d.n : d.n).toString()
  if (d.s > 0) {
    digits = digits.padStart(d.s + 1, '0')
    digits = digits.slice(0, -d.s) + '.' + digits.slice(-d.s)
  }
  return (neg ? '-' : '') + digits
}

// trimZeros drops trailing fraction zeros for display: "1990.5000" → "1990.5".
export function trimZeros(v: string): string {
  if (!v.includes('.')) return v
  const t = v.replace(/0+$/, '')
  return t.endsWith('.') ? t.slice(0, -1) : t
}

// cmpStr orders two decimal strings numerically; an unparsable string sorts last.
export function cmpStr(a: string, b: string): number {
  const x = parseDec(a)
  const y = parseDec(b)
  if (!x || !y) return x ? -1 : y ? 1 : 0
  return cmp(x, y)
}

// scaleOf is the number of fraction digits a step implies ("0.01" → 2).
export function scaleOf(step: string): number {
  const d = parseDec(step)
  return d ? d.s : 0
}

// toNumberUnsafe is for chart coordinates only (lightweight-charts draws
// with floats); money never goes through it.
export function toNumberUnsafe(v: string): number {
  return Number(v)
}

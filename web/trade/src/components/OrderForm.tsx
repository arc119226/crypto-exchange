import { useEffect, useState, type FormEvent } from 'react'
import { call, client, describeError, type Market, type Order } from '../api/client'
import { cmp, isMultiple, isPositive, mul, mustDec, parseDec } from '../lib/decimal'
import { clientOrderId } from '../lib/format'

type Side = 'buy' | 'sell'
type Type = 'limit' | 'market'

// validate applies the market's grid before the request leaves the
// browser, so the usual mistakes come back as a sentence instead of a 422
// (the server checks the same things; docs/plan-v1.0.md §5.2).
export function validate(m: Market, side: Side, type: Type, price: string, qty: string, quoteQty: string): string | null {
  const tick = mustDec(m.price_tick)
  const step = mustDec(m.qty_step)
  const minNotional = mustDec(m.min_notional)
  if (type === 'market' && side === 'buy') {
    const q = parseDec(quoteQty)
    if (!q || !isPositive(q)) return 'Enter the quote amount to spend'
    if (cmp(q, minNotional) < 0) return `Below the minimum notional ${m.min_notional} ${m.quote_asset}`
    return null
  }
  const q = parseDec(qty)
  if (!q || !isPositive(q)) return 'Enter a quantity'
  if (!isMultiple(q, step)) return `Quantity must be a multiple of ${m.qty_step}`
  if (m.max_qty && cmp(q, mustDec(m.max_qty)) > 0) return `Above the maximum quantity ${m.max_qty}`
  if (type === 'limit') {
    const p = parseDec(price)
    if (!p || !isPositive(p)) return 'Enter a price'
    if (!isMultiple(p, tick)) return `Price must be a multiple of ${m.price_tick}`
    if (cmp(mul(p, q), minNotional) < 0) return `Below the minimum notional ${m.min_notional} ${m.quote_asset}`
  }
  return null
}

export function OrderForm({ market, pickedPrice, onPlaced }: { market: Market; pickedPrice: string | null; onPlaced: (o: Order) => void }) {
  const [side, setSide] = useState<Side>('buy')
  const [type, setType] = useState<Type>('limit')
  const [tif, setTif] = useState<'gtc' | 'ioc'>('gtc')
  const [price, setPrice] = useState('')
  const [qty, setQty] = useState('')
  const [quoteQty, setQuoteQty] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<string | null>(null)

  useEffect(() => {
    if (pickedPrice) setPrice(pickedPrice)
  }, [pickedPrice])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setError(null)
    setResult(null)
    const problem = validate(market, side, type, price, qty, quoteQty)
    if (problem) {
      setError(problem)
      return
    }
    setBusy(true)
    try {
      const body = {
        client_order_id: clientOrderId(),
        market: market.symbol,
        side,
        type,
        ...(type === 'limit' ? { price, qty, time_in_force: tif } : {}),
        ...(type === 'market' ? (side === 'buy' ? { quote_qty: quoteQty } : { qty }) : {}),
      }
      const { order } = await call(() => client.POST('/v1/orders', { body }))
      if (order.status === 'rejected') {
        setError(`Rejected: ${order.reject_reason ?? 'unknown reason'}`)
      } else {
        setResult(`${order.status.replace('_', ' ')} · filled ${order.filled_qty}`)
      }
      onPlaced(order)
    } catch (err) {
      setError(describeError(err))
    } finally {
      setBusy(false)
    }
  }

  const marketBuy = type === 'market' && side === 'buy'
  return (
    <form className="card stack" onSubmit={submit} data-testid="order-form">
      <h2>Place order</h2>
      <div className="tabs">
        <button type="button" className={side === 'buy' ? 'active buy' : ''} onClick={() => setSide('buy')} data-testid="side-buy">
          Buy {market.base_asset}
        </button>
        <button type="button" className={side === 'sell' ? 'active sell' : ''} onClick={() => setSide('sell')} data-testid="side-sell">
          Sell {market.base_asset}
        </button>
      </div>
      <div className="row">
        <select value={type} onChange={(e) => setType(e.target.value as Type)} data-testid="order-type">
          <option value="limit">Limit</option>
          <option value="market">Market</option>
        </select>
        {type === 'limit' && (
          <select value={tif} onChange={(e) => setTif(e.target.value as 'gtc' | 'ioc')} data-testid="order-tif">
            <option value="gtc">GTC</option>
            <option value="ioc">IOC</option>
          </select>
        )}
      </div>
      {type === 'limit' && (
        <div className="field">
          <label htmlFor="price">Price ({market.quote_asset}, tick {market.price_tick})</label>
          <input id="price" inputMode="decimal" value={price} onChange={(e) => setPrice(e.target.value)} data-testid="price" />
        </div>
      )}
      {marketBuy ? (
        <div className="field">
          <label htmlFor="quote">Spend ({market.quote_asset})</label>
          <input id="quote" inputMode="decimal" value={quoteQty} onChange={(e) => setQuoteQty(e.target.value)} data-testid="quote-qty" />
        </div>
      ) : (
        <div className="field">
          <label htmlFor="qty">Quantity ({market.base_asset}, step {market.qty_step})</label>
          <input id="qty" inputMode="decimal" value={qty} onChange={(e) => setQty(e.target.value)} data-testid="qty" />
        </div>
      )}
      {error && <p className="error" role="alert" data-testid="order-error">{error}</p>}
      {result && <p className="ok" data-testid="order-result">{result}</p>}
      <button type="submit" className={side} disabled={busy || market.status !== 'active'} data-testid="submit-order">
        {market.status === 'active' ? `${side === 'buy' ? 'Buy' : 'Sell'} ${market.base_asset}` : `Market ${market.status}`}
      </button>
    </form>
  )
}

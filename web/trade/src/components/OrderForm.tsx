import { useEffect, useState, type FormEvent } from 'react'
import { call, client, describeError, type Market, type Order } from '../api/client'
import { enumLabel } from '../i18n/enums'
import { useLocale, type Key, type Params } from '../i18n/LocaleProvider'
import { cmp, isMultiple, isPositive, mul, mustDec, parseDec } from '../lib/decimal'
import { clientOrderId } from '../lib/format'

type Side = 'buy' | 'sell'
type Type = 'limit' | 'market'

// Invalid names what is wrong with the form as a message key and its
// values; the component turns it into a sentence in the current language.
export interface Invalid {
  key: Key
  params?: Params
}

// validate applies the market's grid before the request leaves the
// browser, so the usual mistakes come back as a sentence instead of a 422
// (the server checks the same things; docs/plan-v1.0.md §5.2).
export function validate(m: Market, side: Side, type: Type, price: string, qty: string, quoteQty: string): Invalid | null {
  const tick = mustDec(m.price_tick)
  const step = mustDec(m.qty_step)
  const minNotional = mustDec(m.min_notional)
  const belowMin: Invalid = { key: 'trade.validate.min_notional', params: { min: m.min_notional, quote: m.quote_asset } }
  if (type === 'market' && side === 'buy') {
    const q = parseDec(quoteQty)
    if (!q || !isPositive(q)) return { key: 'trade.validate.quote_qty' }
    if (cmp(q, minNotional) < 0) return belowMin
    return null
  }
  const q = parseDec(qty)
  if (!q || !isPositive(q)) return { key: 'trade.validate.qty' }
  if (!isMultiple(q, step)) return { key: 'trade.validate.qty_step', params: { step: m.qty_step } }
  if (m.max_qty && cmp(q, mustDec(m.max_qty)) > 0) return { key: 'trade.validate.max_qty', params: { max: m.max_qty } }
  if (type === 'limit') {
    const p = parseDec(price)
    if (!p || !isPositive(p)) return { key: 'trade.validate.price' }
    if (!isMultiple(p, tick)) return { key: 'trade.validate.price_tick', params: { tick: m.price_tick } }
    if (cmp(mul(p, q), minNotional) < 0) return belowMin
  }
  return null
}

export function OrderForm({ market, pickedPrice, onPlaced }: { market: Market; pickedPrice: string | null; onPlaced: (o: Order) => void }) {
  const { locale, t } = useLocale()
  const [side, setSide] = useState<Side>('buy')
  const [type, setType] = useState<Type>('limit')
  const [tif, setTif] = useState<'gtc' | 'ioc'>('gtc')
  const [price, setPrice] = useState('')
  const [qty, setQty] = useState('')
  const [quoteQty, setQuoteQty] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<Order | null>(null)

  useEffect(() => {
    if (pickedPrice) setPrice(pickedPrice)
  }, [pickedPrice])

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setError(null)
    setResult(null)
    const problem = validate(market, side, type, price, qty, quoteQty)
    if (problem) {
      setError(t(problem.key, problem.params))
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
        setError(t('trade.rejected', { reason: enumLabel(locale, 'reject', order.reject_reason || 'unknown') }))
      } else {
        setResult(order)
      }
      onPlaced(order)
    } catch (err) {
      setError(describeError(err, t))
    } finally {
      setBusy(false)
    }
  }

  const marketBuy = type === 'market' && side === 'buy'
  const asset = market.base_asset
  return (
    <form className="card stack" onSubmit={submit} data-testid="order-form">
      <h2>{t('trade.place_order')}</h2>
      <div className="tabs">
        <button type="button" className={side === 'buy' ? 'active buy' : ''} onClick={() => setSide('buy')} data-testid="side-buy">
          {t('trade.buy', { asset })}
        </button>
        <button type="button" className={side === 'sell' ? 'active sell' : ''} onClick={() => setSide('sell')} data-testid="side-sell">
          {t('trade.sell', { asset })}
        </button>
      </div>
      <div className="row">
        <select value={type} onChange={(e) => setType(e.target.value as Type)} data-testid="order-type">
          <option value="limit">{t('trade.limit')}</option>
          <option value="market">{t('trade.market')}</option>
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
          <label htmlFor="price">{t('trade.price_label', { quote: market.quote_asset, tick: market.price_tick })}</label>
          <input id="price" inputMode="decimal" value={price} onChange={(e) => setPrice(e.target.value)} data-testid="price" />
        </div>
      )}
      {marketBuy ? (
        <div className="field">
          <label htmlFor="quote">{t('trade.spend_label', { quote: market.quote_asset })}</label>
          <input id="quote" inputMode="decimal" value={quoteQty} onChange={(e) => setQuoteQty(e.target.value)} data-testid="quote-qty" />
        </div>
      ) : (
        <div className="field">
          <label htmlFor="qty">{t('trade.qty_label', { base: asset, step: market.qty_step })}</label>
          <input id="qty" inputMode="decimal" value={qty} onChange={(e) => setQty(e.target.value)} data-testid="qty" />
        </div>
      )}
      {error && <p className="error" role="alert" data-testid="order-error">{error}</p>}
      {result && (
        <p className="ok" data-testid="order-result" data-status={result.status}>
          {t('trade.result', { status: enumLabel(locale, 'order', result.status), filled: result.filled_qty })}
        </p>
      )}
      <button type="submit" className={side} disabled={busy || market.status !== 'active'} data-testid="submit-order">
        {market.status === 'active'
          ? t(side === 'buy' ? 'trade.buy' : 'trade.sell', { asset })
          : t('trade.market_status', { status: enumLabel(locale, 'market', market.status) })}
      </button>
    </form>
  )
}

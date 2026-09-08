import { useEffect, useState } from 'react'
import { call, client } from '../api/client'
import { trimZeros } from '../lib/decimal'
import { formatTime } from '../lib/format'
import type { PublicFeed } from '../ws/public'

interface Row {
  id: string
  price: string
  qty: string
  taker_side: 'buy' | 'sell'
  at: string
}

const KEEP = 50

// TradesList shows the market's recent trades: the REST list first, then
// the trades channel prepends each new one.
export function TradesList({ feed, market }: { feed: PublicFeed; market: string }) {
  const [rows, setRows] = useState<Row[]>([])

  useEffect(() => {
    let cancelled = false
    setRows([])
    call(() => client.GET('/v1/markets/{symbol}/trades', { params: { path: { symbol: market }, query: { limit: KEEP } } }))
      .then((res) => {
        if (cancelled) return
        const initial = res.trades.map((t) => ({ id: t.trade_id, price: t.price, qty: t.qty, taker_side: t.taker_side, at: t.executed_at }))
        setRows((live) => {
          const seen = new Set(live.map((r) => r.id))
          return [...live, ...initial.filter((r) => !seen.has(r.id))].slice(0, KEEP)
        })
      })
      .catch(() => {
        // the live channel still fills the list
      })
    const unsubscribe = feed.subscribe('trades', market, (m) => {
      if (m.type !== 'update' || !m.trade) return
      const t = m.trade
      setRows((live) => {
        if (live.some((r) => r.id === t.trade_id)) return live
        return [{ id: t.trade_id, price: t.price, qty: t.qty, taker_side: t.taker_side, at: t.executed_at }, ...live].slice(0, KEEP)
      })
    })
    return () => {
      cancelled = true
      unsubscribe()
    }
  }, [feed, market])

  return (
    <div className="card">
      <h2>Recent trades</h2>
      <div className="scroll" style={{ maxHeight: 260 }}>
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Price</th>
              <th>Qty</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.id} data-testid="trade-row" data-price={r.price}>
                <td className="muted">{formatTime(r.at)}</td>
                <td className={r.taker_side}>{trimZeros(r.price)}</td>
                <td>{trimZeros(r.qty)}</td>
              </tr>
            ))}
            {rows.length === 0 && (
              <tr>
                <td colSpan={3} className="muted">
                  No trades yet
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}

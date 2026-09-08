import { useEffect, useMemo, useState } from 'react'
import type { Market } from '../api/client'
import { cmpStr, mustDec, sub, format, trimZeros } from '../lib/decimal'
import type { Level, PublicFeed, PublicMessage } from '../ws/public'

type BookStatus = 'connecting' | 'live' | 'resync'

interface BookState {
  bids: Map<string, string>
  asks: Map<string, string>
  seq: number
  status: BookStatus
}

// useOrderBook implements the client half of the depth contract
// (docs/plan-v1.0.md §7.5, docs/ws-api.md): a snapshot sets the book and
// its seq; a delta with seq <= last is a duplicate and is dropped; seq ==
// last + 1 is applied (qty "0" removes the level); anything else is a gap
// and the subscription is re-taken for a fresh snapshot.
export function useOrderBook(feed: PublicFeed, market: string): BookState {
  const [state, setState] = useState<BookState>({ bids: new Map(), asks: new Map(), seq: 0, status: 'connecting' })

  useEffect(() => {
    let seq = 0
    let live = false
    setState({ bids: new Map(), asks: new Map(), seq: 0, status: 'connecting' })
    const apply = (m: Map<string, string>, levels: Level[] | undefined): Map<string, string> => {
      const next = new Map(m)
      for (const [price, qty] of levels ?? []) {
        if (qty === '0' || /^0(\.0+)?$/.test(qty)) next.delete(price)
        else next.set(price, qty)
      }
      return next
    }
    const unsubscribe = feed.subscribe('depth', market, (m: PublicMessage) => {
      if (m.type === 'snapshot' && m.seq !== undefined) {
        seq = m.seq
        live = true
        setState({ bids: apply(new Map(), m.bids), asks: apply(new Map(), m.asks), seq, status: 'live' })
        return
      }
      if (m.type === 'delta' && m.seq !== undefined) {
        if (!live || m.seq <= seq) return
        if (m.seq !== seq + 1) {
          live = false
          setState((s) => ({ ...s, status: 'resync' }))
          feed.resync('depth', market)
          return
        }
        seq = m.seq
        setState((s) => ({ bids: apply(s.bids, m.bids), asks: apply(s.asks, m.asks), seq, status: 'live' }))
        return
      }
      if (m.type === 'unsubscribed') {
        live = false
      }
    })
    return unsubscribe
  }, [feed, market])

  return state
}

export function OrderBook({ feed, market, info, onPickPrice }: { feed: PublicFeed; market: string; info: Market | null; onPickPrice?: (price: string) => void }) {
  const book = useOrderBook(feed, market)
  const rows = 15
  const asks = useMemo(() => [...book.asks.entries()].sort((a, b) => cmpStr(a[0], b[0])).slice(0, rows), [book.asks])
  const bids = useMemo(() => [...book.bids.entries()].sort((a, b) => cmpStr(b[0], a[0])).slice(0, rows), [book.bids])
  const bestAsk = asks[0]?.[0]
  const bestBid = bids[0]?.[0]
  const spread = bestAsk && bestBid ? trimZeros(format(sub(mustDec(bestAsk), mustDec(bestBid)))) : null

  return (
    <div className="card book" data-testid="order-book" data-status={book.status} data-seq={book.seq}>
      <h2>
        Order book <span className="muted">{info ? `${info.base_asset}/${info.quote_asset}` : ''}</span>
      </h2>
      <p className="muted">
        <span className={`status-dot ${book.status === 'live' ? 'live' : 'connecting'}`} />
        {book.status === 'live' ? `seq ${book.seq}` : book.status === 'resync' ? 'resyncing…' : 'connecting…'}
      </p>
      <table>
        <thead>
          <tr>
            <th>Price</th>
            <th>Qty</th>
          </tr>
        </thead>
        <tbody>
          {[...asks].reverse().map(([price, qty]) => (
            <tr key={`a${price}`} data-testid="ask-row" data-price={price} onClick={() => onPickPrice?.(price)}>
              <td className="sell">{trimZeros(price)}</td>
              <td>{trimZeros(qty)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="spread">{spread ? `spread ${spread}` : '—'}</div>
      <table>
        <tbody>
          {bids.map(([price, qty]) => (
            <tr key={`b${price}`} data-testid="bid-row" data-price={price} onClick={() => onPickPrice?.(price)}>
              <td className="buy">{trimZeros(price)}</td>
              <td>{trimZeros(qty)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

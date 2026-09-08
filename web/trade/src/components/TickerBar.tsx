import { useEffect, useState } from 'react'
import { call, client, type Market, type Ticker } from '../api/client'
import { trimZeros } from '../lib/decimal'
import type { PublicFeed } from '../ws/public'

export function TickerBar({ feed, market, info }: { feed: PublicFeed; market: string; info: Market | null }) {
  const [ticker, setTicker] = useState<Ticker | null>(null)

  useEffect(() => {
    let cancelled = false
    setTicker(null)
    call(() => client.GET('/v1/markets/{symbol}/ticker', { params: { path: { symbol: market } } }))
      .then((t) => {
        if (!cancelled) setTicker((cur) => cur ?? t)
      })
      .catch(() => {
        // the channel fills it in on the next trade
      })
    const unsubscribe = feed.subscribe('ticker', market, (m) => {
      if (m.type === 'update' && m.ticker) setTicker(m.ticker as unknown as Ticker)
    })
    return () => {
      cancelled = true
      unsubscribe()
    }
  }, [feed, market])

  const pct = ticker?.change_pct
  const up = pct !== undefined && !pct.startsWith('-')
  return (
    <div className="card ticker" data-testid="ticker">
      <span className="last">{ticker?.last_price ? trimZeros(ticker.last_price) : '—'}</span>
      <span className="item">
        <span>24h change</span>
        <span className={pct === undefined ? 'muted' : up ? 'buy' : 'sell'}>{pct !== undefined ? `${pct}%` : '—'}</span>
      </span>
      <span className="item">
        <span>24h high</span>
        <span>{ticker?.high ? trimZeros(ticker.high) : '—'}</span>
      </span>
      <span className="item">
        <span>24h low</span>
        <span>{ticker?.low ? trimZeros(ticker.low) : '—'}</span>
      </span>
      <span className="item">
        <span>24h volume</span>
        <span>
          {ticker ? trimZeros(ticker.volume) : '—'} {info?.base_asset ?? ''}
        </span>
      </span>
      <span className="item">
        <span>Fees</span>
        <span className="muted">{info ? `maker ${info.maker_bps} bps / taker ${info.taker_bps} bps` : '—'}</span>
      </span>
    </div>
  )
}

import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { call, client, describeError, type Market, type Ticker } from '../api/client'
import { trimZeros } from '../lib/decimal'

interface Row {
  market: Market
  ticker: Ticker | null
}

export function MarketsPage() {
  const navigate = useNavigate()
  const [rows, setRows] = useState<Row[] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const { markets } = await call(() => client.GET('/v1/markets'))
        const out: Row[] = await Promise.all(
          markets.map(async (market) => {
            try {
              const ticker = await call(() => client.GET('/v1/markets/{symbol}/ticker', { params: { path: { symbol: market.symbol } } }))
              return { market, ticker }
            } catch {
              return { market, ticker: null }
            }
          }),
        )
        if (!cancelled) setRows(out)
      } catch (err) {
        if (!cancelled) setError(describeError(err))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  return (
    <div className="page">
      <div className="card">
        <h2>Markets</h2>
        {error && <p className="error">{error}</p>}
        {!rows && !error && <p className="muted">Loading…</p>}
        {rows && (
          <table className="markets-table">
            <thead>
              <tr>
                <th>Market</th>
                <th>Last</th>
                <th>24h change</th>
                <th>24h volume</th>
                <th>Trades</th>
                <th>Status</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(({ market, ticker }) => {
                const pct = ticker?.change_pct
                const up = pct !== undefined && !pct.startsWith('-')
                return (
                  <tr key={market.symbol} onClick={() => navigate(`/trade/${market.symbol}`)} data-testid={`market-${market.symbol}`}>
                    <td>
                      <strong>{market.symbol}</strong> <span className="muted">{market.base_asset}/{market.quote_asset}</span>
                    </td>
                    <td>{ticker?.last_price ? trimZeros(ticker.last_price) : '—'}</td>
                    <td className={pct === undefined ? 'muted' : up ? 'buy' : 'sell'}>{pct !== undefined ? `${pct}%` : '—'}</td>
                    <td>{ticker ? `${trimZeros(ticker.volume)} ${market.base_asset}` : '—'}</td>
                    <td>{ticker?.trades ?? '—'}</td>
                    <td className={market.status === 'active' ? 'ok' : 'muted'}>{market.status}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  )
}

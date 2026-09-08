import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { call, client, describeError, type Market, type Ticker } from '../api/client'
import { enumLabel } from '../i18n/enums'
import { useLocale } from '../i18n/LocaleProvider'
import { trimZeros } from '../lib/decimal'

interface Row {
  market: Market
  ticker: Ticker | null
}

export function MarketsPage() {
  const navigate = useNavigate()
  const { locale, t } = useLocale()
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
        if (!cancelled) setError(describeError(err, t))
      }
    })()
    return () => {
      cancelled = true
    }
    // one load per visit; a language switch re-words an error on the next visit
  }, [])

  return (
    <div className="page">
      <div className="card">
        <h2>{t('markets.title')}</h2>
        {error && <p className="error">{error}</p>}
        {!rows && !error && <p className="muted">{t('common.loading')}</p>}
        {rows && (
          <table className="markets-table">
            <thead>
              <tr>
                <th>{t('markets.col.market')}</th>
                <th>{t('markets.col.last')}</th>
                <th>{t('markets.col.change')}</th>
                <th>{t('markets.col.volume')}</th>
                <th>{t('markets.col.trades')}</th>
                <th>{t('col.status')}</th>
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
                    <td className={market.status === 'active' ? 'ok' : 'muted'}>{enumLabel(locale, 'market', market.status)}</td>
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

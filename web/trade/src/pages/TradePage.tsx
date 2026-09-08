import { useEffect, useMemo, useState } from 'react'
import { useParams } from 'react-router-dom'
import { call, client, describeError, type Market } from '../api/client'
import { Balances, FillsList, OpenOrders } from '../components/AccountPanels'
import { KlineChart } from '../components/KlineChart'
import { OrderBook } from '../components/OrderBook'
import { OrderForm } from '../components/OrderForm'
import { TickerBar } from '../components/TickerBar'
import { TradesList } from '../components/TradesList'
import { enumLabel } from '../i18n/enums'
import { useLocale } from '../i18n/LocaleProvider'
import { PublicFeed, wsURL, type FeedStatus } from '../ws/public'

export function TradePage() {
  const { symbol = '' } = useParams()
  const { locale, t } = useLocale()
  const [market, setMarket] = useState<Market | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [picked, setPicked] = useState<string | null>(null)
  const [refreshKey, setRefreshKey] = useState(0)
  const [feedStatus, setFeedStatus] = useState<FeedStatus>('closed')

  // one public socket for the whole page; the components share it
  const feed = useMemo(() => new PublicFeed(wsURL('/ws/v1/public')), [])
  useEffect(() => {
    feed.start()
    const off = feed.onStatus(setFeedStatus)
    return () => {
      off()
      feed.stop()
    }
  }, [feed])

  useEffect(() => {
    let cancelled = false
    setMarket(null)
    setError(null)
    call(() => client.GET('/v1/markets/{symbol}', { params: { path: { symbol } } }))
      .then((m) => {
        if (!cancelled) setMarket(m)
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(describeError(err, t))
      })
    return () => {
      cancelled = true
    }
    // one load per market; a language switch re-words an error on the next load
  }, [symbol])

  if (error) return <p className="page error">{error}</p>

  return (
    <div className="page">
      <div className="row" style={{ marginBottom: 12 }}>
        <TickerBar feed={feed} market={symbol} info={market} />
        <span className="muted" data-testid="public-status" data-status={feedStatus}>
          <span className={`status-dot ${feedStatus}`} />
          {t('trade.public_stream', { status: enumLabel(locale, 'feed', feedStatus) })}
        </span>
      </div>
      <div className="trade-grid">
        <OrderBook feed={feed} market={symbol} info={market} onPickPrice={setPicked} />
        <KlineChart feed={feed} market={symbol} />
        <div className="side">
          {market ? <OrderForm market={market} pickedPrice={picked} onPlaced={() => setRefreshKey((k) => k + 1)} /> : <div className="card muted">{t('trade.loading_market')}</div>}
          <Balances market={market} refreshKey={refreshKey} />
          <TradesList feed={feed} market={symbol} />
        </div>
        <div className="lists stack">
          <OpenOrders market={symbol} refreshKey={refreshKey} />
          <FillsList market={symbol} refreshKey={refreshKey} />
        </div>
      </div>
    </div>
  )
}

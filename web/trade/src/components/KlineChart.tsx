import { useEffect, useRef, useState } from 'react'
import { CandlestickSeries, createChart, type IChartApi, type ISeriesApi, type UTCTimestamp } from 'lightweight-charts'
import { call, client, type KlineInterval } from '../api/client'
import { toNumberUnsafe } from '../lib/decimal'
import type { PublicFeed } from '../ws/public'

const INTERVALS: KlineInterval[] = ['1m', '5m', '15m', '1h', '1d']

interface Bar {
  time: UTCTimestamp
  open: number
  high: number
  low: number
  close: number
}

function toBar(c: { start: string; open: string; high: string; low: string; close: string }): Bar {
  return {
    time: Math.floor(Date.parse(c.start) / 1000) as UTCTimestamp,
    open: toNumberUnsafe(c.open),
    high: toNumberUnsafe(c.high),
    low: toNumberUnsafe(c.low),
    close: toNumberUnsafe(c.close),
  }
}

// KlineChart draws the REST history and then applies kline.<interval>
// updates from the stream. Amounts become floats only here, for pixels;
// nothing computed from them goes back to the API (docs/plan-v1.0.md §6.5).
export function KlineChart({ feed, market }: { feed: PublicFeed; market: string }) {
  const container = useRef<HTMLDivElement>(null)
  const chart = useRef<IChartApi | null>(null)
  const series = useRef<ISeriesApi<'Candlestick'> | null>(null)
  const [interval, setInterval] = useState<KlineInterval>('1m')
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!container.current) return
    const c = createChart(container.current, {
      layout: { background: { color: '#171d25' }, textColor: '#8b95a7' },
      grid: { vertLines: { color: '#262e3a' }, horzLines: { color: '#262e3a' } },
      timeScale: { timeVisible: true, secondsVisible: false },
      autoSize: true,
    })
    const s = c.addSeries(CandlestickSeries, {
      upColor: '#2ecc71',
      downColor: '#e74c3c',
      borderVisible: false,
      wickUpColor: '#2ecc71',
      wickDownColor: '#e74c3c',
    })
    chart.current = c
    series.current = s
    return () => {
      c.remove()
      chart.current = null
      series.current = null
    }
  }, [])

  useEffect(() => {
    let cancelled = false
    let lastTime = 0
    const s = series.current
    if (!s) return
    s.setData([])
    setError(null)
    call(() => client.GET('/v1/markets/{symbol}/klines', { params: { path: { symbol: market }, query: { interval, limit: 300 } } }))
      .then((res) => {
        if (cancelled || !series.current) return
        const bars = res.klines.map(toBar)
        series.current.setData(bars)
        lastTime = bars.length ? bars[bars.length - 1]!.time : 0
        chart.current?.timeScale().fitContent()
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      })
    const unsubscribe = feed.subscribe(`kline.${interval}`, market, (m) => {
      if (m.type !== 'update' || !m.candle || !series.current) return
      const bar = toBar(m.candle)
      if (bar.time < lastTime) return // an older bucket than the last drawn one cannot be applied
      lastTime = bar.time
      series.current.update(bar)
    })
    return () => {
      cancelled = true
      unsubscribe()
    }
  }, [feed, market, interval])

  return (
    <div className="card chart">
      <div className="row" style={{ justifyContent: 'space-between' }}>
        <h2 style={{ margin: 0 }}>{market}</h2>
        <div className="tabs" style={{ margin: 0 }}>
          {INTERVALS.map((iv) => (
            <button key={iv} className={iv === interval ? 'active' : ''} onClick={() => setInterval(iv)} data-testid={`interval-${iv}`}>
              {iv}
            </button>
          ))}
        </div>
      </div>
      {error && <p className="error">{error}</p>}
      <div ref={container} style={{ height: 320 }} data-testid="kline-chart" />
    </div>
  )
}

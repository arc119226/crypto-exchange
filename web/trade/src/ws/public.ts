// PublicFeed is one WebSocket to /ws/v1/public shared by every component on
// the page (docs/ws-api.md). Subscriptions are reference counted per
// channel|market; the socket reconnects with backoff and re-subscribes, and
// every listener then receives a fresh snapshot from the server, which is
// how the §7.5 client rules restart after a disconnect.

export type Level = [price: string, qty: string]

export interface PublicMessage {
  type: string
  channel?: string
  market?: string
  seq?: number
  at?: string
  code?: string
  message?: string
  bids?: Level[]
  asks?: Level[]
  trade?: { trade_id: string; price: string; qty: string; quote_qty: string; taker_side: 'buy' | 'sell'; executed_at: string }
  ticker?: Record<string, unknown>
  candle?: { start: string; open: string; high: string; low: string; close: string; volume: string; quote_volume: string; trades: number }
}

export type FeedStatus = 'connecting' | 'open' | 'closed'
type Listener = (m: PublicMessage) => void
type StatusListener = (s: FeedStatus) => void

export function wsURL(path: string): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${window.location.host}${path}`
}

export class PublicFeed {
  private ws: WebSocket | null = null
  private readonly subs = new Map<string, Set<Listener>>()
  private readonly statusListeners = new Set<StatusListener>()
  private attempts = 0
  private timer: ReturnType<typeof setTimeout> | null = null
  private stopped = false
  status: FeedStatus = 'closed'

  constructor(private readonly url: string) {}

  start(): void {
    this.stopped = false
    if (!this.ws) this.connect()
  }

  stop(): void {
    this.stopped = true
    if (this.timer) clearTimeout(this.timer)
    this.timer = null
    this.ws?.close(1000, 'client closed')
    this.ws = null
    this.setStatus('closed')
  }

  onStatus(l: StatusListener): () => void {
    this.statusListeners.add(l)
    l(this.status)
    return () => this.statusListeners.delete(l)
  }

  subscribe(channel: string, market: string, l: Listener): () => void {
    const key = `${channel}|${market}`
    let set = this.subs.get(key)
    if (!set) {
      set = new Set()
      this.subs.set(key, set)
      this.send({ op: 'subscribe', channel, market })
    }
    set.add(l)
    return () => {
      const s = this.subs.get(key)
      if (!s) return
      s.delete(l)
      if (s.size === 0) {
        this.subs.delete(key)
        this.send({ op: 'unsubscribe', channel, market })
      }
    }
  }

  // resync drops and re-takes a subscription so the server sends a new
  // snapshot: the §7.5 answer to a sequence gap.
  resync(channel: string, market: string): void {
    if (!this.subs.has(`${channel}|${market}`)) return
    this.send({ op: 'unsubscribe', channel, market })
    this.send({ op: 'subscribe', channel, market })
  }

  private send(m: Record<string, unknown>): void {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m))
  }

  private setStatus(s: FeedStatus): void {
    if (this.status === s) return
    this.status = s
    for (const l of this.statusListeners) l(s)
  }

  private connect(): void {
    this.setStatus('connecting')
    const ws = new WebSocket(this.url)
    this.ws = ws
    ws.onopen = () => {
      this.attempts = 0
      this.setStatus('open')
      for (const key of this.subs.keys()) {
        const [channel, market] = key.split('|')
        this.send({ op: 'subscribe', channel, market })
      }
    }
    ws.onmessage = (ev) => {
      let m: PublicMessage
      try {
        m = JSON.parse(String(ev.data)) as PublicMessage
      } catch {
        return
      }
      if (m.channel && m.market) {
        const set = this.subs.get(`${m.channel}|${m.market}`)
        if (set) for (const l of set) l(m)
        return
      }
      if (m.type === 'error') {
        for (const set of this.subs.values()) for (const l of set) l(m)
      }
    }
    ws.onclose = () => {
      if (this.ws !== ws) return
      this.ws = null
      this.setStatus('closed')
      if (this.stopped) return
      const wait = Math.min(1000 * 2 ** this.attempts, 15_000)
      this.attempts += 1
      this.timer = setTimeout(() => this.connect(), wait)
    }
    ws.onerror = () => {
      // onclose follows; nothing to do here
    }
  }
}

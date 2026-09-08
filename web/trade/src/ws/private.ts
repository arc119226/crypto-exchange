// PrivateFeed is the account's WebSocket to /ws/v1/private (docs/ws-api.md):
// authenticate, then either resume from the last account_seq seen or go
// live from the auth acknowledgement's sequence. A resume the server can no
// longer serve (resume_too_late / resume_failed) is reported through
// onResync so the page refetches its lists, which is also what happens on
// the very first connection.

export interface PrivateMessage {
  channel: string
  type: string
  role?: 'maker' | 'taker'
  seq: number | null
  account_seq: number | null
  event_id: string
  occurred_at: string
  data: unknown
}

export type PrivateStatus = 'connecting' | 'authenticating' | 'live' | 'closed'

interface ControlMessage {
  type: string
  code?: string
  message?: string
  account_id?: string
  account_seq?: number
  since_seq?: number
  replayed?: number
  channel?: string
}

export interface PrivateFeedHandlers {
  getToken: (force?: boolean) => Promise<string | null>
  onEvent: (m: PrivateMessage) => void
  onResync: () => void
  onStatus?: (s: PrivateStatus) => void
}

export class PrivateFeed {
  private ws: WebSocket | null = null
  private attempts = 0
  private timer: ReturnType<typeof setTimeout> | null = null
  private stopped = false
  private forceRefresh = false
  lastSeq: number | null = null
  status: PrivateStatus = 'closed'

  constructor(
    private readonly url: string,
    private readonly h: PrivateFeedHandlers,
  ) {}

  start(): void {
    this.stopped = false
    if (!this.ws) void this.connect()
  }

  stop(): void {
    this.stopped = true
    if (this.timer) clearTimeout(this.timer)
    this.timer = null
    this.ws?.close(1000, 'client closed')
    this.ws = null
    this.setStatus('closed')
  }

  private setStatus(s: PrivateStatus): void {
    if (this.status === s) return
    this.status = s
    this.h.onStatus?.(s)
  }

  private send(m: Record<string, unknown>): void {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(m))
  }

  private scheduleReconnect(): void {
    if (this.stopped) return
    const wait = Math.min(1000 * 2 ** this.attempts, 15_000)
    this.attempts += 1
    this.timer = setTimeout(() => void this.connect(), wait)
  }

  private async connect(): Promise<void> {
    this.setStatus('connecting')
    const token = await this.h.getToken(this.forceRefresh)
    this.forceRefresh = false
    if (this.stopped) return
    if (!token) {
      this.scheduleReconnect()
      return
    }
    const ws = new WebSocket(this.url)
    this.ws = ws
    ws.onopen = () => {
      this.setStatus('authenticating')
      this.send({ op: 'auth', token })
    }
    ws.onmessage = (ev) => {
      let m: ControlMessage & Partial<PrivateMessage>
      try {
        m = JSON.parse(String(ev.data)) as ControlMessage & Partial<PrivateMessage>
      } catch {
        return
      }
      this.handle(m)
    }
    ws.onclose = (ev) => {
      if (this.ws !== ws) return
      this.ws = null
      this.setStatus('closed')
      if (ev.code === 1008 && (ev.reason === 'auth_failed' || ev.reason === 'auth_required')) {
        this.forceRefresh = true
      }
      this.scheduleReconnect()
    }
  }

  private handle(m: ControlMessage & Partial<PrivateMessage>): void {
    switch (m.type) {
      case 'auth': {
        this.attempts = 0
        if (this.lastSeq === null) {
          // first connection: everything up to the ack is in the REST
          // lists the page fetches; live frames follow
          this.lastSeq = m.account_seq ?? null
          this.h.onResync()
          this.send({ op: 'ping' }) // any non-resume op ends the hold window
          this.setStatus('live')
        } else {
          this.send({ op: 'resume', since_seq: this.lastSeq })
        }
        return
      }
      case 'resumed':
        this.setStatus('live')
        return
      case 'pong':
      case 'subscribed':
      case 'unsubscribed':
        return
      case 'error':
        if (m.code === 'resume_too_late' || m.code === 'resume_failed') {
          // the connection is live from here; what was missed is refetched
          this.lastSeq = null
          this.h.onResync()
          this.setStatus('live')
        }
        return
      default:
        break
    }
    if (m.channel && m.event_id) {
      const pm = m as PrivateMessage
      if (pm.account_seq !== null && pm.account_seq !== undefined) {
        if (this.lastSeq === null || pm.account_seq > this.lastSeq) this.lastSeq = pm.account_seq
      }
      this.h.onEvent(pm)
    }
  }
}

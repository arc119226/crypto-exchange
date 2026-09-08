import { createContext, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useAuth } from '../auth/session'
import { PrivateFeed, type PrivateMessage, type PrivateStatus } from './private'
import { wsURL } from './public'

type Handler = (m: PrivateMessage) => void

interface PrivateFeedValue {
  status: PrivateStatus
  // resyncVersion increments whenever the page should refetch its account
  // lists: the first connection and any resume the server refused.
  resyncVersion: number
  subscribe: (h: Handler) => () => void
}

const Ctx = createContext<PrivateFeedValue | null>(null)

export function PrivateFeedProvider({ children }: { children: ReactNode }) {
  const { session, accessToken } = useAuth()
  const [status, setStatus] = useState<PrivateStatus>('closed')
  const [resyncVersion, setResyncVersion] = useState(0)
  const handlers = useRef(new Set<Handler>())
  const tokenRef = useRef(accessToken)
  tokenRef.current = accessToken

  useEffect(() => {
    if (!session) {
      setStatus('closed')
      return
    }
    const feed = new PrivateFeed(wsURL('/ws/v1/private'), {
      getToken: (force) => tokenRef.current(force),
      onEvent: (m) => {
        for (const h of handlers.current) h(m)
      },
      onResync: () => setResyncVersion((v) => v + 1),
      onStatus: setStatus,
    })
    feed.start()
    return () => feed.stop()
  }, [session?.accountId]) // eslint-disable-line react-hooks/exhaustive-deps

  const value = useMemo<PrivateFeedValue>(
    () => ({
      status,
      resyncVersion,
      subscribe: (h) => {
        handlers.current.add(h)
        return () => handlers.current.delete(h)
      },
    }),
    [status, resyncVersion],
  )
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function usePrivateFeed(): PrivateFeedValue {
  const v = useContext(Ctx)
  if (!v) throw new Error('usePrivateFeed outside PrivateFeedProvider')
  return v
}

// useAccountEvents calls handler for every private frame on the given
// channels. The handler is read through a ref so callers can pass a fresh
// closure on every render.
export function useAccountEvents(channels: string[], handler: Handler): void {
  const { subscribe } = usePrivateFeed()
  const ref = useRef(handler)
  ref.current = handler
  const key = channels.join(',')
  useEffect(() => {
    const wanted = new Set(key.split(',').filter(Boolean))
    return subscribe((m) => {
      if (wanted.has(m.channel)) ref.current(m)
    })
  }, [subscribe, key])
}

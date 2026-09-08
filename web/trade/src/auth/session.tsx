import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { call, client, setAccessToken, setRefresher, type Session } from '../api/client'

// Token handling (docs/plan-v1.0.md §8, docs/domain.md §25): the access
// token is kept in memory; the refresh token in sessionStorage so a reload
// keeps the session but a new tab or a closed browser does not. Refresh
// tokens are single use and a reuse revokes the whole family, so a leaked
// one exposes itself the moment either side uses it.
const REFRESH_KEY = 'exchange.refresh_token'

export interface SessionInfo {
  accountId: string
  userId: string
  role: string
  expiresAt: number // ms since epoch
}

interface AuthValue {
  session: SessionInfo | null
  ready: boolean
  login: (email: string, password: string) => Promise<void>
  register: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
  // accessToken returns the current token, refreshing first when asked or
  // when it is about to expire; null when there is no session.
  accessToken: (force?: boolean) => Promise<string | null>
}

const AuthContext = createContext<AuthValue | null>(null)

function readRefreshToken(): string | null {
  try {
    return sessionStorage.getItem(REFRESH_KEY)
  } catch {
    return null
  }
}

function writeRefreshToken(token: string | null): void {
  try {
    if (token) sessionStorage.setItem(REFRESH_KEY, token)
    else sessionStorage.removeItem(REFRESH_KEY)
  } catch {
    // private mode without storage: the session lasts one page load
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [session, setSession] = useState<SessionInfo | null>(null)
  const [ready, setReady] = useState(false)
  const token = useRef<string | null>(null)
  const refreshing = useRef<Promise<string | null> | null>(null)

  const apply = useCallback((s: Session) => {
    token.current = s.access_token
    setAccessToken(s.access_token)
    writeRefreshToken(s.refresh_token)
    setSession({ accountId: s.account_id, userId: s.user_id, role: s.role, expiresAt: Date.parse(s.expires_at) })
  }, [])

  const clear = useCallback(() => {
    token.current = null
    setAccessToken(null)
    writeRefreshToken(null)
    setSession(null)
  }, [])

  const refresh = useCallback((): Promise<string | null> => {
    if (refreshing.current) return refreshing.current
    const rt = readRefreshToken()
    if (!rt) {
      clear()
      return Promise.resolve(null)
    }
    const p = (async () => {
      try {
        const { data, response } = await client.POST('/v1/auth/refresh', { body: { refresh_token: rt } })
        if (!response.ok || !data) {
          clear()
          return null
        }
        apply(data)
        return data.access_token
      } catch {
        clear()
        return null
      } finally {
        refreshing.current = null
      }
    })()
    refreshing.current = p
    return p
  }, [apply, clear])

  useEffect(() => {
    setRefresher(refresh)
    return () => setRefresher(null)
  }, [refresh])

  // a reload: turn the stored refresh token into a session before rendering
  useEffect(() => {
    let cancelled = false
    void refresh().finally(() => {
      if (!cancelled) setReady(true)
    })
    return () => {
      cancelled = true
    }
  }, [refresh])

  // refresh a minute before the access token expires
  useEffect(() => {
    if (!session) return
    const wait = Math.max(session.expiresAt - Date.now() - 60_000, 5_000)
    const t = setTimeout(() => void refresh(), wait)
    return () => clearTimeout(t)
  }, [session, refresh])

  const login = useCallback(
    async (email: string, password: string) => {
      const s = await call(() => client.POST('/v1/auth/login', { body: { email, password } }))
      apply(s)
    },
    [apply],
  )

  const register = useCallback(
    async (email: string, password: string) => {
      const s = await call(() => client.POST('/v1/auth/register', { body: { email, password } }))
      apply(s)
    },
    [apply],
  )

  const logout = useCallback(async () => {
    const rt = readRefreshToken()
    clear()
    if (rt) {
      try {
        await client.POST('/v1/auth/logout', { body: { refresh_token: rt } })
      } catch {
        // the server-side family is revoked on next use anyway
      }
    }
  }, [clear])

  const accessToken = useCallback(
    async (force = false) => {
      if (!token.current && !readRefreshToken()) return null
      if (force || !token.current || (session && session.expiresAt - Date.now() < 30_000)) {
        return refresh()
      }
      return token.current
    },
    [refresh, session],
  )

  const value = useMemo<AuthValue>(
    () => ({ session, ready, login, register, logout, accessToken }),
    [session, ready, login, register, logout, accessToken],
  )
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthValue {
  const v = useContext(AuthContext)
  if (!v) throw new Error('useAuth outside AuthProvider')
  return v
}

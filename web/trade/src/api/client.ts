import createClient, { type Middleware } from 'openapi-fetch'
import type { Translate } from '../i18n/messages'
import { problemKey, statusKey } from '../i18n/problems'
import type { components, paths } from './schema'

export type Schemas = components['schemas']
export type Problem = Schemas['Problem']
export type Market = Schemas['Market']
export type Order = Schemas['Order']
export type Fill = Schemas['Fill']
export type Balance = Schemas['Balance']
export type Trade = Schemas['Trade']
export type Ticker = Schemas['Ticker']
export type Kline = Schemas['Kline']
export type KlineInterval = Schemas['KlineInterval']
export type Session = Schemas['Session']
export type Asset = Schemas['Asset']
export type Deposit = Schemas['Deposit']
export type Withdrawal = Schemas['Withdrawal']
export type DepositAddress = Schemas['DepositAddress']

// ApiError is what every failed call throws: the HTTP status and, when the
// server sent one, its RFC 7807 problem document.
export class ApiError extends Error {
  constructor(
    public readonly status: number,
    public readonly problem: Problem | null,
  ) {
    super(problem?.detail || problem?.title || `HTTP ${status}`)
    this.name = 'ApiError'
  }
}

// describeError turns a failed call into a sentence in the interface's
// language. A problem detail the catalogue knows is translated; any other
// detail is shown after the generic sentence for its status (in English
// the detail alone, which is what the interface showed before it had a
// second language); a fetch that never reached the server is a TypeError.
// ApiError.message keeps the English detail for logs.
export function describeError(err: unknown, t: Translate): string {
  if (err instanceof ApiError) {
    const detail = err.problem?.detail ?? ''
    const known = detail ? problemKey(detail) : undefined
    if (known) return t(known)
    const generic = t(statusKey(err.status))
    return detail ? t('error.with_detail', { generic, detail }) : generic
  }
  if (err instanceof TypeError) return t('error.network')
  if (err instanceof Error) return err.message
  return String(err)
}

// The access token lives in memory only (docs/plan-v1.0.md §8): a page
// reload gets a new one from the refresh token the session module keeps.
let accessToken: string | null = null
let refresher: (() => Promise<string | null>) | null = null

export function setAccessToken(token: string | null): void {
  accessToken = token
}

export function setRefresher(fn: (() => Promise<string | null>) | null): void {
  refresher = fn
}

const bearer: Middleware = {
  onRequest({ request }) {
    if (accessToken && !request.headers.has('Authorization')) {
      request.headers.set('Authorization', `Bearer ${accessToken}`)
    }
    return request
  },
}

export const client = createClient<paths>({ baseUrl: '/' })
client.use(bearer)

interface Result<T> {
  data?: T
  error?: unknown
  response: Response
}

// call runs one request, retries it once after refreshing an expired
// access token, and turns any non-2xx into an ApiError.
export async function call<T>(fn: () => Promise<Result<T>>): Promise<T> {
  let r = await fn()
  if (r.response.status === 401 && refresher && accessToken) {
    const fresh = await refresher()
    if (fresh) r = await fn()
  }
  if (!r.response.ok) {
    throw new ApiError(r.response.status, asProblem(r.error))
  }
  return r.data as T
}

function asProblem(v: unknown): Problem | null {
  if (v && typeof v === 'object' && 'title' in v && 'status' in v) return v as Problem
  return null
}

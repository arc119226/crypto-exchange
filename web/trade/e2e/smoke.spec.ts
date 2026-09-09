import { expect, test, type APIRequestContext, type Page } from '@playwright/test'

// Phase 6 DoD smoke (docs/plan-v1.0.md §12): register in the browser, fund
// through the admin faucet from Node, place a resting bid, watch the order
// book pick it up over the public stream, let a second account cross it
// through the REST API, and watch the fill, the balances and the open
// order's remaining quantity change over the private stream.
//
// The whole flow runs with the interface switched to Traditional Chinese
// (ADR-0010): every locator is a test id or a data attribute, so the same
// spec is valid in either language, and the amount assertions prove that
// prices and quantities are not reformatted by the switch. language.spec.ts
// covers the switch itself.
//
// Environment: API_URL (default http://localhost:8080), ADMIN_URL (default
// http://localhost:8082) and ADMIN_API_KEY (required) name the running
// stack; the browser itself only ever talks to the Vite preview server.

const API_URL = process.env.API_URL ?? 'http://localhost:8080'
const ADMIN_URL = process.env.ADMIN_URL ?? 'http://localhost:8082'
const ADMIN_API_KEY = process.env.ADMIN_API_KEY ?? ''
const MARKET = process.env.SMOKE_MARKET ?? 'ETH-USDC'

interface Session {
  account_id: string
  access_token: string
}

async function registerViaAPI(api: APIRequestContext, email: string, password: string): Promise<Session> {
  const r = await api.post(`${API_URL}/v1/auth/register`, { data: { email, password } })
  expect(r.status(), await r.text()).toBe(201)
  return (await r.json()) as Session
}

async function loginViaAPI(api: APIRequestContext, email: string, password: string): Promise<Session> {
  const r = await api.post(`${API_URL}/v1/auth/login`, { data: { email, password } })
  expect(r.status(), await r.text()).toBe(200)
  return (await r.json()) as Session
}

async function fund(api: APIRequestContext, accountId: string, asset: string, amount: string): Promise<void> {
  const r = await api.post(`${ADMIN_URL}/admin/v1/ledger/adjustments`, {
    headers: { 'X-Admin-Api-Key': ADMIN_API_KEY },
    data: {
      account_id: accountId,
      asset,
      amount,
      direction: 'credit',
      reason: 'web smoke faucet',
      idempotency_key: `web-smoke:${accountId}:${asset}:${Date.now()}`,
    },
  })
  expect([200, 201], await r.text()).toContain(r.status())
}

async function placeViaAPI(api: APIRequestContext, s: Session, body: Record<string, unknown>): Promise<Record<string, unknown>> {
  const r = await api.post(`${API_URL}/v1/orders`, {
    headers: { Authorization: `Bearer ${s.access_token}` },
    data: body,
  })
  expect(r.status(), await r.text()).toBe(201)
  return (await r.json()) as Record<string, unknown>
}

async function getJSON<T>(api: APIRequestContext, url: string): Promise<T> {
  const r = await api.get(url)
  expect(r.status(), await r.text()).toBe(200)
  return (await r.json()) as T
}

// trimZeros formats a price the way the API does (money.Amount drops
// trailing zeros), so the data-price prefix locators below match.
function trimZeros(v: string): string {
  return v.includes('.') ? v.replace(/\.?0+$/, '') : v
}

// bidPrice picks where the resting bid goes: SMOKE_PRICE when set, else one
// tick above the current best bid. That makes the order the best bid, so it
// is in the fifteen rows the book renders whatever else rests there (in CI
// the stack has just run a loadgen burst that leaves ~150 orders around
// 2000), its level is unique (nothing rests above the best bid), and the
// taker's sell at the same price fills it first. An empty book takes 1234.5.
async function bidPrice(api: APIRequestContext): Promise<string> {
  if (process.env.SMOKE_PRICE) return process.env.SMOKE_PRICE
  type Level = { price: string }
  const depth = await getJSON<{ bids: Level[]; asks: Level[] }>(api, `${API_URL}/v1/markets/${MARKET}/depth?limit=1`)
  const best = depth.bids[0]?.price
  if (!best) return '1234.5'
  const { markets } = await getJSON<{ markets: { symbol: string; price_tick: string }[] }>(api, `${API_URL}/v1/markets`)
  const tick = markets.find((m) => m.symbol === MARKET)?.price_tick ?? '0.01'
  const scale = (tick.split('.')[1] ?? '').length
  const units = (v: string): number => Math.round(Number(v) * 10 ** scale)
  const price = trimZeros(((units(best) + units(tick)) / 10 ** scale).toFixed(scale))
  const ask = depth.asks[0]?.price
  expect(!ask || units(price) < units(ask), `a one-tick spread (${best} / ${ask}) leaves no room for a resting best bid`).toBe(true)
  return price
}

async function available(page: Page, asset: string): Promise<string> {
  return (await page.getByTestId(`balance-${asset}`).getAttribute('data-available')) ?? ''
}

test.beforeAll(() => {
  test.skip(!ADMIN_API_KEY, 'ADMIN_API_KEY is required for the faucet')
})

test('register, place a bid, see it in the book, get filled, balances move', async ({ page, request }) => {
  const stamp = Date.now().toString(36)
  const makerEmail = `smoke-maker-${stamp}@example.com`
  const password = `Sm0ke-${stamp}-passw0rd`

  // switch to Traditional Chinese, then register through the browser ...
  await page.goto('/register')
  await page.getByTestId('lang-zh-TW').click()
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh-TW')
  await page.getByTestId('register-email').fill(makerEmail)
  await page.getByTestId('register-password').fill(password)
  await page.getByTestId('register-submit').click()
  await expect(page).toHaveURL(/\/markets$/)

  // ... fund from Node (the admin key stays out of the browser) ...
  const maker = await loginViaAPI(request, makerEmail, password)
  await fund(request, maker.account_id, 'USDC', '100000')
  await fund(request, maker.account_id, 'ETH', '10')

  // ... pick the bid's price from the book as it is ...
  const PRICE = await bidPrice(request)

  // ... and open the market
  await page.getByTestId(`market-${MARKET}`).click()
  await expect(page).toHaveURL(new RegExp(`/trade/${MARKET}$`))
  const book = page.getByTestId('order-book')
  await expect(book).toHaveAttribute('data-status', 'live')
  await expect(page.getByTestId('private-status')).toHaveAttribute('data-status', 'live')
  await expect(page.getByTestId('balance-USDC')).toHaveAttribute('data-available', /^100000/)
  const ethBefore = await available(page, 'ETH')

  // a resting bid that becomes the best bid (see bidPrice)
  await page.getByTestId('side-buy').click()
  await page.getByTestId('price').fill(PRICE)
  await page.getByTestId('qty').fill('0.5')
  await page.getByTestId('submit-order').click()
  await expect(page.getByTestId('order-result')).toHaveAttribute('data-status', 'open')

  // the public stream's delta puts the level into the book, the private
  // stream's order.accepted refreshes the open orders
  const bid = book.locator(`[data-testid="bid-row"][data-price^="${PRICE}"]`)
  await expect(bid).toBeVisible()
  await expect(bid.locator('td').nth(1)).toHaveText(/^0\.5/)
  const open = page.locator(`[data-testid="open-order"][data-price^="${PRICE}"]`)
  await expect(open).toBeVisible()
  await expect(page.getByTestId('balance-USDC')).not.toHaveAttribute('data-available', /^100000(\.0+)?$/)

  // a second account crosses it through the API
  const takerEmail = `smoke-taker-${stamp}@example.com`
  const taker = await registerViaAPI(request, takerEmail, password)
  await fund(request, taker.account_id, 'ETH', '1')
  const res = await placeViaAPI(request, taker, {
    client_order_id: `smoke-taker-${stamp}`,
    market: MARKET,
    side: 'sell',
    type: 'limit',
    price: PRICE,
    qty: '0.2',
  })
  expect((res.order as { status: string }).status).toBe('filled')

  // the fill shows up, the open order shrinks, the ETH balance grows
  const fill = page.locator(`[data-testid="fill-row"][data-price^="${PRICE}"]`)
  await expect(fill).toBeVisible()
  await expect(fill).toHaveAttribute('data-qty', /^0\.2/)
  await expect(open).toHaveAttribute('data-status', 'partially_filled')
  await expect(bid.locator('td').nth(1)).toHaveText(/^0\.3/)
  await expect
    .poll(async () => (await available(page, 'ETH')) !== ethBefore, { message: 'ETH balance changes after the fill' })
    .toBe(true)
  const trade = page.locator(`[data-testid="trade-row"][data-price^="${PRICE}"]`)
  await expect(trade.first()).toBeVisible()

  // cancel the rest from the browser: the level leaves the book
  await open.getByTestId('cancel-order').click()
  await expect(open).toHaveCount(0)
  await expect(bid).toHaveCount(0)
})

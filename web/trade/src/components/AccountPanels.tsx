import { useState } from 'react'
import { call, client, describeError, type Balance, type Fill, type Market, type Order } from '../api/client'
import { useResource } from '../hooks/useRefetch'
import { trimZeros } from '../lib/decimal'
import { formatTime, shortId } from '../lib/format'
import { useAccountEvents, usePrivateFeed } from '../ws/PrivateFeedProvider'

// The account panels take their initial state from REST and refetch when
// the private stream says something changed (docs/ws-api.md): orders,
// fills and balances frames are triggers; the lists themselves stay the
// server's, so nothing here reconstructs state from event payloads.

export function OpenOrders({ market, refreshKey }: { market: string; refreshKey: number }) {
  const { resyncVersion } = usePrivateFeed()
  const orders = useResource(
    () => call(() => client.GET('/v1/orders', { params: { query: { market, open_only: true, limit: 100 } } })).then((r) => r.orders),
    `${market}:${resyncVersion}:${refreshKey}`,
  )
  useAccountEvents(['orders'], () => orders.bump())
  const [error, setError] = useState<string | null>(null)

  const cancel = async (o: Order) => {
    setError(null)
    try {
      await call(() => client.DELETE('/v1/orders/{id}', { params: { path: { id: o.id } } }))
      orders.bump()
    } catch (err) {
      setError(describeError(err))
    }
  }

  return (
    <div className="card">
      <h2>Open orders</h2>
      {(orders.error || error) && <p className="error">{orders.error ?? error}</p>}
      <div className="scroll">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Side</th>
              <th>Price</th>
              <th>Qty</th>
              <th>Filled</th>
              <th>Status</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {(orders.data ?? []).map((o) => (
              <tr key={o.id} data-testid="open-order" data-order-id={o.id} data-price={o.price ?? ''} data-status={o.status}>
                <td className="muted">{formatTime(o.created_at)}</td>
                <td className={o.side}>{o.side}</td>
                <td>{o.price ? trimZeros(o.price) : 'market'}</td>
                <td>{o.qty ? trimZeros(o.qty) : o.quote_qty ? `${trimZeros(o.quote_qty)} quote` : ''}</td>
                <td>{trimZeros(o.filled_qty)}</td>
                <td>{o.status.replace('_', ' ')}</td>
                <td>
                  <button className="link" onClick={() => void cancel(o)} data-testid="cancel-order">
                    Cancel
                  </button>
                </td>
              </tr>
            ))}
            {orders.data && orders.data.length === 0 && (
              <tr>
                <td colSpan={7} className="muted">
                  No open orders
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}

export function FillsList({ market, refreshKey }: { market: string; refreshKey: number }) {
  const { resyncVersion } = usePrivateFeed()
  const fills = useResource(
    () => call(() => client.GET('/v1/fills', { params: { query: { market, limit: 50 } } })).then((r) => r.fills),
    `${market}:${resyncVersion}:${refreshKey}`,
  )
  useAccountEvents(['fills'], () => fills.bump())
  return (
    <div className="card">
      <h2>My fills</h2>
      {fills.error && <p className="error">{fills.error}</p>}
      <div className="scroll">
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Side</th>
              <th>Price</th>
              <th>Qty</th>
              <th>Fee</th>
              <th>Role</th>
              <th>Order</th>
            </tr>
          </thead>
          <tbody>
            {(fills.data ?? []).map((f: Fill) => (
              <tr key={`${f.trade_id}:${f.order_id}`} data-testid="fill-row" data-price={f.price} data-qty={f.qty}>
                <td className="muted">{formatTime(f.executed_at)}</td>
                <td className={f.side}>{f.side}</td>
                <td>{trimZeros(f.price)}</td>
                <td>{trimZeros(f.qty)}</td>
                <td className="muted">
                  {trimZeros(f.fee)} {f.fee_asset}
                </td>
                <td className="muted">{f.is_maker ? 'maker' : 'taker'}</td>
                <td className="muted">{shortId(f.order_id)}</td>
              </tr>
            ))}
            {fills.data && fills.data.length === 0 && (
              <tr>
                <td colSpan={7} className="muted">
                  No fills yet
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}

export function Balances({ market, refreshKey }: { market: Market | null; refreshKey: number }) {
  const { resyncVersion } = usePrivateFeed()
  const balances = useResource(() => call(() => client.GET('/v1/balances')).then((r) => r.balances), `${resyncVersion}:${refreshKey}`)
  useAccountEvents(['balances', 'deposits', 'withdrawals'], () => balances.bump())
  const rows = [...(balances.data ?? [])].sort((a: Balance, b: Balance) => rank(a.asset, market) - rank(b.asset, market) || a.asset.localeCompare(b.asset))
  return (
    <div className="card">
      <h2>Balances</h2>
      {balances.error && <p className="error">{balances.error}</p>}
      <table>
        <thead>
          <tr>
            <th>Asset</th>
            <th>Available</th>
            <th>On hold</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((b) => (
            <tr key={b.asset} data-testid={`balance-${b.asset}`} data-available={b.available} data-hold={b.hold}>
              <td>{b.asset}</td>
              <td>{trimZeros(b.available)}</td>
              <td className="muted">{trimZeros(b.hold)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function rank(asset: string, m: Market | null): number {
  if (!m) return 1
  if (asset === m.base_asset) return 0
  if (asset === m.quote_asset) return 0
  return 1
}

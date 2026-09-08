import { useEffect, useState, type FormEvent } from 'react'
import { call, client, describeError, type Asset, type DepositAddress } from '../api/client'
import { useResource } from '../hooks/useRefetch'
import { trimZeros } from '../lib/decimal'
import { formatDateTime, idempotencyKey, shortId } from '../lib/format'
import { useAccountEvents, usePrivateFeed } from '../ws/PrivateFeedProvider'

// WalletPage: deposit addresses, deposits, withdrawals. Deposit and
// withdrawal frames on the private stream refetch the lists and the
// balances (the balance change itself is a ledger posting, which has no
// event of its own -- docs/domain.md §25).
export function WalletPage() {
  const { resyncVersion } = usePrivateFeed()
  const assets = useResource(() => call(() => client.GET('/v1/assets')).then((r) => r.assets), 'assets')
  const balances = useResource(() => call(() => client.GET('/v1/balances')).then((r) => r.balances), `balances:${resyncVersion}`)
  const deposits = useResource(() => call(() => client.GET('/v1/deposits', { params: { query: { limit: 50 } } })).then((r) => r.deposits), `deposits:${resyncVersion}`)
  const withdrawals = useResource(() => call(() => client.GET('/v1/withdrawals', { params: { query: { limit: 50 } } })).then((r) => r.withdrawals), `withdrawals:${resyncVersion}`)
  useAccountEvents(['deposits', 'balances'], () => {
    deposits.bump()
    balances.bump()
  })
  useAccountEvents(['withdrawals'], () => {
    withdrawals.bump()
    balances.bump()
  })

  const [asset, setAsset] = useState('')
  useEffect(() => {
    if (!asset && assets.data?.[0]) setAsset(assets.data[0].symbol)
  }, [assets.data, asset])

  return (
    <div className="page stack">
      <div className="row" style={{ alignItems: 'flex-start' }}>
        <div className="card" style={{ flex: 1 }}>
          <h2>Balances</h2>
          {balances.error && <p className="error">{balances.error}</p>}
          <table>
            <thead>
              <tr>
                <th>Asset</th>
                <th>Available</th>
                <th>On hold</th>
                <th>Total</th>
              </tr>
            </thead>
            <tbody>
              {(balances.data ?? []).map((b) => (
                <tr key={b.asset} data-testid={`wallet-balance-${b.asset}`}>
                  <td>{b.asset}</td>
                  <td>{trimZeros(b.available)}</td>
                  <td className="muted">{trimZeros(b.hold)}</td>
                  <td>{trimZeros(b.total)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <DepositPanel assets={assets.data ?? []} asset={asset} setAsset={setAsset} />
        <WithdrawPanel assets={assets.data ?? []} onCreated={() => withdrawals.bump()} />
      </div>
      <div className="card">
        <h2>Deposits</h2>
        {deposits.error && <p className="error">{deposits.error}</p>}
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Asset</th>
              <th>Amount</th>
              <th>Confirmations</th>
              <th>Status</th>
              <th>Tx</th>
            </tr>
          </thead>
          <tbody>
            {(deposits.data ?? []).map((d) => (
              <tr key={d.id} data-testid="deposit-row">
                <td className="muted">{formatDateTime(d.created_at)}</td>
                <td>{d.asset}</td>
                <td>{trimZeros(d.amount)}</td>
                <td className="muted">
                  {d.confirmations}/{d.required_confirmations}
                </td>
                <td>{d.status}</td>
                <td className="muted">
                  <code>{shortId(d.tx_hash)}</code>
                </td>
              </tr>
            ))}
            {deposits.data && deposits.data.length === 0 && (
              <tr>
                <td colSpan={6} className="muted">
                  No deposits yet
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
      <div className="card">
        <h2>Withdrawals</h2>
        {withdrawals.error && <p className="error">{withdrawals.error}</p>}
        <table>
          <thead>
            <tr>
              <th>Time</th>
              <th>Asset</th>
              <th>Amount</th>
              <th>To</th>
              <th>Status</th>
              <th>Tx</th>
            </tr>
          </thead>
          <tbody>
            {(withdrawals.data ?? []).map((w) => (
              <tr key={w.id} data-testid="withdrawal-row" data-status={w.status}>
                <td className="muted">{formatDateTime(w.created_at)}</td>
                <td>{w.asset}</td>
                <td>{trimZeros(w.amount)}</td>
                <td className="muted">
                  <code>{shortId(w.to_address)}</code>
                </td>
                <td>
                  {w.status}
                  {w.failure_reason ? <span className="error"> · {w.failure_reason}</span> : null}
                </td>
                <td className="muted">{w.tx_hash ? <code>{shortId(w.tx_hash)}</code> : '—'}</td>
              </tr>
            ))}
            {withdrawals.data && withdrawals.data.length === 0 && (
              <tr>
                <td colSpan={6} className="muted">
                  No withdrawals yet
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function DepositPanel({ assets, asset, setAsset }: { assets: Asset[]; asset: string; setAsset: (a: string) => void }) {
  const [address, setAddress] = useState<DepositAddress | null>(null)
  const [error, setError] = useState<string | null>(null)
  const info = assets.find((a) => a.symbol === asset)
  useEffect(() => {
    if (!asset) return
    let cancelled = false
    setAddress(null)
    setError(null)
    call(() => client.GET('/v1/deposit-address', { params: { query: { asset } } }))
      .then((a) => {
        if (!cancelled) setAddress(a)
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(describeError(err))
      })
    return () => {
      cancelled = true
    }
  }, [asset])
  return (
    <div className="card stack" style={{ flex: 1 }}>
      <h2>Deposit</h2>
      <div className="field">
        <label htmlFor="deposit-asset">Asset</label>
        <select id="deposit-asset" value={asset} onChange={(e) => setAsset(e.target.value)}>
          {assets.map((a) => (
            <option key={a.symbol} value={a.symbol} disabled={!a.deposit_enabled}>
              {a.symbol} {a.deposit_enabled ? '' : '(deposits disabled)'}
            </option>
          ))}
        </select>
      </div>
      {error && <p className="error">{error}</p>}
      {address && (
        <p>
          Send {asset} on chain {address.chain_id} to <code data-testid="deposit-address">{address.address}</code>
        </p>
      )}
      {info && (
        <p className="muted">
          Minimum {trimZeros(info.min_deposit)} {info.symbol}; credited after {info.required_confirmations} confirmations.
        </p>
      )}
    </div>
  )
}

function WithdrawPanel({ assets, onCreated }: { assets: Asset[]; onCreated: () => void }) {
  const [asset, setAsset] = useState('')
  const [amount, setAmount] = useState('')
  const [to, setTo] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [ok, setOk] = useState<string | null>(null)
  useEffect(() => {
    if (!asset && assets[0]) setAsset(assets[0].symbol)
  }, [assets, asset])
  const info = assets.find((a) => a.symbol === asset)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setError(null)
    setOk(null)
    setBusy(true)
    try {
      const w = await call(() =>
        client.POST('/v1/withdrawals', {
          params: { header: { 'Idempotency-Key': idempotencyKey() } },
          body: { asset, amount, to_address: to },
        }),
      )
      setOk(`Withdrawal ${shortId(w.id)} ${w.status}`)
      setAmount('')
      setTo('')
      onCreated()
    } catch (err) {
      setError(describeError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="card stack" style={{ flex: 1 }} onSubmit={submit} data-testid="withdraw-form">
      <h2>Withdraw</h2>
      <div className="field">
        <label htmlFor="withdraw-asset">Asset</label>
        <select id="withdraw-asset" value={asset} onChange={(e) => setAsset(e.target.value)}>
          {assets.map((a) => (
            <option key={a.symbol} value={a.symbol} disabled={!a.withdraw_enabled}>
              {a.symbol} {a.withdraw_enabled ? '' : '(withdrawals disabled)'}
            </option>
          ))}
        </select>
      </div>
      <div className="field">
        <label htmlFor="withdraw-amount">Amount{info ? ` (min ${trimZeros(info.min_withdrawal)}, fee ${trimZeros(info.withdrawal_fee)} ${info.symbol})` : ''}</label>
        <input id="withdraw-amount" inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} required />
      </div>
      <div className="field">
        <label htmlFor="withdraw-to">To address</label>
        <input id="withdraw-to" value={to} onChange={(e) => setTo(e.target.value)} required placeholder="0x…" />
      </div>
      {error && <p className="error" role="alert">{error}</p>}
      {ok && <p className="ok">{ok}</p>}
      <button type="submit" className="primary" disabled={busy || !asset}>
        Request withdrawal
      </button>
    </form>
  )
}

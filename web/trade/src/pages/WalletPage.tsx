import { useEffect, useState, type FormEvent } from 'react'
import { call, client, describeError, type Asset, type DepositAddress } from '../api/client'
import { useAuth } from '../auth/session'
import { useResource } from '../hooks/useRefetch'
import { enumLabel } from '../i18n/enums'
import { useLocale } from '../i18n/LocaleProvider'
import { add, format, mustDec, parseDec, trimZeros, withdrawalFee } from '../lib/decimal'
import { formatDateTime, idempotencyKey, shortId } from '../lib/format'
import { useAccountEvents, usePrivateFeed } from '../ws/PrivateFeedProvider'

// WalletPage: deposit addresses, deposits, withdrawals. Deposit and
// withdrawal frames on the private stream refetch the lists and the
// balances (the balance change itself is a ledger posting, which has no
// event of its own -- docs/domain.md §25).
export function WalletPage() {
  const { locale, intl, t } = useLocale()
  const { session } = useAuth()
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
      {/* the full id: the top bar shows only its first characters, and the
          dev faucet (make faucet ACCOUNT=...) needs all of it */}
      <p className="muted" style={{ margin: 0 }}>
        {t('wallet.account_id')} <code data-testid="account-id">{session?.accountId ?? ''}</code>
      </p>
      <div className="row" style={{ alignItems: 'flex-start' }}>
        <div className="card" style={{ flex: 1 }}>
          <h2>{t('balances.title')}</h2>
          {balances.error && <p className="error">{balances.error}</p>}
          <table>
            <thead>
              <tr>
                <th>{t('col.asset')}</th>
                <th>{t('balances.col.available')}</th>
                <th>{t('balances.col.hold')}</th>
                <th>{t('balances.col.total')}</th>
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
        <h2>{t('wallet.deposits')}</h2>
        {deposits.error && <p className="error">{deposits.error}</p>}
        <table>
          <thead>
            <tr>
              <th>{t('col.time')}</th>
              <th>{t('col.asset')}</th>
              <th>{t('col.amount')}</th>
              <th>{t('wallet.col.confirmations')}</th>
              <th>{t('col.status')}</th>
              <th>{t('col.tx')}</th>
            </tr>
          </thead>
          <tbody>
            {(deposits.data ?? []).map((d) => (
              <tr key={d.id} data-testid="deposit-row">
                <td className="muted">{formatDateTime(d.created_at, intl)}</td>
                <td>{d.asset}</td>
                <td>{trimZeros(d.amount)}</td>
                <td className="muted">
                  {d.confirmations}/{d.required_confirmations}
                </td>
                <td>{enumLabel(locale, 'deposit', d.status)}</td>
                <td className="muted">
                  <code>{shortId(d.tx_hash)}</code>
                </td>
              </tr>
            ))}
            {deposits.data && deposits.data.length === 0 && (
              <tr>
                <td colSpan={6} className="muted">
                  {t('wallet.deposits_empty')}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
      <div className="card">
        <h2>{t('wallet.withdrawals')}</h2>
        {withdrawals.error && <p className="error">{withdrawals.error}</p>}
        <table>
          <thead>
            <tr>
              <th>{t('col.time')}</th>
              <th>{t('col.asset')}</th>
              <th>{t('col.amount')}</th>
              <th>{t('wallet.col.to')}</th>
              <th>{t('col.status')}</th>
              <th>{t('col.tx')}</th>
            </tr>
          </thead>
          <tbody>
            {(withdrawals.data ?? []).map((w) => (
              <tr key={w.id} data-testid="withdrawal-row" data-status={w.status}>
                <td className="muted">{formatDateTime(w.created_at, intl)}</td>
                <td>{w.asset}</td>
                <td>{trimZeros(w.amount)}</td>
                <td className="muted">
                  <code>{shortId(w.to_address)}</code>
                </td>
                <td>
                  {enumLabel(locale, 'withdrawal', w.status)}
                  {w.failure_reason ? <span className="error"> · {w.failure_reason}</span> : null}
                </td>
                <td className="muted">{w.tx_hash ? <code>{shortId(w.tx_hash)}</code> : '—'}</td>
              </tr>
            ))}
            {withdrawals.data && withdrawals.data.length === 0 && (
              <tr>
                <td colSpan={6} className="muted">
                  {t('wallet.withdrawals_empty')}
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
  const { t } = useLocale()
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
        if (!cancelled) setError(describeError(err, t))
      })
    return () => {
      cancelled = true
    }
    // one request per asset; a language switch re-words an error on the next one
  }, [asset])
  return (
    <div className="card stack" style={{ flex: 1 }}>
      <h2>{t('wallet.deposit')}</h2>
      <div className="field">
        <label htmlFor="deposit-asset">{t('col.asset')}</label>
        <select id="deposit-asset" value={asset} onChange={(e) => setAsset(e.target.value)}>
          {assets.map((a) => (
            <option key={a.symbol} value={a.symbol} disabled={!a.deposit_enabled}>
              {a.symbol} {a.deposit_enabled ? '' : t('wallet.deposits_disabled')}
            </option>
          ))}
        </select>
      </div>
      {error && <p className="error">{error}</p>}
      {address && (
        <p>
          {t('wallet.send_to', { asset, chain: address.chain_id })} <code data-testid="deposit-address">{address.address}</code>
        </p>
      )}
      {info && <p className="muted">{t('wallet.deposit_min', { min: trimZeros(info.min_deposit), asset: info.symbol, confirmations: info.required_confirmations })}</p>}
    </div>
  )
}

function WithdrawPanel({ assets, onCreated }: { assets: Asset[]; onCreated: () => void }) {
  const { locale, t } = useLocale()
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

  // What the account will actually be debited, once the typed amount parses.
  // The fee is charged on top (§23.3), so the destination receives `amount`
  // and the balance falls by amount + fee. The server snapshots its own
  // quote on the row; this is the same formula, shown before the click.
  const parsed = info ? parseDec(amount.trim()) : null
  const quote =
    info && parsed && parsed.n > 0n
      ? (() => {
          const fee = withdrawalFee(parsed, mustDec(info.withdrawal_fee), info.withdrawal_fee_bps, info.scale)
          return { fee: trimZeros(format(fee)), total: trimZeros(format(add(parsed, fee))) }
        })()
      : null

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
      setOk(t('wallet.withdrawal_created', { id: shortId(w.id), status: enumLabel(locale, 'withdrawal', w.status) }))
      setAmount('')
      setTo('')
      onCreated()
    } catch (err) {
      setError(describeError(err, t))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="card stack" style={{ flex: 1 }} onSubmit={submit} data-testid="withdraw-form">
      <h2>{t('wallet.withdraw')}</h2>
      <div className="field">
        <label htmlFor="withdraw-asset">{t('col.asset')}</label>
        <select id="withdraw-asset" value={asset} onChange={(e) => setAsset(e.target.value)}>
          {assets.map((a) => (
            <option key={a.symbol} value={a.symbol} disabled={!a.withdraw_enabled}>
              {a.symbol} {a.withdraw_enabled ? '' : t('wallet.withdrawals_disabled')}
            </option>
          ))}
        </select>
      </div>
      <div className="field">
        <label htmlFor="withdraw-amount">
          {info ? t('wallet.amount_label', { min: trimZeros(info.min_withdrawal), asset: info.symbol }) : t('wallet.amount')}
        </label>
        <input id="withdraw-amount" inputMode="decimal" value={amount} onChange={(e) => setAmount(e.target.value)} required />
        {info && (
          <p className="muted" data-testid="withdraw-cost">
            {quote
              ? t('wallet.fee_total', { fee: quote.fee, total: quote.total, asset: info.symbol })
              : t('wallet.fee_rate', { fee: trimZeros(info.withdrawal_fee), bps: String(info.withdrawal_fee_bps), asset: info.symbol })}
          </p>
        )}
      </div>
      <div className="field">
        <label htmlFor="withdraw-to">{t('wallet.to_address')}</label>
        <input id="withdraw-to" value={to} onChange={(e) => setTo(e.target.value)} required placeholder="0x…" />
      </div>
      {error && <p className="error" role="alert">{error}</p>}
      {ok && <p className="ok">{ok}</p>}
      <button type="submit" className="primary" disabled={busy || !asset}>
        {t('wallet.request_withdrawal')}
      </button>
    </form>
  )
}

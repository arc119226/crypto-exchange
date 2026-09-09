-- The revenue report (docs/plan-v1.0.md 23.5): the three fee sources and the
-- gas they cost, per asset, over a half-open period [from, to). Read by the
-- admin role, which already has SELECT on trading.trades (migration 0005) and
-- on every ledger table (migration 0003), so this adds no grant.
--
-- The trading fee is read from trading.trades and deliberately NOT from the
-- ledger. A settle entry credits fee_revenue with the same two numbers the
-- trade row carries (internal/ledger/settle.go), so summing both would double
-- the trading fee. Only the trade row knows which side was the maker, which
-- the report has to show separately -- and that settles which source wins.
-- The ledger half below therefore excludes kind 'settle'.
--
-- Two clocks meet here and they are not the same clock: trading.trades.created_at
-- is the engine's command timestamp, ledger.journal_entries.created_at is the
-- database's now(). A withdrawal is also counted when its fee was booked --
-- at confirmation, not when the user asked -- so one that spans midnight falls
-- in the later period. Both facts are stated in the endpoint description
-- rather than left to be inferred from here.

-- name: RevenueTradingFees :many
-- Maker and taker fees per fee asset. A trade's two fees are charged in
-- different assets (the base and the quote), so one trade contributes a leg to
-- two asset rows, and `trades` counts the trades charged in this asset --
-- including the ones charged zero, which is every trade until an operator sets
-- a rate.
SELECT f.asset::text                          AS asset,
       SUM(f.maker_fee)::numeric(36,18)       AS maker_fees,
       SUM(f.taker_fee)::numeric(36,18)       AS taker_fees,
       count(*)::bigint                       AS trades
  FROM (
        SELECT t.maker_fee_asset AS asset, t.maker_fee AS maker_fee, 0::numeric(36,18) AS taker_fee
          FROM trading.trades t
         WHERE t.tenant_id = sqlc.arg(tenant_id)::text
           AND t.created_at >= sqlc.arg(from_time)::timestamptz
           AND t.created_at <  sqlc.arg(to_time)::timestamptz
        UNION ALL
        SELECT t.taker_fee_asset, 0::numeric(36,18), t.taker_fee
          FROM trading.trades t
         WHERE t.tenant_id = sqlc.arg(tenant_id)::text
           AND t.created_at >= sqlc.arg(from_time)::timestamptz
           AND t.created_at <  sqlc.arg(to_time)::timestamptz
       ) f
 WHERE (sqlc.arg(asset)::text = '' OR f.asset = sqlc.arg(asset)::text)
 GROUP BY f.asset
 ORDER BY f.asset;

-- name: RevenueFeeRevenue :many
-- credit minus debit on fee_revenue per (asset, entry kind). Kind 'fee' is the
-- withdrawal fee, an entry of its own posted only at confirmation; kind
-- 'deposit' is the deposit fee, a third posting inside the credit entry rather
-- than an entry of its own -- which is why this filters on the account rather
-- than on the entry. Kind 'settle' is excluded: those are the trading fees.
--
-- The kind is returned rather than filtered to the two known values, so a
-- credit from anywhere else -- an operator adjustment against fee_revenue,
-- say -- shows up in the report's `other` column instead of vanishing.
--
-- credit minus debit rather than SUM(credit), so a reversing entry subtracts
-- instead of being ignored.
SELECT p.asset,
       e.kind,
       (SUM(CASE p.direction WHEN 'credit' THEN p.amount ELSE 0 END)
      - SUM(CASE p.direction WHEN 'debit'  THEN p.amount ELSE 0 END))::numeric(36,18) AS net,
       count(DISTINCT e.id)::bigint AS entries
  FROM ledger.postings p
  JOIN ledger.journal_entries e ON e.id = p.entry_id
  JOIN ledger.accounts a ON a.id = p.account_id
 WHERE e.tenant_id = sqlc.arg(tenant_id)::text
   AND a.house_code = 'fee_revenue'
   AND e.kind <> 'settle'
   AND e.created_at >= sqlc.arg(from_time)::timestamptz
   AND e.created_at <  sqlc.arg(to_time)::timestamptz
   AND (sqlc.arg(asset)::text = '' OR p.asset = sqlc.arg(asset)::text)
 GROUP BY p.asset, e.kind
 ORDER BY p.asset, e.kind;

-- name: RevenueGasExpense :many
-- debit minus credit on gas_expense per asset. Gas is always the chain's
-- native coin, never the asset withdrawn, so an ERC-20 row shows revenue with
-- no gas and the native row shows gas against only its own withdrawals' fees.
-- 23.5 forbids converting between assets, so the net column is only meaningful
-- within one asset, and that asymmetry is the reason why.
--
-- No kind filter: sweeps and nonce fills book gas too, and 23.5 defines this
-- column as every gas_expense debit. Filtering on the account keeps that true
-- if another path starts booking gas later.
SELECT p.asset,
       (SUM(CASE p.direction WHEN 'debit'  THEN p.amount ELSE 0 END)
      - SUM(CASE p.direction WHEN 'credit' THEN p.amount ELSE 0 END))::numeric(36,18) AS net,
       count(DISTINCT e.id)::bigint AS entries
  FROM ledger.postings p
  JOIN ledger.journal_entries e ON e.id = p.entry_id
  JOIN ledger.accounts a ON a.id = p.account_id
 WHERE e.tenant_id = sqlc.arg(tenant_id)::text
   AND a.house_code = 'gas_expense'
   AND e.created_at >= sqlc.arg(from_time)::timestamptz
   AND e.created_at <  sqlc.arg(to_time)::timestamptz
   AND (sqlc.arg(asset)::text = '' OR p.asset = sqlc.arg(asset)::text)
 GROUP BY p.asset
 ORDER BY p.asset;

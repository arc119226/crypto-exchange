-- Registry queries. Always schema-qualified; never rely on search_path.

-- name: ListAssets :many
SELECT * FROM registry.assets
WHERE tenant_id = $1
ORDER BY symbol;

-- name: GetAssetBySymbol :one
SELECT * FROM registry.assets
WHERE tenant_id = $1 AND symbol = $2;

-- name: ListMarkets :many
SELECT m.*,
       b.symbol AS base_symbol,
       b.scale  AS base_scale,
       q.symbol AS quote_symbol,
       q.scale  AS quote_scale,
       f.name   AS fee_schedule_name,
       f.maker_bps,
       f.taker_bps
FROM registry.markets m
JOIN registry.assets b ON b.id = m.base_asset_id
JOIN registry.assets q ON q.id = m.quote_asset_id
JOIN registry.fee_schedules f ON f.id = m.fee_schedule_id
WHERE m.tenant_id = $1
ORDER BY m.symbol;

-- name: GetMarketBySymbol :one
SELECT m.*,
       b.symbol AS base_symbol,
       b.scale  AS base_scale,
       q.symbol AS quote_symbol,
       q.scale  AS quote_scale,
       f.name   AS fee_schedule_name,
       f.maker_bps,
       f.taker_bps
FROM registry.markets m
JOIN registry.assets b ON b.id = m.base_asset_id
JOIN registry.assets q ON q.id = m.quote_asset_id
JOIN registry.fee_schedules f ON f.id = m.fee_schedule_id
WHERE m.tenant_id = $1 AND m.symbol = $2;

-- name: GetFeeScheduleByName :one
SELECT * FROM registry.fee_schedules
WHERE tenant_id = $1 AND name = $2;

-- Upserts bump version/updated_at only when something actually changed, so
-- re-running the seed is a true no-op.

-- name: UpsertFeeSchedule :exec
INSERT INTO registry.fee_schedules (tenant_id, name, maker_bps, taker_bps)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, name) DO UPDATE
SET maker_bps  = EXCLUDED.maker_bps,
    taker_bps  = EXCLUDED.taker_bps,
    version    = registry.fee_schedules.version + 1,
    updated_at = now()
WHERE (registry.fee_schedules.maker_bps, registry.fee_schedules.taker_bps)
      IS DISTINCT FROM (EXCLUDED.maker_bps, EXCLUDED.taker_bps);

-- name: UpsertAsset :exec
INSERT INTO registry.assets (
    tenant_id, symbol, name, chain_id, contract_address, is_native, scale, display_scale,
    required_confirmations, min_deposit, min_withdrawal, withdrawal_fee, sweep_threshold,
    deposit_enabled, withdraw_enabled, status
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (tenant_id, symbol) DO UPDATE
SET name                   = EXCLUDED.name,
    chain_id               = EXCLUDED.chain_id,
    contract_address       = EXCLUDED.contract_address,
    is_native              = EXCLUDED.is_native,
    scale                  = EXCLUDED.scale,
    display_scale          = EXCLUDED.display_scale,
    required_confirmations = EXCLUDED.required_confirmations,
    min_deposit            = EXCLUDED.min_deposit,
    min_withdrawal         = EXCLUDED.min_withdrawal,
    withdrawal_fee         = EXCLUDED.withdrawal_fee,
    sweep_threshold        = EXCLUDED.sweep_threshold,
    deposit_enabled        = EXCLUDED.deposit_enabled,
    withdraw_enabled       = EXCLUDED.withdraw_enabled,
    status                 = EXCLUDED.status,
    version                = registry.assets.version + 1,
    updated_at             = now()
WHERE (registry.assets.name, registry.assets.chain_id, registry.assets.contract_address, registry.assets.is_native,
       registry.assets.scale, registry.assets.display_scale, registry.assets.required_confirmations,
       registry.assets.min_deposit, registry.assets.min_withdrawal, registry.assets.withdrawal_fee,
       registry.assets.sweep_threshold, registry.assets.deposit_enabled, registry.assets.withdraw_enabled,
       registry.assets.status)
      IS DISTINCT FROM
      (EXCLUDED.name, EXCLUDED.chain_id, EXCLUDED.contract_address, EXCLUDED.is_native,
       EXCLUDED.scale, EXCLUDED.display_scale, EXCLUDED.required_confirmations,
       EXCLUDED.min_deposit, EXCLUDED.min_withdrawal, EXCLUDED.withdrawal_fee,
       EXCLUDED.sweep_threshold, EXCLUDED.deposit_enabled, EXCLUDED.withdraw_enabled,
       EXCLUDED.status);

-- name: UpsertMarket :exec
INSERT INTO registry.markets (
    tenant_id, symbol, base_asset_id, quote_asset_id, price_tick, qty_step, min_notional,
    max_qty, max_slippage_bps, fee_schedule_id, self_trade_policy, status
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (tenant_id, symbol) DO UPDATE
SET base_asset_id     = EXCLUDED.base_asset_id,
    quote_asset_id    = EXCLUDED.quote_asset_id,
    price_tick        = EXCLUDED.price_tick,
    qty_step          = EXCLUDED.qty_step,
    min_notional      = EXCLUDED.min_notional,
    max_qty           = EXCLUDED.max_qty,
    max_slippage_bps  = EXCLUDED.max_slippage_bps,
    fee_schedule_id   = EXCLUDED.fee_schedule_id,
    self_trade_policy = EXCLUDED.self_trade_policy,
    status            = EXCLUDED.status,
    version           = registry.markets.version + 1,
    updated_at        = now()
WHERE (registry.markets.base_asset_id, registry.markets.quote_asset_id, registry.markets.price_tick,
       registry.markets.qty_step, registry.markets.min_notional, registry.markets.max_qty,
       registry.markets.max_slippage_bps, registry.markets.fee_schedule_id,
       registry.markets.self_trade_policy, registry.markets.status)
      IS DISTINCT FROM
      (EXCLUDED.base_asset_id, EXCLUDED.quote_asset_id, EXCLUDED.price_tick,
       EXCLUDED.qty_step, EXCLUDED.min_notional, EXCLUDED.max_qty,
       EXCLUDED.max_slippage_bps, EXCLUDED.fee_schedule_id,
       EXCLUDED.self_trade_policy, EXCLUDED.status);

-- name: SetMarketStatus :exec
-- Status changes are the one registry write the admin API makes on a live
-- market; version and updated_at move only when the status really changes so
-- an idempotent replay does not emit a new event.
UPDATE registry.markets
   SET status     = $3,
       version    = version + 1,
       updated_at = now()
 WHERE tenant_id = $1 AND symbol = $2 AND status IS DISTINCT FROM $3;

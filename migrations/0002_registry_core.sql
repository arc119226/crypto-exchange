-- +goose Up
-- Registry: assets, markets, fee schedules and withdrawal limits
-- (docs/plan-v1.0.md §6.6). Amounts are NUMERIC(36,18); only ex_admin and
-- ex_all may write, nobody may DELETE.

CREATE TABLE registry.fee_schedules (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      text        NOT NULL DEFAULT 'default',
    name           text        NOT NULL,
    maker_bps      integer     NOT NULL CHECK (maker_bps BETWEEN 0 AND 10000),
    taker_bps      integer     NOT NULL CHECK (taker_bps BETWEEN 0 AND 10000),
    effective_from timestamptz NOT NULL DEFAULT now(),
    version        integer     NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE registry.assets (
    id                     uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              text           NOT NULL DEFAULT 'default',
    symbol                 text           NOT NULL CHECK (symbol ~ '^[A-Z0-9]{2,16}$'),
    name                   text           NOT NULL,
    chain_id               bigint         NOT NULL,
    contract_address       text           CHECK (contract_address ~ '^0x[0-9a-fA-F]{40}$'),
    is_native              boolean        NOT NULL,
    scale                  smallint       NOT NULL CHECK (scale BETWEEN 0 AND 18),
    display_scale          smallint       NOT NULL CHECK (display_scale BETWEEN 0 AND scale),
    required_confirmations integer        NOT NULL CHECK (required_confirmations > 0),
    min_deposit            numeric(36,18) NOT NULL DEFAULT 0 CHECK (min_deposit >= 0),
    min_withdrawal         numeric(36,18) NOT NULL DEFAULT 0 CHECK (min_withdrawal >= 0),
    withdrawal_fee         numeric(36,18) NOT NULL DEFAULT 0 CHECK (withdrawal_fee >= 0),
    sweep_threshold        numeric(36,18) NOT NULL DEFAULT 0 CHECK (sweep_threshold >= 0),
    deposit_enabled        boolean        NOT NULL DEFAULT true,
    withdraw_enabled       boolean        NOT NULL DEFAULT true,
    status                 text           NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    version                integer        NOT NULL DEFAULT 1,
    created_at             timestamptz    NOT NULL DEFAULT now(),
    updated_at             timestamptz    NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, symbol),
    UNIQUE NULLS NOT DISTINCT (tenant_id, chain_id, contract_address),
    CHECK ((is_native AND contract_address IS NULL) OR (NOT is_native AND contract_address IS NOT NULL))
);

CREATE TABLE registry.markets (
    id                uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         text           NOT NULL DEFAULT 'default',
    symbol            text           NOT NULL CHECK (symbol ~ '^[A-Z0-9]{2,16}-[A-Z0-9]{2,16}$'),
    base_asset_id     uuid           NOT NULL REFERENCES registry.assets (id),
    quote_asset_id    uuid           NOT NULL REFERENCES registry.assets (id),
    price_tick        numeric(36,18) NOT NULL CHECK (price_tick > 0),
    qty_step          numeric(36,18) NOT NULL CHECK (qty_step > 0),
    min_notional      numeric(36,18) NOT NULL CHECK (min_notional >= 0),
    max_qty           numeric(36,18) CHECK (max_qty IS NULL OR max_qty > 0),
    max_slippage_bps  integer        CHECK (max_slippage_bps IS NULL OR max_slippage_bps BETWEEN 1 AND 10000),
    fee_schedule_id   uuid           NOT NULL REFERENCES registry.fee_schedules (id),
    self_trade_policy text           NOT NULL DEFAULT 'cancel_newest'
                                     CHECK (self_trade_policy IN ('cancel_newest', 'allow', 'cancel_oldest')),
    status            text           NOT NULL DEFAULT 'active'
                                     CHECK (status IN ('active', 'halted', 'cancel_only', 'delisted')),
    version           integer        NOT NULL DEFAULT 1,
    created_at        timestamptz    NOT NULL DEFAULT now(),
    updated_at        timestamptz    NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, symbol),
    CHECK (base_asset_id <> quote_asset_id)
);

CREATE TABLE registry.withdrawal_limits (
    tenant_id             text           NOT NULL DEFAULT 'default',
    asset_id              uuid           NOT NULL REFERENCES registry.assets (id),
    kyc_level             smallint       NOT NULL CHECK (kyc_level BETWEEN 0 AND 2),
    auto_approve_limit    numeric(36,18) NOT NULL CHECK (auto_approve_limit >= 0),
    daily_limit           numeric(36,18) NOT NULL CHECK (daily_limit >= 0),
    require_manual_review boolean        NOT NULL DEFAULT false,
    version               integer        NOT NULL DEFAULT 1,
    created_at            timestamptz    NOT NULL DEFAULT now(),
    updated_at            timestamptz    NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, asset_id, kyc_level)
);

-- Read everywhere; write only from admin (and the all-in-one dev role).
GRANT SELECT ON ALL TABLES IN SCHEMA registry
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;
GRANT INSERT, UPDATE ON ALL TABLES IN SCHEMA registry TO ex_admin, ex_all;

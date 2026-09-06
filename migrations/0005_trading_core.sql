-- +goose Up
-- Trading: orders, trades and the per-market command sequence
-- (docs/plan-v1.0.md §6.2, §8; ADR-0002). Postgres is the source of truth:
-- the engine rebuilds every order book from orders WHERE status IN ('open',
-- 'partially_filled') and continues from market_sequences.last_seq.
-- Only the engine role writes here.

CREATE TABLE trading.orders (
    id              text           PRIMARY KEY,                       -- ULID assigned by the engine
    tenant_id       text           NOT NULL DEFAULT 'default',
    account_id      uuid           NOT NULL REFERENCES ledger.accounts (id),
    market_id       uuid           NOT NULL REFERENCES registry.markets (id),
    market_symbol   text           NOT NULL,                          -- denormalised for events and listings
    client_order_id text           NOT NULL CHECK (length(client_order_id) BETWEEN 1 AND 64),
    side            text           NOT NULL CHECK (side IN ('buy', 'sell')),
    type            text           NOT NULL CHECK (type IN ('limit', 'market')),
    time_in_force   text           NOT NULL CHECK (time_in_force IN ('gtc', 'ioc')),
    price           numeric(36,18) CHECK (price IS NULL OR price > 0),   -- NULL for market orders
    qty             numeric(36,18) CHECK (qty IS NULL OR qty > 0),       -- base; NULL for market buys
    quote_qty       numeric(36,18) CHECK (quote_qty IS NULL OR quote_qty > 0), -- quote budget of a market buy
    filled_qty      numeric(36,18) NOT NULL DEFAULT 0 CHECK (filled_qty >= 0),
    filled_quote    numeric(36,18) NOT NULL DEFAULT 0 CHECK (filled_quote >= 0),
    remaining_qty   numeric(36,18) NOT NULL DEFAULT 0 CHECK (remaining_qty >= 0),
    -- what the order froze at acceptance and what of it is still frozen
    -- (Σ hold_remaining over open orders of an account/asset == balances.hold)
    hold_asset      text           NOT NULL,
    hold_amount     numeric(36,18) NOT NULL DEFAULT 0 CHECK (hold_amount >= 0),
    hold_remaining  numeric(36,18) NOT NULL DEFAULT 0 CHECK (hold_remaining >= 0),
    status          text           NOT NULL CHECK (status IN ('open', 'partially_filled', 'filled', 'cancelled', 'rejected')),
    reject_reason   text,
    cancel_reason   text,
    seq             bigint,                                           -- engine seq of the accepting/rejecting command
    correlation_id  text,
    created_at      timestamptz    NOT NULL DEFAULT now(),
    updated_at      timestamptz    NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, account_id, client_order_id),
    CHECK ((status = 'rejected') = (reject_reason IS NOT NULL)),
    CHECK (type = 'market' OR price IS NOT NULL),
    CHECK ((type = 'market' AND side = 'buy') = (qty IS NULL))
);
CREATE INDEX orders_open_by_market_idx ON trading.orders (market_id, seq) WHERE status IN ('open', 'partially_filled');
CREATE INDEX orders_account_idx ON trading.orders (tenant_id, account_id, created_at DESC, id);
CREATE INDEX orders_market_status_idx ON trading.orders (market_id, status, created_at DESC);

CREATE TABLE trading.trades (
    id               text           PRIMARY KEY,                      -- ULID
    tenant_id        text           NOT NULL DEFAULT 'default',
    market_id        uuid           NOT NULL REFERENCES registry.markets (id),
    market_symbol    text           NOT NULL,
    seq              bigint         NOT NULL,                         -- taker command seq
    idx              integer        NOT NULL CHECK (idx >= 0),        -- position within that command
    maker_order_id   text           NOT NULL REFERENCES trading.orders (id),
    taker_order_id   text           NOT NULL REFERENCES trading.orders (id),
    maker_account_id uuid           NOT NULL REFERENCES ledger.accounts (id),
    taker_account_id uuid           NOT NULL REFERENCES ledger.accounts (id),
    taker_side       text           NOT NULL CHECK (taker_side IN ('buy', 'sell')),
    price            numeric(36,18) NOT NULL CHECK (price > 0),
    qty              numeric(36,18) NOT NULL CHECK (qty > 0),
    quote_qty        numeric(36,18) NOT NULL CHECK (quote_qty > 0),
    maker_fee        numeric(36,18) NOT NULL CHECK (maker_fee >= 0),
    maker_fee_asset  text           NOT NULL,
    taker_fee        numeric(36,18) NOT NULL CHECK (taker_fee >= 0),
    taker_fee_asset  text           NOT NULL,
    created_at       timestamptz    NOT NULL DEFAULT now(),
    UNIQUE (market_id, seq, idx)
);
CREATE INDEX trades_market_idx ON trading.trades (market_id, created_at DESC, id);
CREATE INDEX trades_maker_idx ON trading.trades (maker_account_id, created_at DESC, id);
CREATE INDEX trades_taker_idx ON trading.trades (taker_account_id, created_at DESC, id);

-- One row per market; last_seq is the last command seq the engine committed.
-- The engine advances it with a guarded UPDATE (WHERE last_seq = expected)
-- so a second engine instance can never interleave commands unnoticed.
CREATE TABLE trading.market_sequences (
    market_id  uuid        PRIMARY KEY REFERENCES registry.markets (id),
    last_seq   bigint      NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Privileges (docs/plan-v1.0.md §14): everybody reads; only the engine writes.
-- Terminal orders and trades are never deleted; orders are updated only by
-- the engine while they are open.
GRANT SELECT ON trading.orders, trading.trades, trading.market_sequences
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;
GRANT INSERT, UPDATE ON trading.orders, trading.market_sequences TO ex_engine, ex_all;
GRANT INSERT ON trading.trades TO ex_engine, ex_all;

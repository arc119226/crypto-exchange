-- +goose Up
-- Candles (docs/plan-v1.0.md §12 Phase 6): the worker folds trading.trades
-- into one row per (market, interval, bucket) and remembers, per market, the
-- last engine seq it folded. The cursor moves in the same transaction as the
-- candles it accounts for, so a restart resumes exactly where the last
-- commit left off and no trade is counted twice or missed -- the
-- idempotency is the transaction, not a guard on every row.
--
-- Empty buckets are not stored. A reader that wants a continuous series
-- carries the previous close through the gap (marketdata.FillGaps), which is
-- what every charting library expects and costs nothing to keep.
--
-- No tickers table: the 24-hour ticker is a fold over the last day of 1m
-- candles and is computed on request.

CREATE TABLE marketdata.klines (
    tenant_id     text           NOT NULL DEFAULT 'default',
    market_symbol text           NOT NULL,
    interval      text           NOT NULL CHECK (interval IN ('1m', '5m', '15m', '1h', '1d')),
    bucket_start  timestamptz    NOT NULL,
    open          numeric(36,18) NOT NULL CHECK (open > 0),
    high          numeric(36,18) NOT NULL,
    low           numeric(36,18) NOT NULL CHECK (low > 0),
    close         numeric(36,18) NOT NULL CHECK (close > 0),
    volume        numeric(36,18) NOT NULL CHECK (volume > 0),
    quote_volume  numeric(36,18) NOT NULL CHECK (quote_volume > 0),
    trades        integer        NOT NULL CHECK (trades > 0),
    updated_at    timestamptz    NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, market_symbol, interval, bucket_start),
    CHECK (high >= low AND high >= open AND high >= close AND low <= open AND low <= close)
);

CREATE TABLE marketdata.kline_cursors (
    tenant_id     text        NOT NULL DEFAULT 'default',
    market_symbol text        NOT NULL,
    -- the last trading.trades.seq folded into klines; 0 = nothing yet
    last_seq      bigint      NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, market_symbol)
);

-- Privileges (docs/plan-v1.0.md §14): the worker writes, everybody reads
-- (api serves klines and the ticker, stream seeds its candle ring from them).
GRANT SELECT ON marketdata.klines, marketdata.kline_cursors
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;
GRANT INSERT, UPDATE ON marketdata.klines, marketdata.kline_cursors TO ex_worker, ex_all;

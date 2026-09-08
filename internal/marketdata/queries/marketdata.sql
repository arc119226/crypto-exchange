-- Market data queries: the shadow book's snapshot (read from the engine's
-- tables), the trade tape the candle writer folds, and the candles it writes.

-- name: GetMarketLastSeq :one
SELECT last_seq FROM trading.market_sequences WHERE market_id = $1;

-- name: ListOpenOrdersForBook :many
-- The resting orders of one market, oldest first: what the engine itself
-- restores a book from (internal/trading/runner.go restore).
SELECT id, side, price, remaining_qty, seq
  FROM trading.orders
 WHERE market_id = $1 AND status IN ('open', 'partially_filled')
 ORDER BY seq;

-- name: ListTradesBetweenSeq :many
-- Trades of the commands after after_seq up to and including upto_seq, in
-- execution order.
SELECT id, market_symbol, seq, idx, price, qty, quote_qty, taker_side, created_at
  FROM trading.trades
 WHERE market_id = $1 AND seq > sqlc.arg(after_seq) AND seq <= sqlc.arg(upto_seq)
 ORDER BY seq, idx;

-- name: GetKlineCursor :one
SELECT last_seq FROM marketdata.kline_cursors WHERE tenant_id = $1 AND market_symbol = $2;

-- name: UpsertKlineCursor :exec
INSERT INTO marketdata.kline_cursors (tenant_id, market_symbol, last_seq)
VALUES ($1, $2, $3)
ON CONFLICT (tenant_id, market_symbol) DO UPDATE
   SET last_seq = EXCLUDED.last_seq, updated_at = now();

-- name: UpsertKline :exec
-- Folds a batch's candle into the stored one. The batch comes after every
-- trade already stored (the cursor guarantees it), so the stored open stays
-- and the batch's close wins.
INSERT INTO marketdata.klines (
    tenant_id, market_symbol, interval, bucket_start,
    open, high, low, close, volume, quote_volume, trades
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (tenant_id, market_symbol, interval, bucket_start) DO UPDATE
   SET high         = GREATEST(marketdata.klines.high, EXCLUDED.high),
       low          = LEAST(marketdata.klines.low, EXCLUDED.low),
       close        = EXCLUDED.close,
       volume       = marketdata.klines.volume + EXCLUDED.volume,
       quote_volume = marketdata.klines.quote_volume + EXCLUDED.quote_volume,
       trades       = marketdata.klines.trades + EXCLUDED.trades,
       updated_at   = now();

-- name: ListKlines :many
SELECT market_symbol, interval, bucket_start, open, high, low, close, volume, quote_volume, trades
  FROM marketdata.klines
 WHERE tenant_id = $1 AND market_symbol = $2 AND interval = $3
   AND bucket_start >= sqlc.arg(from_start)::timestamptz AND bucket_start < sqlc.arg(to_start)::timestamptz
 ORDER BY bucket_start
 LIMIT sqlc.arg(row_limit);

-- name: LastKlineBefore :one
SELECT close
  FROM marketdata.klines
 WHERE tenant_id = $1 AND market_symbol = $2 AND interval = $3 AND bucket_start < $4
 ORDER BY bucket_start DESC
 LIMIT 1;

-- name: GetAccountSeq :one
-- The private stream's auth acknowledgement: where the account's event
-- sequence stands right now.
SELECT next_seq FROM ledger.accounts WHERE id = $1;

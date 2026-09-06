-- Trading queries (engine role writes, every role reads). Schema-qualified.

-- Orders -----------------------------------------------------------------

-- name: GetOrder :one
SELECT * FROM trading.orders WHERE id = $1;

-- name: GetOrderByClientID :one
SELECT * FROM trading.orders
WHERE tenant_id = $1 AND account_id = $2 AND client_order_id = $3;

-- name: InsertOrder :one
INSERT INTO trading.orders (
    id, tenant_id, account_id, market_id, market_symbol, client_order_id,
    side, type, time_in_force, price, qty, quote_qty,
    filled_qty, filled_quote, remaining_qty,
    hold_asset, hold_amount, hold_remaining,
    status, reject_reason, cancel_reason, seq, correlation_id, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11, $12,
    $13, $14, $15,
    $16, $17, $18,
    $19, $20, $21, $22, $23, $24
)
RETURNING *;

-- name: UpdateOrderProgress :one
-- A maker after a fill (or the taker's own remainder): fills, remaining,
-- what is still frozen and the resulting status.
UPDATE trading.orders
   SET filled_qty     = $2,
       filled_quote   = $3,
       remaining_qty  = $4,
       hold_remaining = $5,
       status         = $6,
       cancel_reason  = $7,
       updated_at     = now()
 WHERE id = $1
RETURNING *;

-- name: ListOpenOrdersByMarket :many
-- The engine rebuilds the book from these rows on start (ADR-0002).
SELECT * FROM trading.orders
WHERE market_id = $1 AND status IN ('open', 'partially_filled')
ORDER BY seq;

-- name: ListOrdersByAccount :many
SELECT * FROM trading.orders
WHERE tenant_id = $1 AND account_id = $2
  AND (sqlc.arg(market_symbol)::text = '' OR market_symbol = sqlc.arg(market_symbol)::text)
  AND (sqlc.arg(status)::text = '' OR status = sqlc.arg(status)::text)
  AND (sqlc.arg(open_only)::boolean = false OR status IN ('open', 'partially_filled'))
ORDER BY created_at DESC, id DESC
LIMIT $3 OFFSET $4;

-- name: CountOpenOrdersByMarket :one
SELECT count(*) FROM trading.orders WHERE market_id = $1 AND status IN ('open', 'partially_filled');

-- Trades ------------------------------------------------------------------

-- name: InsertTrade :exec
INSERT INTO trading.trades (
    id, tenant_id, market_id, market_symbol, seq, idx,
    maker_order_id, taker_order_id, maker_account_id, taker_account_id, taker_side,
    price, qty, quote_qty, maker_fee, maker_fee_asset, taker_fee, taker_fee_asset, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11,
    $12, $13, $14, $15, $16, $17, $18, $19
);

-- name: ListTradesByMarket :many
SELECT * FROM trading.trades
WHERE tenant_id = $1 AND market_symbol = $2
ORDER BY created_at DESC, id DESC
LIMIT $3 OFFSET $4;

-- name: ListFillsByAccount :many
SELECT * FROM trading.trades
WHERE tenant_id = $1
  AND (maker_account_id = $2 OR taker_account_id = $2)
  AND (sqlc.arg(market_symbol)::text = '' OR market_symbol = sqlc.arg(market_symbol)::text)
  AND (sqlc.arg(order_id)::text = '' OR maker_order_id = sqlc.arg(order_id)::text OR taker_order_id = sqlc.arg(order_id)::text)
ORDER BY created_at DESC, id DESC
LIMIT $3 OFFSET $4;

-- name: ListTradesByOrder :many
SELECT * FROM trading.trades
WHERE maker_order_id = $1 OR taker_order_id = $1
ORDER BY seq, idx;

-- Sequences ---------------------------------------------------------------

-- name: EnsureMarketSequence :one
INSERT INTO trading.market_sequences (market_id)
VALUES ($1)
ON CONFLICT (market_id) DO UPDATE SET updated_at = trading.market_sequences.updated_at
RETURNING last_seq;

-- name: AdvanceMarketSequence :execrows
-- Guarded: succeeds only when nobody else advanced the sequence since the
-- engine last read it (a second engine instance would fail here).
UPDATE trading.market_sequences
   SET last_seq = $2, updated_at = now()
 WHERE market_id = $1 AND last_seq = $2 - 1;

-- name: GetMarketSequence :one
SELECT last_seq FROM trading.market_sequences WHERE market_id = $1;

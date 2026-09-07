-- name: InsertReconciliationReport :one
INSERT INTO admin.reconciliation_reports
    (tenant_id, chain_id, started_at, finished_at, balanced, lines)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: InsertReconciliationBreak :one
INSERT INTO admin.reconciliation_breaks
    (tenant_id, report_id, chain_id, asset, block_height,
     ledger_total, chain_total, uncredited, above_frontier, in_flight, diff)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetLatestReconciliationReport :one
SELECT * FROM admin.reconciliation_reports
WHERE tenant_id = $1 AND chain_id = $2
ORDER BY finished_at DESC
LIMIT 1;

-- name: ListReconciliationBreaks :many
SELECT * FROM admin.reconciliation_breaks
WHERE tenant_id = $1 AND report_id = $2
ORDER BY asset;

-- name: SumUncreditedDeposits :one
-- Money the chain shows at or below the frontier that the ledger has not
-- credited yet: the scanner has seen it but not enough blocks have followed.
--
-- The status list is an allow-list rather than "everything except credited".
-- 'orphaned' and 'dropped' are not on chain any more, and 'reversed' will one
-- day mean the ledger has an entry going the other way -- a deny-list would
-- silently mis-sum the day that lands.
SELECT COALESCE(SUM(amount), 0)::numeric(36,18) AS total
FROM chain.deposits
WHERE tenant_id = $1 AND chain_id = $2 AND asset = $3
  AND status IN ('detected', 'confirming')
  AND block_number <= $4;

-- name: SumCreditedDepositsAbove :one
-- Deposits the ledger credited from blocks above the frontier. Normally zero,
-- because a deposit is credited only once enough blocks have followed it. It
-- stops being zero after a reorg rewinds the scan cursor below a height the
-- ledger has already booked, which is exactly when a report must not lie.
SELECT COALESCE(SUM(amount), 0)::numeric(36,18) AS total
FROM chain.deposits
WHERE tenant_id = $1 AND chain_id = $2 AND asset = $3
  AND status = 'credited' AND block_number > $4;

-- name: SumConfirmedWithdrawalsAbove :one
-- Withdrawal amounts the ledger took out of custody_hot for blocks above the
-- frontier. Only 'confirmed' moves the asset: a withdrawal that reverted on
-- chain leaves the money in pending_withdrawal, and a replaced one refunds the
-- user -- neither touches custody.
--
-- Reachable in normal operation, unlike the deposit case above: the withdrawal
-- worker confirms against the node's head while the frontier is held back to
-- the scanner's cursor, and the two run on separate clocks.
SELECT COALESCE(SUM(amount), 0)::numeric(36,18) AS total
FROM chain.withdrawals
WHERE tenant_id = $1 AND chain_id = $2 AND asset = $3
  AND status = 'confirmed' AND block_number > $4;

-- name: SumGasBookedAbove :one
-- Every gas entry the ledger booked for a transaction mined above the
-- frontier, in the chain's native coin.
--
-- The predicate is "the cost is recorded and the block is above", not a status
-- list, because gas is booked on success and on failure alike (§6.1.4 e, f):
-- a reverted withdrawal still burned it, so does a failed sweep, so does the
-- transaction that displaces a stuck one. Every one of those writes the cost
-- and the block together, which is what makes this poolable into one sum.
SELECT COALESCE(SUM(g.gas), 0)::numeric(36,18) AS total FROM (
    SELECT w.gas_cost AS gas FROM chain.withdrawals w
     WHERE w.tenant_id = sqlc.arg(tenant_id)::text AND w.chain_id = sqlc.arg(chain_id)::bigint
       AND w.gas_cost IS NOT NULL AND w.block_number > sqlc.arg(above_block)::bigint
    UNION ALL
    SELECT s.gas_cost FROM chain.sweeps s
     WHERE s.tenant_id = sqlc.arg(tenant_id)::text AND s.chain_id = sqlc.arg(chain_id)::bigint
       AND s.gas_cost IS NOT NULL AND s.block_number > sqlc.arg(above_block)::bigint
    UNION ALL
    SELECT f.gas_funding_cost FROM chain.sweeps f
     WHERE f.tenant_id = sqlc.arg(tenant_id)::text AND f.chain_id = sqlc.arg(chain_id)::bigint
       AND f.gas_funding_cost IS NOT NULL AND f.gas_funding_block > sqlc.arg(above_block)::bigint
    UNION ALL
    SELECT n.gas_cost FROM chain.nonce_fills n
     WHERE n.tenant_id = sqlc.arg(tenant_id)::text AND n.chain_id = sqlc.arg(chain_id)::bigint
       AND n.gas_cost IS NOT NULL AND n.block_number > sqlc.arg(above_block)::bigint
) g;

-- name: ListOpenChainWork :many
-- Every transaction this system has sent that the ledger has not booked yet.
--
-- Reconciliation asks the node for each one's receipt -- the same call the
-- worker that owns it will make -- because "has it been mined, and what did it
-- cost" cannot be answered from the row: the cost is only written once the
-- worker books it, and there is no gas limit stored to bound it with. A guess
-- would be a tolerance, and this comparison has none.
--
-- Only a withdrawal moves value out of the addresses being counted; a sweep,
-- a gas funding and a nonce fill all move it between two addresses that are
-- both inside the total, so for those only the gas matters. The amount is
-- carried anyway so one loop can handle all four.
SELECT 'withdrawal'::text AS kind, w.id AS ref_id, w.asset, w.amount, w.tx_hash
  FROM chain.withdrawals w
 WHERE w.tenant_id = sqlc.arg(tenant_id)::text AND w.chain_id = sqlc.arg(chain_id)::bigint
   AND w.status IN ('signed', 'broadcast') AND w.tx_hash IS NOT NULL
UNION ALL
SELECT 'sweep', s.id, s.asset, s.amount, s.tx_hash
  FROM chain.sweeps s
 WHERE s.tenant_id = sqlc.arg(tenant_id)::text AND s.chain_id = sqlc.arg(chain_id)::bigint
   AND s.status = 'broadcast' AND s.tx_hash IS NOT NULL
UNION ALL
SELECT 'gas_funding', f.id, f.asset, f.amount, f.gas_funding_tx_hash
  FROM chain.sweeps f
 WHERE f.tenant_id = sqlc.arg(tenant_id)::text AND f.chain_id = sqlc.arg(chain_id)::bigint
   AND f.status IN ('requested', 'gas_funded')
   AND f.gas_funding_tx_hash IS NOT NULL AND f.gas_funding_cost IS NULL
UNION ALL
SELECT 'nonce_fill', n.id::text, ''::text, 0::numeric(36,18), n.tx_hash
  FROM chain.nonce_fills n
 WHERE n.tenant_id = sqlc.arg(tenant_id)::text AND n.chain_id = sqlc.arg(chain_id)::bigint
   AND n.status = 'broadcast';

-- name: SetHotWalletLowAlerted :exec
-- Edge-triggering for alert.hot_wallet_low: the timestamp when the balance is
-- below the line, NULL when it comes back above it.
UPDATE chain.hot_wallets
SET low_alerted_at = $3, updated_at = now()
WHERE tenant_id = $1 AND chain_id = $2;

-- name: ConfirmNonceFill :one
-- A gap fill is mined: record where and what it cost, so the gas can be
-- booked exactly once and attributed to a block.
UPDATE chain.nonce_fills
SET status = $4, block_number = $5, gas_cost = $6, updated_at = now()
WHERE tenant_id = $1 AND chain_id = $2 AND nonce = $3
RETURNING *;

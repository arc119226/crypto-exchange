-- name: GetHotWallet :one
SELECT * FROM chain.hot_wallets WHERE tenant_id = $1 AND chain_id = $2;

-- name: UpsertHotWallet :one
-- Records the hot wallet on first start, and on every start after that returns
-- what is already stored.
--
-- The address is deliberately NOT overwritten. It is the evidence NonceManager
-- compares against the key it derives, and a row that adopted whatever address
-- it was last asked about could never disagree with anything -- which is to
-- say the "this is a different wallet" refusal would be unreachable, and a
-- deployment given the wrong seed would carry on allocating nonces against a
-- stranger's counter.
INSERT INTO chain.hot_wallets (tenant_id, chain_id, address, next_nonce)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, chain_id) DO UPDATE
SET updated_at = now()
RETURNING *;

-- name: AllocateNonce :one
-- Hands out the next nonce and advances the counter in one statement, so two
-- workers cannot be given the same one. The row lock is the serialisation
-- point of §6.4.2's single-goroutine rule, held by the database rather than
-- by trust.
UPDATE chain.hot_wallets
SET next_nonce = next_nonce + 1, updated_at = now()
WHERE tenant_id = $1 AND chain_id = $2
RETURNING next_nonce - 1 AS nonce;

-- name: SetNextNonce :exec
-- Recycles the most recently allocated nonce (§6.4.2: only when it is the
-- last one out, otherwise the gap is filled instead).
UPDATE chain.hot_wallets
SET next_nonce = $3, updated_at = now()
WHERE tenant_id = $1 AND chain_id = $2;

-- name: InsertSignature :one
-- Appended by the signer role only. The UNIQUE (kind, ref_id, attempt) is what
-- makes signing unrepeatable: a retry or a replayed message collides here
-- rather than producing a second valid transaction.
INSERT INTO chain.signing_log (
    tenant_id, kind, ref_id, attempt, chain_id, from_address, to_address, nonce, tx_hash, raw_tx
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetSignature :one
SELECT * FROM chain.signing_log
WHERE tenant_id = $1 AND kind = $2 AND ref_id = $3 AND attempt = $4;

-- name: InsertNonceFill :one
INSERT INTO chain.nonce_fills (tenant_id, chain_id, nonce, tx_hash, reason)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: UpdateNonceFillStatus :exec
UPDATE chain.nonce_fills
SET status = $4, updated_at = now()
WHERE tenant_id = $1 AND chain_id = $2 AND nonce = $3;

-- name: ListUnconfirmedNonceFills :many
SELECT * FROM chain.nonce_fills
WHERE tenant_id = $1 AND chain_id = $2 AND status = 'broadcast'
ORDER BY nonce;

-- name: SignWithdrawal :one
-- The one statement of §6.4.2's "nonce, raw signed tx and state land in the
-- same transaction". Splitting it would leave a signed transaction nobody
-- knows about, or a nonce nobody can spend.
UPDATE chain.withdrawals
SET status = 'signed', nonce = $3, raw_tx = $4, tx_hash = $5,
    version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: MarkWithdrawalBroadcast :one
UPDATE chain.withdrawals
SET status = 'broadcast', broadcast_at = now(), version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: ReplaceWithdrawalTx :one
-- A re-send with a higher fee: same nonce, new signature, new hash. The
-- replacement counter is also the attempt number in chain.signing_log.
UPDATE chain.withdrawals
SET raw_tx = $3, tx_hash = $4, broadcast_at = now(), replacements = replacements + 1,
    version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: MarkWithdrawalConfirmed :one
UPDATE chain.withdrawals
SET status = 'confirmed', block_number = $3, gas_cost = $4,
    version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: RecordWithdrawalGas :one
-- A failed transaction still burned gas, so the cost is recorded even though
-- the transfer did not happen (§6.1.4 e).
UPDATE chain.withdrawals
SET block_number = $3, gas_cost = $4, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: ListAllocatedNonces :many
-- NonceManager's startup scan: every nonce this tenant has handed out on this
-- chain, with the state of the withdrawal holding it.
SELECT id, nonce, status, tx_hash, raw_tx FROM chain.withdrawals
WHERE tenant_id = $1 AND chain_id = $2 AND nonce IS NOT NULL
ORDER BY nonce;

-- name: AllocateWithdrawalNonce :one
-- Pins the nonce onto the withdrawal before anything is signed, so a crash
-- between allocating and recording the signature does not strand the nonce and
-- does not make the retry ask for a different transaction. The status stays
-- funds_locked: nothing has been signed yet.
UPDATE chain.withdrawals
SET nonce = $3, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: MarkWithdrawalCancelling :one
-- Records the same-nonce transaction sent to displace a stuck withdrawal. The
-- withdrawal stays broadcast: it is not resolved until the cancellation is
-- mined.
UPDATE chain.withdrawals
SET cancel_tx_hash = $3, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: RetryWithdrawal :one
-- Sends a failed withdrawal back for another attempt: the transaction columns
-- are cleared so a fresh nonce is allocated, and the replacement counter is
-- advanced so the new signature has a signing-log key of its own.
UPDATE chain.withdrawals
SET status = 'funds_locked', failure_reason = NULL,
    nonce = NULL, raw_tx = NULL, tx_hash = NULL, broadcast_at = NULL,
    block_number = NULL, cancel_tx_hash = NULL,
    replacements = replacements + 1, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: ListCancellingWithdrawals :many
SELECT * FROM chain.withdrawals
WHERE tenant_id = $1 AND status = 'broadcast' AND cancel_tx_hash IS NOT NULL
ORDER BY updated_at
LIMIT $2;

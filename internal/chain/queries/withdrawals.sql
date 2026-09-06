-- name: GetWithdrawalByIdempotencyKey :one
-- The replay path of docs/plan-v1.0.md §6.4.2. The key is scoped to the
-- account so one client's key cannot reach another's withdrawal.
SELECT * FROM chain.withdrawals
WHERE tenant_id = $1 AND account_id = $2 AND idempotency_key = $3;

-- name: InsertWithdrawal :one
INSERT INTO chain.withdrawals (
    tenant_id, account_id, asset, amount, to_address, chain_id,
    idempotency_key, request_hash, status, correlation_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'requested', $9)
RETURNING *;

-- name: GetWithdrawal :one
SELECT * FROM chain.withdrawals WHERE tenant_id = $1 AND id = $2;

-- name: GetWithdrawalForUpdate :one
-- Every transition reads the row under a lock first: two workers must not
-- both decide a withdrawal, and an admin approval must not race the worker
-- that is already locking the funds.
SELECT * FROM chain.withdrawals
WHERE tenant_id = $1 AND id = $2
FOR UPDATE;

-- name: ClaimWithdrawals :many
-- The worker's queue. SKIP LOCKED so a second worker takes different rows
-- rather than waiting behind the first (the pattern chain.deposit_addresses
-- already uses for the address pool).
SELECT * FROM chain.withdrawals
WHERE tenant_id = $1 AND status = ANY(@statuses::text[])
ORDER BY created_at
LIMIT $2
FOR UPDATE SKIP LOCKED;

-- name: UpdateWithdrawalStatus :one
-- The one transition statement: it carries the failure reason because a
-- failure must never be recorded without one (0010 CHECK).
UPDATE chain.withdrawals
SET status = $3, failure_reason = $4, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: LockWithdrawalFunds :one
-- funds_locked also records which ledger entry did the locking, so the hold
-- can be traced without reconstructing its idempotency key.
UPDATE chain.withdrawals
SET status = 'funds_locked', hold_entry_id = $3, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: ReviewWithdrawal :one
-- The admin decision. reviewed_by and reviewed_at are written together
-- because 0010 requires them to be both set or both null.
UPDATE chain.withdrawals
SET status = $3, failure_reason = $4, reviewed_by = $5, reviewed_at = now(),
    review_note = $6, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: ListWithdrawalsByAccount :many
SELECT * FROM chain.withdrawals
WHERE tenant_id = $1 AND account_id = $2
ORDER BY created_at DESC
LIMIT $3;

-- name: ListWithdrawalsByStatus :many
-- The admin review queue, oldest first: the person waiting longest is served
-- first, unlike the user-facing listing.
SELECT * FROM chain.withdrawals
WHERE tenant_id = $1 AND status = ANY(@statuses::text[])
ORDER BY created_at
LIMIT $2;

-- name: SumWithdrawnSince :one
-- The daily limit of §6.4.2. Everything that is not rejected or failed counts
-- against it: a withdrawal still in review is money the account has already
-- committed, and letting a second one through while the first waits would be
-- a way to spend the limit twice.
SELECT COALESCE(SUM(amount), 0)::numeric(36,18) AS total
FROM chain.withdrawals
WHERE tenant_id = $1 AND account_id = $2 AND asset = $3
  AND created_at >= $4
  AND status NOT IN ('rejected', 'failed');

-- name: CountWithdrawalsByStatus :one
-- Feeds the review-queue gauge: a withdrawal waiting for a person is a user
-- waiting for their money, and nothing else in the system notices.
SELECT count(*) FROM chain.withdrawals
WHERE tenant_id = $1 AND status = ANY(@statuses::text[]);

-- name: GetAccountKYCLevel :one
-- The withdrawal policy is per KYC level, and the level lives on the user
-- behind the account. Reading it costs the chain role a column-scoped SELECT
-- on auth.users (migration 0010) rather than a dependency on the auth service.
SELECT u.kyc_level FROM ledger.accounts a
JOIN auth.users u ON u.id = a.owner_user_id
WHERE a.id = $1;

-- name: RequestWithdrawalResolve :one
-- The admin side of §6.4.2 resolve: record what an operator asked for. It
-- writes no transaction column and moves no money -- the chain role applies
-- the request, because it is the only role with a node, a signer and the
-- grants. resolve_error is cleared so a retried request does not show the
-- previous attempt's failure.
UPDATE chain.withdrawals
SET resolve_action = $3, resolve_note = $4, resolve_requested_by = $5,
    resolve_requested_at = now(), resolve_error = NULL,
    version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: ClaimResolveRequests :many
-- The chain role's resolve queue, oldest first. SKIP LOCKED for the same
-- reason ClaimWithdrawals uses it.
SELECT * FROM chain.withdrawals
WHERE tenant_id = $1 AND resolve_action IS NOT NULL
ORDER BY resolve_requested_at
LIMIT $2
FOR UPDATE SKIP LOCKED;

-- name: ClearWithdrawalResolve :exec
-- Applied, or found to be inapplicable. The note and the requester stay: they
-- are the record of what was asked, and $3 says what came of it.
UPDATE chain.withdrawals
SET resolve_action = NULL, resolve_error = $3, updated_at = now()
WHERE tenant_id = $1 AND id = $2;

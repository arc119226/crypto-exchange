-- Accounts --------------------------------------------------------------

-- name: GetAccount :one
SELECT * FROM ledger.accounts WHERE id = $1;

-- name: GetAccounts :many
SELECT * FROM ledger.accounts WHERE id = ANY($1::uuid[]) ORDER BY id;

-- name: ListHouseAccounts :many
SELECT * FROM ledger.accounts WHERE tenant_id = $1 AND kind = 'house' ORDER BY house_code;

-- name: EnsureHouseAccount :one
INSERT INTO ledger.accounts (tenant_id, kind, house_code)
VALUES ($1, 'house', $2)
ON CONFLICT (tenant_id, house_code) WHERE house_code IS NOT NULL
DO UPDATE SET updated_at = ledger.accounts.updated_at
RETURNING *;

-- name: CreateSpotAccount :one
INSERT INTO ledger.accounts (tenant_id, kind, owner_user_id)
VALUES ($1, 'spot', $2)
RETURNING *;

-- name: ListAccounts :many
SELECT * FROM ledger.accounts
WHERE tenant_id = $1 AND (sqlc.arg(kind)::text = '' OR kind = sqlc.arg(kind)::text)
ORDER BY created_at, id
LIMIT $2 OFFSET $3;

-- name: UpdateAccountStatus :one
UPDATE ledger.accounts
   SET status = $2, version = version + 1, updated_at = now()
 WHERE id = $1
RETURNING *;

-- Journal ---------------------------------------------------------------

-- name: InsertJournalEntry :one
INSERT INTO ledger.journal_entries (tenant_id, idempotency_key, kind, ref_type, ref_id, reason, correlation_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, idempotency_key) DO NOTHING
RETURNING *;

-- name: GetJournalEntryByKey :one
SELECT * FROM ledger.journal_entries WHERE tenant_id = $1 AND idempotency_key = $2;

-- name: GetJournalEntry :one
SELECT * FROM ledger.journal_entries WHERE id = $1;

-- name: InsertPosting :exec
INSERT INTO ledger.postings (entry_id, account_id, asset, bucket, direction, amount)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListPostingsByEntry :many
SELECT * FROM ledger.postings WHERE entry_id = $1 ORDER BY id;

-- name: ListEntries :many
SELECT * FROM ledger.journal_entries
WHERE tenant_id = $1
  AND (sqlc.arg(ref_type)::text = '' OR ref_type = sqlc.arg(ref_type)::text)
  AND (sqlc.arg(ref_id)::text = '' OR ref_id = sqlc.arg(ref_id)::text)
ORDER BY id DESC
LIMIT $2 OFFSET $3;

-- name: ListEntriesByAccount :many
SELECT e.* FROM ledger.journal_entries e
WHERE e.tenant_id = $1
  AND EXISTS (SELECT 1 FROM ledger.postings p WHERE p.entry_id = e.id AND p.account_id = $2)
ORDER BY e.id DESC
LIMIT $3 OFFSET $4;

-- Balances --------------------------------------------------------------

-- name: LockBalance :one
-- Creates the row if missing and takes the row lock (the no-op UPDATE locks);
-- callers lock rows in (account_id, asset) order to avoid deadlocks.
INSERT INTO ledger.balances (account_id, asset)
VALUES ($1, $2)
ON CONFLICT (account_id, asset) DO UPDATE SET version = ledger.balances.version
RETURNING account_id, asset, available, hold, version;

-- name: ApplyBalanceDelta :one
UPDATE ledger.balances
   SET available = available + $3,
       hold      = hold + $4,
       version   = version + 1,
       updated_at = now()
 WHERE account_id = $1 AND asset = $2
RETURNING account_id, asset, available, hold, version;

-- name: GetBalances :many
SELECT * FROM ledger.balances WHERE account_id = $1 ORDER BY asset;

-- name: GetBalance :one
SELECT * FROM ledger.balances WHERE account_id = $1 AND asset = $2;

-- Derived views -----------------------------------------------------------

-- name: TrialBalance :many
SELECT p.asset,
       SUM(CASE p.direction WHEN 'debit'  THEN p.amount ELSE 0 END)::numeric(36,18) AS debits,
       SUM(CASE p.direction WHEN 'credit' THEN p.amount ELSE 0 END)::numeric(36,18) AS credits
  FROM ledger.postings p
  JOIN ledger.journal_entries e ON e.id = p.entry_id
 WHERE e.tenant_id = $1
 GROUP BY p.asset
 ORDER BY p.asset;

-- name: AccountBucketSums :many
-- credit − debit per (asset, bucket): for spot accounts this must equal the
-- balances cache (docs/plan-v1.0.md §6.1.5 invariant 2).
SELECT p.asset, p.bucket,
       (SUM(CASE p.direction WHEN 'credit' THEN p.amount ELSE 0 END)
      - SUM(CASE p.direction WHEN 'debit'  THEN p.amount ELSE 0 END))::numeric(36,18) AS credit_minus_debit
  FROM ledger.postings p
 WHERE p.account_id = $1
 GROUP BY p.asset, p.bucket
 ORDER BY p.asset, p.bucket;

-- name: HouseBalances :many
-- debit − credit per (house_code, asset); sign interpretation per account type is in Go.
SELECT a.house_code, p.asset,
       (SUM(CASE p.direction WHEN 'debit'  THEN p.amount ELSE 0 END)
      - SUM(CASE p.direction WHEN 'credit' THEN p.amount ELSE 0 END))::numeric(36,18) AS debit_minus_credit
  FROM ledger.postings p
  JOIN ledger.accounts a ON a.id = p.account_id
 WHERE a.tenant_id = $1 AND a.kind = 'house'
 GROUP BY a.house_code, p.asset
 ORDER BY a.house_code, p.asset;

-- name: BumpAccountSeq :one
-- Per-account event sequence for the private stream (docs/plan-v1.0.md §7.1);
-- called inside the transaction that writes the outbox rows.
UPDATE ledger.accounts SET next_seq = next_seq + 1 WHERE id = $1 RETURNING next_seq;

-- name: GetSpotAccountByOwner :one
-- Registration opens exactly one spot account per user (docs/plan-v1.0.md §6.7).
SELECT * FROM ledger.accounts
WHERE tenant_id = $1 AND owner_user_id = $2 AND kind = 'spot'
ORDER BY created_at, id
LIMIT 1;

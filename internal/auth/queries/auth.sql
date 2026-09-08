-- Auth queries (api role writes users, refresh tokens and API keys; admin
-- edits users). Schema-qualified.

-- Users ------------------------------------------------------------------

-- name: CreateUser :one
INSERT INTO auth.users (tenant_id, email, password_hash, role)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetUser :one
SELECT * FROM auth.users WHERE id = $1;

-- name: GetUserByEmail :one
SELECT * FROM auth.users WHERE tenant_id = $1 AND email = $2;

-- name: UpdateUserPassword :exec
UPDATE auth.users SET password_hash = $2, version = version + 1, updated_at = now() WHERE id = $1;

-- name: UpdateUserStatus :one
UPDATE auth.users SET status = $2, version = version + 1, updated_at = now() WHERE id = $1 RETURNING *;

-- name: UpdateUserKYCLevel :one
UPDATE auth.users SET kyc_level = $2, version = version + 1, updated_at = now() WHERE id = $1 RETURNING *;

-- name: CountAdmins :one
SELECT count(*) FROM auth.users WHERE tenant_id = $1 AND role = 'admin';

-- Refresh tokens -----------------------------------------------------------

-- name: InsertRefreshToken :one
INSERT INTO auth.refresh_tokens (tenant_id, user_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetRefreshTokenByHash :one
SELECT * FROM auth.refresh_tokens WHERE token_hash = $1;

-- name: RevokeRefreshToken :execrows
-- Marks a token used/revoked; 0 rows means it was already gone (reuse attempt).
UPDATE auth.refresh_tokens
   SET revoked_at = now(), replaced_by = $2
 WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeUserRefreshTokens :execrows
UPDATE auth.refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;

-- API keys -------------------------------------------------------------------

-- name: InsertAPIKey :one
INSERT INTO auth.api_keys (tenant_id, user_id, key_id, secret_enc, label, scopes, ip_allowlist)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetAPIKeyByKeyID :one
SELECT * FROM auth.api_keys WHERE key_id = $1;

-- name: GetAPIKey :one
SELECT * FROM auth.api_keys WHERE id = $1;

-- name: ListAPIKeysByUser :many
SELECT * FROM auth.api_keys WHERE user_id = $1 ORDER BY created_at, id;

-- name: RevokeAPIKey :execrows
UPDATE auth.api_keys SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: TouchAPIKey :exec
UPDATE auth.api_keys SET last_used_at = now() WHERE id = $1;

-- name: ListUsers :many
-- The operator's directory view. position() rather than LIKE so an email
-- fragment cannot carry wildcards.
SELECT * FROM auth.users
WHERE tenant_id = $1
  AND (sqlc.arg(email)::text = '' OR position(sqlc.arg(email)::text IN email) > 0)
  AND (sqlc.arg(status)::text = '' OR status = sqlc.arg(status)::text)
  AND (sqlc.arg(role)::text = '' OR role = sqlc.arg(role)::text)
ORDER BY created_at DESC, id
LIMIT $2 OFFSET $3;

-- name: CountActiveAdmins :one
-- Freezing the last one would lock everybody out of the back office.
SELECT count(*) FROM auth.users WHERE tenant_id = $1 AND role = 'admin' AND status = 'active';

-- Admin sessions (0018) ----------------------------------------------------

-- name: InsertAdminSession :one
INSERT INTO auth.admin_sessions (id_hash, tenant_id, user_id, expires_at, totp_verified_at, ip)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAdminSessionByHash :one
SELECT * FROM auth.admin_sessions WHERE id_hash = $1;

-- name: TouchAdminSession :exec
UPDATE auth.admin_sessions SET last_seen_at = now() WHERE id_hash = $1;

-- name: RevokeAdminSession :execrows
UPDATE auth.admin_sessions SET revoked_at = now() WHERE id_hash = $1 AND revoked_at IS NULL;

-- name: RevokeUserAdminSessions :execrows
UPDATE auth.admin_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;

-- TOTP (0007 columns + 0018 bookkeeping) -------------------------------------
-- None of these bump version / updated_at: a failed code is not an edit of
-- the user, and a counter that rewrote the row's version would make every
-- optimistic check elsewhere see phantom changes.

-- name: SetPendingTOTPSecret :exec
UPDATE auth.users
   SET totp_secret_enc = $2, totp_enabled = false,
       totp_failures = 0, totp_locked_until = NULL, totp_last_step = NULL
 WHERE id = $1;

-- name: EnableTOTP :exec
UPDATE auth.users
   SET totp_enabled = true, totp_failures = 0, totp_locked_until = NULL, totp_last_step = $2
 WHERE id = $1;

-- name: RecordTOTPSuccess :exec
UPDATE auth.users SET totp_failures = 0, totp_locked_until = NULL, totp_last_step = $2 WHERE id = $1;

-- name: RecordTOTPFailure :one
-- Locks when this failure reaches the limit. The lock time is computed by the
-- caller so tests can drive the clock.
UPDATE auth.users
   SET totp_failures = totp_failures + 1,
       totp_locked_until = CASE WHEN totp_failures + 1 >= sqlc.arg(max_failures)::int
                                THEN sqlc.arg(lock_until)::timestamptz
                                ELSE totp_locked_until END
 WHERE id = $1
RETURNING totp_failures, totp_locked_until;

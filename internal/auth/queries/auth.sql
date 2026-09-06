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

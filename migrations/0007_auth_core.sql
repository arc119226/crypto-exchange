-- +goose Up
-- Minimal auth (docs/plan-v1.0.md §3.1, §14; ADR-0006): users directory,
-- refresh tokens and API keys. The engine never sees users: registration
-- opens a ledger spot account (ledger.accounts.owner_user_id) and every
-- trading call carries the account id. Passwords are argon2id PHC strings;
-- refresh tokens are stored as SHA-256 hashes; API key secrets are stored
-- AES-256-GCM encrypted under API_KEY_MASTER_KEY (the server must know the
-- secret to verify HMAC signatures).

CREATE TABLE auth.users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     text        NOT NULL DEFAULT 'default',
    email         text        NOT NULL CHECK (email = lower(email) AND email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),
    password_hash text        NOT NULL,
    role          text        NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    kyc_level     smallint    NOT NULL DEFAULT 0 CHECK (kyc_level BETWEEN 0 AND 2),
    status        text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen')),
    -- admin TOTP (Phase 5); kept here so the row shape is final
    totp_secret_enc bytea,
    totp_enabled  boolean     NOT NULL DEFAULT false,
    version       integer     NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, email)
);

CREATE TABLE auth.refresh_tokens (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  text        NOT NULL DEFAULT 'default',
    user_id    uuid        NOT NULL REFERENCES auth.users (id),
    token_hash bytea       NOT NULL UNIQUE,          -- sha256 of the opaque token
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    replaced_by uuid       REFERENCES auth.refresh_tokens (id)
);
CREATE INDEX refresh_tokens_user_idx ON auth.refresh_tokens (user_id, expires_at);

CREATE TABLE auth.api_keys (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    text        NOT NULL DEFAULT 'default',
    user_id      uuid        NOT NULL REFERENCES auth.users (id),
    key_id       text        NOT NULL UNIQUE,          -- public identifier sent in X-API-KEY
    secret_enc   bytea       NOT NULL,                 -- AES-256-GCM(nonce || ciphertext) of the HMAC secret
    label        text        NOT NULL DEFAULT '',
    scopes       text[]      NOT NULL CHECK (scopes <@ ARRAY['read', 'trade', 'withdraw']::text[] AND cardinality(scopes) > 0),
    ip_allowlist text[]      NOT NULL DEFAULT '{}',    -- empty = any
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);
CREATE INDEX api_keys_user_idx ON auth.api_keys (user_id, created_at);

-- Privileges (docs/plan-v1.0.md §14): the api role owns registration, login
-- and API keys; admin reads and edits users (kyc_level, status, Phase 5);
-- nobody deletes (revocation is a timestamp).
GRANT SELECT ON auth.users, auth.refresh_tokens, auth.api_keys TO ex_api, ex_admin, ex_all;
GRANT INSERT, UPDATE ON auth.users, auth.refresh_tokens, auth.api_keys TO ex_api, ex_all;
GRANT INSERT, UPDATE ON auth.users TO ex_admin;

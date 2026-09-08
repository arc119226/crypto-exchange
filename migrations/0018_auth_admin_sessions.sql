-- +goose Up
-- Administrator sessions and TOTP state (docs/plan-v1.0.md §12 Phase 5, §14,
-- ADR-0006).
--
-- A human administrator logs in with a password and a TOTP code and gets a
-- server-side session: the cookie carries an opaque random token, this table
-- holds its SHA-256, and nothing about the session is decidable from the
-- cookie alone. No JWT is involved, which is why the admin role never needs
-- the signing key (§14).

CREATE TABLE auth.admin_sessions (
    -- sha256 of the token in the cookie. A dump of this table yields nothing a
    -- browser could present, for the same reason refresh_tokens stores a hash.
    id_hash          bytea       PRIMARY KEY,
    tenant_id        text        NOT NULL DEFAULT 'default',
    user_id          uuid        NOT NULL REFERENCES auth.users (id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    -- Null until the code has been checked. A session with a password behind
    -- it and no code is not an administrator; it is a person who is allowed
    -- to try a code, and the middleware lets it reach exactly that page.
    totp_verified_at timestamptz,
    last_seen_at     timestamptz NOT NULL DEFAULT now(),
    -- Revocation is a timestamp, never a DELETE (0007, 0013). Logging out,
    -- passing the code (the id is rotated, the old row revoked), and freezing
    -- the user all land here.
    revoked_at       timestamptz,
    ip               text        NOT NULL DEFAULT '',
    CHECK (expires_at > created_at)
);

CREATE INDEX admin_sessions_user_idx ON auth.admin_sessions (user_id, created_at DESC);

-- TOTP bookkeeping on the user. totp_secret_enc and totp_enabled have been on
-- the row since 0007; these three are what verifying a code needs.
ALTER TABLE auth.users
    -- Consecutive wrong codes. Reset on success. Only codes count: a wrong
    -- password is throttled per source address instead, because a counter an
    -- outsider can drive with nothing but the administrator's email address
    -- would be a lock anyone could apply every fifteen minutes.
    ADD COLUMN totp_failures     integer     NOT NULL DEFAULT 0 CHECK (totp_failures >= 0),
    ADD COLUMN totp_locked_until timestamptz,
    -- The 30-second step of the last accepted code. A code is accepted only
    -- for a later step, so the one somebody shoulder-surfed thirty seconds ago
    -- has already been spent. Recorded as the step the code matched, not the
    -- step of the clock: with one step of skew allowed, those can differ.
    ADD COLUMN totp_last_step    bigint;

-- Privileges (docs/plan-v1.0.md §14). The admin role owns its own sessions
-- and the TOTP columns it verifies against; it already holds UPDATE on
-- auth.users (0007). The api role gets nothing here: a public-API request can
-- never be, or become, an administrator session.
GRANT SELECT, INSERT, UPDATE ON auth.admin_sessions TO ex_admin, ex_all;

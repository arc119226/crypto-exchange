-- +goose Up
-- Ledger: double-entry journal with a balances cache (docs/plan-v1.0.md §6.1,
-- ADR-0005). Amounts are NUMERIC(36,18). Postings are append-only: no role is
-- granted UPDATE or DELETE on postings or journal entries; the deferred
-- constraint trigger rejects any entry whose per-asset debits != credits.

CREATE TABLE ledger.accounts (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     text        NOT NULL DEFAULT 'default',
    kind          text        NOT NULL CHECK (kind IN ('spot', 'house')),
    -- house accounts are the exchange's own books; asset lives on the posting
    house_code    text        CHECK (house_code IN (
                                  'fee_revenue', 'gas_expense', 'custody_hot',
                                  'custody_deposit_addresses', 'pending_withdrawal', 'external')),
    -- auth.users arrives in Phase 3; until then spot accounts may be ownerless (dev/admin created)
    owner_user_id uuid,
    status        text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen')),
    -- per-account event sequence for the private stream (Phase 3, §7.1)
    next_seq      bigint      NOT NULL DEFAULT 0,
    version       integer     NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'house') = (house_code IS NOT NULL)),
    CHECK (kind = 'spot' OR owner_user_id IS NULL)
);
CREATE UNIQUE INDEX accounts_house_code_uniq ON ledger.accounts (tenant_id, house_code) WHERE house_code IS NOT NULL;
CREATE INDEX accounts_owner_idx ON ledger.accounts (tenant_id, owner_user_id) WHERE owner_user_id IS NOT NULL;

CREATE TABLE ledger.journal_entries (
    id              bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       text        NOT NULL DEFAULT 'default',
    idempotency_key text        NOT NULL,
    kind            text        NOT NULL,   -- hold | release | settle | credit | adjustment | deposit | withdrawal | sweep | gas | reversal
    ref_type        text,                   -- order | trade | withdrawal | deposit | adjustment | sweep
    ref_id          text,
    reason          text,                   -- required for adjustments (admin)
    correlation_id  text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, idempotency_key)
);
CREATE INDEX journal_entries_ref_idx ON ledger.journal_entries (tenant_id, ref_type, ref_id);

CREATE TABLE ledger.postings (
    id         bigint         GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    entry_id   bigint         NOT NULL REFERENCES ledger.journal_entries (id),
    account_id uuid           NOT NULL REFERENCES ledger.accounts (id),
    asset      text           NOT NULL,
    -- spot accounts post to available|hold (cached in ledger.balances); house accounts post to house
    bucket     text           NOT NULL CHECK (bucket IN ('available', 'hold', 'house')),
    direction  text           NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount     numeric(36,18) NOT NULL CHECK (amount > 0)
);
CREATE INDEX postings_entry_idx ON ledger.postings (entry_id);
CREATE INDEX postings_account_idx ON ledger.postings (account_id, asset, id);

-- Cache of the spot accounts' liability balances: available = Σcredit − Σdebit
-- of the available bucket, hold likewise. Maintained in the same transaction
-- as the postings; house accounts are derived from postings only.
CREATE TABLE ledger.balances (
    account_id uuid           NOT NULL REFERENCES ledger.accounts (id),
    asset      text           NOT NULL,
    available  numeric(36,18) NOT NULL DEFAULT 0 CHECK (available >= 0),
    hold       numeric(36,18) NOT NULL DEFAULT 0 CHECK (hold >= 0),
    version    bigint         NOT NULL DEFAULT 1,
    updated_at timestamptz    NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, asset)
);

-- +goose StatementBegin
CREATE FUNCTION ledger.check_entry_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    bad_asset text;
    bad_diff  numeric;
BEGIN
    SELECT asset, SUM(CASE direction WHEN 'debit' THEN amount ELSE -amount END)
      INTO bad_asset, bad_diff
      FROM ledger.postings
     WHERE entry_id = NEW.entry_id
     GROUP BY asset
    HAVING SUM(CASE direction WHEN 'debit' THEN amount ELSE -amount END) <> 0
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'ledger: journal entry % is unbalanced for asset % (debit - credit = %)',
            NEW.entry_id, bad_asset, bad_diff
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- Deferred to commit so an entry can be inserted posting by posting.
CREATE CONSTRAINT TRIGGER postings_balanced
    AFTER INSERT ON ledger.postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_balanced();

-- House accounts of the default tenant (one per code; the asset is on the posting).
INSERT INTO ledger.accounts (tenant_id, kind, house_code) VALUES
    ('default', 'house', 'fee_revenue'),
    ('default', 'house', 'gas_expense'),
    ('default', 'house', 'custody_hot'),
    ('default', 'house', 'custody_deposit_addresses'),
    ('default', 'house', 'pending_withdrawal'),
    ('default', 'house', 'external')
ON CONFLICT (tenant_id, house_code) WHERE house_code IS NOT NULL DO NOTHING;

-- Privileges (docs/plan-v1.0.md §14): GRANT only what each role needs.
GRANT SELECT ON ledger.accounts, ledger.journal_entries, ledger.postings, ledger.balances
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;
-- ledger writers (engine: trades/holds, chain: deposits/withdrawals, admin: adjustments)
GRANT INSERT ON ledger.journal_entries, ledger.postings TO ex_engine, ex_chain, ex_admin, ex_all;
GRANT INSERT, UPDATE ON ledger.balances TO ex_engine, ex_chain, ex_admin, ex_all;
-- registration creates the spot account (api); admin creates/freezes; engine bumps next_seq
GRANT INSERT ON ledger.accounts TO ex_api, ex_admin, ex_all;
GRANT UPDATE ON ledger.accounts TO ex_engine, ex_admin, ex_all;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA ledger TO ex_engine, ex_chain, ex_admin, ex_all;

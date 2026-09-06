-- +goose Up
-- Event bus: transactional outbox and the consumer idempotency table
-- (docs/plan-v1.0.md §7.1, §7.3; ADR-0002). Every module that emits an event
-- INSERTs into eventbus.outbox inside its own business transaction; the
-- engine's relay publishes rows in id order to JetStream and stamps
-- published_at. Consumers of the processing kind record event ids in
-- processed_events inside the transaction that applies the event.

CREATE TABLE eventbus.outbox (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id       text        NOT NULL UNIQUE,                    -- ULID; also the JetStream Nats-Msg-Id
    event_type     text        NOT NULL CHECK (event_type ~ '^[a-z_]+\.[a-z_]+$'),
    schema_version integer     NOT NULL DEFAULT 1 CHECK (schema_version >= 1),
    tenant_id      text        NOT NULL DEFAULT 'default',
    market_id      text,                                            -- market symbol for market-domain events
    account_id     uuid,                                            -- owner for account-domain events
    seq            bigint,                                          -- per-market engine seq
    account_seq    bigint,                                          -- per-account seq (ledger.accounts.next_seq)
    subject        text        NOT NULL,                            -- ex.v1.<domain>.<type>.<tenant>.<scope>
    headers        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    payload        jsonb       NOT NULL,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    correlation_id text,
    causation_id   text,
    published_at   timestamptz
);
-- The relay scans this index; it stays small because published rows leave it.
CREATE INDEX outbox_unpublished_idx ON eventbus.outbox (id) WHERE published_at IS NULL;
-- Private-stream resume (Phase 6): events of one account after a given account_seq.
CREATE INDEX outbox_account_seq_idx ON eventbus.outbox (tenant_id, account_id, account_seq) WHERE account_id IS NOT NULL;

-- Wake the relay as soon as a transaction with new events commits.
-- +goose StatementBegin
CREATE FUNCTION eventbus.notify_outbox() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('outbox_new', '');
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER outbox_notify
    AFTER INSERT ON eventbus.outbox
    FOR EACH STATEMENT
    EXECUTE FUNCTION eventbus.notify_outbox();

CREATE TABLE eventbus.processed_events (
    consumer     text        NOT NULL,
    event_id     text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

-- Privileges (docs/plan-v1.0.md §14). Producers INSERT; only the relay
-- (engine role) marks rows published; nobody deletes (retention is a
-- worker job in a later phase, granted then).
GRANT SELECT ON eventbus.outbox, eventbus.processed_events
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;
GRANT INSERT ON eventbus.outbox TO ex_api, ex_engine, ex_chain, ex_admin, ex_worker, ex_all;
GRANT UPDATE ON eventbus.outbox TO ex_engine, ex_all;
GRANT INSERT ON eventbus.processed_events TO ex_engine, ex_stream, ex_admin, ex_worker, ex_all;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA eventbus TO ex_api, ex_engine, ex_chain, ex_admin, ex_worker, ex_all;

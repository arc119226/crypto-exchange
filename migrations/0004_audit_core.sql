-- +goose Up
-- Append-only audit trail (docs/plan-v1.0.md §14): every admin write, auth
-- event, withdrawal state change, signing request and registry change lands
-- here. Writers may only INSERT; nobody may UPDATE or DELETE.

CREATE TABLE audit.audit_events (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id      text        NOT NULL DEFAULT 'default',
    actor_type     text        NOT NULL CHECK (actor_type IN ('user', 'admin', 'system', 'api_key')),
    actor_id       text,
    action         text        NOT NULL,
    target_type    text,
    target_id      text,
    before         jsonb,
    after          jsonb,
    ip             text,
    correlation_id text,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_target_idx ON audit.audit_events (tenant_id, target_type, target_id, id);
CREATE INDEX audit_events_actor_idx ON audit.audit_events (tenant_id, actor_type, actor_id, id);
CREATE INDEX audit_events_action_idx ON audit.audit_events (tenant_id, action, id);

GRANT INSERT ON audit.audit_events TO ex_api, ex_engine, ex_chain, ex_signer, ex_admin, ex_worker, ex_all;
GRANT SELECT ON audit.audit_events TO ex_admin, ex_all;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA audit TO ex_api, ex_engine, ex_chain, ex_signer, ex_admin, ex_worker, ex_all;

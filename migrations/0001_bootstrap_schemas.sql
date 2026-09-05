-- +goose Up
-- One Postgres, one schema per module (docs/plan-v1.0.md §8, §14). Tables are
-- owned by ex_migrate; every process role gets a dedicated login role and
-- only the privileges granted explicitly in later migrations.
--
-- Tenant convention: every business table carries
--   tenant_id text NOT NULL DEFAULT 'default'
-- and includes it in its unique keys. v1 runs a single tenant (ADR-0003).

CREATE SCHEMA IF NOT EXISTS auth;
CREATE SCHEMA IF NOT EXISTS registry;
CREATE SCHEMA IF NOT EXISTS ledger;
CREATE SCHEMA IF NOT EXISTS trading;
CREATE SCHEMA IF NOT EXISTS eventbus;
CREATE SCHEMA IF NOT EXISTS chain;
CREATE SCHEMA IF NOT EXISTS marketdata;
CREATE SCHEMA IF NOT EXISTS webhook;
CREATE SCHEMA IF NOT EXISTS audit;
CREATE SCHEMA IF NOT EXISTS admin;

-- Roles are expected to exist (infra/postgres/initdb/01-roles.sh or
-- scripts/db-roles.sql). `exchange migrate` turns error 42704 into a hint.
GRANT USAGE ON SCHEMA auth, registry, ledger, trading, eventbus, chain, marketdata, webhook, audit, admin
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;

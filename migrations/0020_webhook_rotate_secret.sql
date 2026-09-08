-- +goose Up
-- Webhook signing-secret rotation (docs/plan-v1.0.md §7.6; the piece 5b
-- deferred).
--
-- A customer cannot change the secret on every receiver at the same instant
-- the exchange starts signing with a new one, so a rotation keeps the old
-- secret for a grace period and every delivery in that window carries two
-- signatures: X-Exchange-Signature: v1=<new>,v1=<old>. A receiver that has
-- switched verifies the first; one that has not yet verifies the second;
-- neither drops a delivery. docs/webhooks.md has said "the header can carry
-- several" since 5b -- this is what it was for.

ALTER TABLE webhook.endpoints
    ADD COLUMN previous_secret_enc   bytea,
    ADD COLUMN previous_secret_until timestamptz,
    -- Both or neither: an old secret with no expiry would be a second
    -- permanent secret, and an expiry with no secret means nothing.
    ADD CONSTRAINT endpoints_previous_secret_pair
        CHECK ((previous_secret_enc IS NULL) = (previous_secret_until IS NULL));

-- No new grants. The admin role already holds UPDATE on webhook.endpoints
-- (0016) and the worker SELECT; a column follows its table.

-- +goose Up
-- The deep-reorg reversal of a credited deposit (docs/plan-v1.0.md §6.4.1,
-- the `reversed` row of the deposit state machine).
--
-- This state has been declared since 0009 -- it is in the CHECK, in the admin
-- filters, in the event catalogue, and api/events/v1/deposit.reversed.json has
-- had a schema and a golden envelope for as long -- but nothing in the code
-- could ever write it. The scanner's rewind deliberately skips credited
-- deposits, on the grounds that the money may already have been spent, and
-- there the story stopped. So detection was built and the response was not: a
-- deposit removed by a reorg deeper than its confirmations left an account
-- holding a balance the chain no longer backs, reconciliation reported it as a
-- break, and there was nothing in the product that could resolve it.
--
-- The shape follows withdrawal resolve (0011), for the same reason and by the
-- same privilege boundary: the admin role has a person's decision but no node
-- and no view of the chain, so it RECORDS what should happen and the chain
-- role does it on its next tick.

ALTER TABLE chain.deposits
    -- Stamped by the scanner when a credited deposit's block is abandoned.
    -- The status deliberately stays 'credited': the ledger still holds the
    -- credit, and it must go on doing so until the reversal actually posts.
    -- This column is the queue.
    ADD COLUMN reorged_at_block bigint CHECK (reorged_at_block IS NULL OR reorged_at_block >= 0),
    -- The operator's decision. No action column, because there is only one
    -- action: a reversal either happens or it does not.
    ADD COLUMN reversal_requested_by text,
    ADD COLUMN reversal_requested_at timestamptz,
    ADD COLUMN reversal_note text,
    -- Why the last attempt could not be applied -- almost always that the
    -- account has already spent the money. Kept after the request is cleared,
    -- so the person who asked can see what happened.
    ADD COLUMN reversal_error text,
    ADD COLUMN reversed_at timestamptz;

-- A request must say who asked and why. One-directional, like 0011's: the
-- worker clears reversal_requested_at when it has applied the request, and the
-- requester and the note stay as the record of what was asked.
ALTER TABLE chain.deposits
    ADD CONSTRAINT deposits_reversal_is_attributed CHECK (
        reversal_requested_at IS NULL
        OR (reversal_note IS NOT NULL AND reversal_requested_by IS NOT NULL));

-- Reversed means the reversing entry was posted, and says when.
ALTER TABLE chain.deposits
    ADD CONSTRAINT deposits_reversed_has_a_time CHECK (
        (status = 'reversed') = (reversed_at IS NOT NULL));

-- 0023 tied fee and credited_amount to credited_at. 0009 ties credited_at to
-- status = 'credited' exactly, so a reversal has to clear it -- and that would
-- have taken the two fee columns with it, erasing the numbers the reversal was
-- built from at the moment they became most worth keeping. They are tied to
-- "the ledger has touched this deposit" instead, which stays true afterwards.
--
-- credited_at itself does go: the deposits table is the scanner's memory of
-- the chain, and the authoritative record of when the credit happened is the
-- journal entry, which is permanent.
ALTER TABLE chain.deposits
    DROP CONSTRAINT deposits_credited_amount_with_credited_at,
    DROP CONSTRAINT deposits_fee_with_credited_at,
    ADD CONSTRAINT deposits_credited_amount_once_the_ledger_moved CHECK (
        (credited_amount IS NOT NULL) = (credited_at IS NOT NULL OR status = 'reversed')),
    ADD CONSTRAINT deposits_fee_once_the_ledger_moved CHECK (
        (fee IS NOT NULL) = (credited_at IS NOT NULL OR status = 'reversed'));

-- The operator's queue: credited deposits whose block was abandoned. Partial,
-- because in a healthy exchange it is empty.
CREATE INDEX deposits_awaiting_reversal_idx ON chain.deposits (tenant_id, chain_id, reorged_at_block)
    WHERE status = 'credited' AND reorged_at_block IS NOT NULL;

-- Admin asks for a reversal and nothing more: it may not touch the status, the
-- amounts, or the block columns. Recording a decision and acting on it are
-- different roles, which is the whole point of the split (§14).
GRANT UPDATE (reversal_requested_by, reversal_requested_at, reversal_note, version, updated_at)
  ON chain.deposits TO ex_admin;

-- +goose Down
REVOKE UPDATE (reversal_requested_by, reversal_requested_at, reversal_note, version, updated_at)
  ON chain.deposits FROM ex_admin;
DROP INDEX chain.deposits_awaiting_reversal_idx;
ALTER TABLE chain.deposits
    DROP CONSTRAINT deposits_fee_once_the_ledger_moved,
    DROP CONSTRAINT deposits_credited_amount_once_the_ledger_moved,
    ADD CONSTRAINT deposits_credited_amount_with_credited_at
        CHECK ((credited_at IS NOT NULL) = (credited_amount IS NOT NULL)),
    ADD CONSTRAINT deposits_fee_with_credited_at
        CHECK ((credited_at IS NOT NULL) = (fee IS NOT NULL));
ALTER TABLE chain.deposits
    DROP CONSTRAINT deposits_reversed_has_a_time,
    DROP CONSTRAINT deposits_reversal_is_attributed,
    DROP COLUMN reversed_at,
    DROP COLUMN reversal_error,
    DROP COLUMN reversal_note,
    DROP COLUMN reversal_requested_at,
    DROP COLUMN reversal_requested_by,
    DROP COLUMN reorged_at_block;

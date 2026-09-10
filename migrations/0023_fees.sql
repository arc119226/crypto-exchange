-- +goose Up
-- Phase 8 fees (docs/plan-v1.0.md §12 Phase 8, §23): the two rates an
-- operator sets, and the per-row snapshots that make a rate change safe for
-- money already in flight.
--
-- Every column here defaults to the behaviour that exists today, so this
-- migration alone changes nothing: withdrawal_fee_bps and deposit_fee_bps are
-- 0, the withdrawal fee snapshot is 0, and a deposit credits its full amount.
-- The fees only start moving when somebody sets a rate in the back office.

-- The two rates. registry.assets already carries withdrawal_fee, a flat
-- amount that has been stored, edited and displayed since 0002 with nothing
-- reading it (§23.3); these two join it, and Phase 8 is where all three
-- finally reach the money.
--
-- Basis points rather than a decimal rate: an integer 0..10000 cannot be
-- 0.1 + 0.2, and the whole codebase already refuses floating point on a
-- money path. 10000 is 100%, which is legal but absurd -- the guard against
-- a 100% deposit fee crediting a user zero lives in Go, where it can say why.
ALTER TABLE registry.assets
    ADD COLUMN withdrawal_fee_bps integer NOT NULL DEFAULT 0
        CHECK (withdrawal_fee_bps BETWEEN 0 AND 10000),
    ADD COLUMN deposit_fee_bps    integer NOT NULL DEFAULT 0
        CHECK (deposit_fee_bps BETWEEN 0 AND 10000);

-- The withdrawal's own copy of what it will be charged, written when the
-- request is accepted (§6.4.2). This is the point of the column: an operator
-- who raises the rate must not change the price of a withdrawal a user has
-- already agreed to, and a withdrawal that sits in the review queue overnight
-- settles at the rate it was quoted.
--
-- fee_asset is the asset the fee is charged in, which today is always the
-- asset being withdrawn -- a USDC withdrawal is charged in USDC even though
-- its gas is paid in ETH (§23.3). It is a column rather than an assumption
-- because the row has to stay readable if that ever stops being true.
ALTER TABLE chain.withdrawals
    ADD COLUMN fee       numeric(36,18) NOT NULL DEFAULT 0 CHECK (fee >= 0),
    ADD COLUMN fee_asset text;
UPDATE chain.withdrawals SET fee_asset = asset WHERE fee_asset IS NULL;
ALTER TABLE chain.withdrawals
    ALTER COLUMN fee_asset SET NOT NULL,
    ADD CONSTRAINT withdrawals_fee_asset_present CHECK (fee_asset <> '');

-- What the deposit was charged and what the user actually received. Unlike a
-- withdrawal fee, this one is deducted from the arriving amount rather than
-- added to it (§6.1.4 i), so amount stays "what the chain delivered" and
-- credited_amount is "what became the user's balance". Keeping both means a
-- deposit row can be reconciled against the chain and against the ledger
-- without either number having to be derived.
--
-- Both are computed at credit time, so they exist exactly when credited_at
-- does. Tying the constraint to credited_at rather than to status is
-- deliberate: the table already uses credited_at as the marker that the
-- ledger was touched, and a reversal clears it.
ALTER TABLE chain.deposits
    ADD COLUMN fee             numeric(36,18) CHECK (fee IS NULL OR fee >= 0),
    ADD COLUMN credited_amount numeric(36,18) CHECK (credited_amount IS NULL OR credited_amount >= 0);
UPDATE chain.deposits SET fee = 0, credited_amount = amount WHERE credited_at IS NOT NULL;
ALTER TABLE chain.deposits
    ADD CONSTRAINT deposits_credited_amount_with_credited_at
        CHECK ((credited_at IS NOT NULL) = (credited_amount IS NOT NULL)),
    ADD CONSTRAINT deposits_fee_with_credited_at
        CHECK ((credited_at IS NOT NULL) = (fee IS NOT NULL)),
    -- The fee comes out of the amount, so it cannot exceed it. A withdrawal
    -- fee has no such relation: it is charged on top.
    ADD CONSTRAINT deposits_fee_within_amount
        CHECK (fee IS NULL OR fee <= amount);

-- The api role inserts withdrawals, and now inserts the fee snapshot with
-- them. INSERT is granted on the whole table (0010), so the new columns are
-- already covered; the chain role's column-level UPDATE grants are not, and
-- nothing updates the snapshot after the row exists -- that is the point.

-- +goose Down
ALTER TABLE chain.deposits
    DROP CONSTRAINT deposits_fee_within_amount,
    DROP CONSTRAINT deposits_fee_with_credited_at,
    DROP CONSTRAINT deposits_credited_amount_with_credited_at,
    DROP COLUMN credited_amount,
    DROP COLUMN fee;
ALTER TABLE chain.withdrawals
    DROP CONSTRAINT withdrawals_fee_asset_present,
    DROP COLUMN fee_asset,
    DROP COLUMN fee;
ALTER TABLE registry.assets
    DROP COLUMN deposit_fee_bps,
    DROP COLUMN withdrawal_fee_bps;

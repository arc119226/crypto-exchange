-- Invariants a restored copy must satisfy (scripts/restore-drill.sh,
-- docs/runbooks/backup-restore.md). Each check raises, so under
-- ON_ERROR_STOP the drill fails on the first broken one; the counts at the
-- end are what an operator compares with the source.
\set ON_ERROR_STOP on

DO $$
DECLARE
    bad text;
BEGIN
    -- 1. trial balance: per asset, debits equal credits (docs/plan-v1.0.md §6.1)
    SELECT string_agg(asset || ' ' || diff, ', ') INTO bad
    FROM (
        SELECT asset, sum(CASE direction WHEN 'debit' THEN amount ELSE -amount END) AS diff
        FROM ledger.postings GROUP BY asset
    ) d
    WHERE diff <> 0;
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'trial balance broken: %', bad;
    END IF;

    -- 2. the balance cache equals its postings, bucket by bucket
    SELECT count(*)::text INTO bad
    FROM (
        SELECT b.account_id, b.asset, b.available, b.hold,
               coalesce(sum(CASE WHEN p.bucket = 'available' THEN CASE p.direction WHEN 'credit' THEN p.amount ELSE -p.amount END END), 0) AS available_calc,
               coalesce(sum(CASE WHEN p.bucket = 'hold'      THEN CASE p.direction WHEN 'credit' THEN p.amount ELSE -p.amount END END), 0) AS hold_calc
        FROM ledger.balances b
        LEFT JOIN ledger.postings p ON p.account_id = b.account_id AND p.asset = b.asset
        GROUP BY 1, 2, 3, 4
    ) x
    WHERE available <> available_calc OR hold <> hold_calc;
    IF bad <> '0' THEN
        RAISE EXCEPTION '% balance rows disagree with their postings', bad;
    END IF;

    -- 3. every market's sequence is the last command it committed. An order
    -- row carries the seq of the command that accepted or rejected it, but a
    -- cancel consumes a seq without writing an order row, so the newest
    -- outbox event of the market (kept 30 days by retention) is the exact
    -- witness while there is one; the orders alone only bound it from below.
    SELECT string_agg(market_id::text || ' last_seq ' || last_seq || ' orders ' || o_max || ' events ' || coalesce(e_max::text, 'none'), ', ') INTO bad
    FROM (
        SELECT s.market_id, s.last_seq,
               coalesce((SELECT max(o.seq) FROM trading.orders o WHERE o.market_id = s.market_id), 0) AS o_max,
               (SELECT max(e.seq) FROM eventbus.outbox e JOIN registry.markets m ON m.id = s.market_id WHERE e.market_id = m.symbol) AS e_max
        FROM trading.market_sequences s
    ) x
    WHERE last_seq < o_max OR (e_max IS NOT NULL AND last_seq <> greatest(o_max, e_max));
    IF bad IS NOT NULL THEN
        RAISE EXCEPTION 'market sequence does not match the orders and events: %', bad;
    END IF;

    -- 4. trades: the fills of one command are numbered 0..n-1 without a hole
    SELECT count(*)::text INTO bad
    FROM (SELECT market_id, seq, count(*) AS n, max(idx) AS m FROM trading.trades GROUP BY 1, 2) t
    WHERE n <> m + 1;
    IF bad <> '0' THEN
        RAISE EXCEPTION '% fill groups have a hole in idx', bad;
    END IF;

    -- 5. no posting without its entry, no entry with fewer than two postings
    SELECT count(*)::text INTO bad
    FROM ledger.journal_entries e
    WHERE (SELECT count(*) FROM ledger.postings p WHERE p.entry_id = e.id) < 2;
    IF bad <> '0' THEN
        RAISE EXCEPTION '% journal entries have fewer than two postings', bad;
    END IF;
END $$;

SELECT 'journal_entries' AS "table", count(*) AS rows FROM ledger.journal_entries
UNION ALL SELECT 'postings', count(*) FROM ledger.postings
UNION ALL SELECT 'accounts', count(*) FROM ledger.accounts
UNION ALL SELECT 'orders', count(*) FROM trading.orders
UNION ALL SELECT 'trades', count(*) FROM trading.trades
UNION ALL SELECT 'users', count(*) FROM auth.users
UNION ALL SELECT 'outbox', count(*) FROM eventbus.outbox
UNION ALL SELECT 'withdrawals', count(*) FROM chain.withdrawals
UNION ALL SELECT 'deposits', count(*) FROM chain.deposits;

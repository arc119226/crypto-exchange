// Package ledger is the double-entry book of the exchange and the only
// writer of balances (docs/plan-v1.0.md §4 principle 3, §6.1, ADR-0005).
//
// Every money movement is a journal entry made of postings; within one entry
// each asset's debits equal its credits (checked in Go before writing and by
// a deferred trigger at commit). Spot accounts have an available and a hold
// bucket whose liability balances are cached in ledger.balances inside the
// same transaction; house accounts (fee revenue, gas expense, custody, pending
// withdrawal, external) are derived from postings. Holds are entries too:
// Hold moves available → hold, Release moves it back, Settle moves hold to
// the counterparty's available and charges fees in the received asset.
//
// Callers own the transaction: Post and its wrappers take a pgx.Tx so the
// engine can write orders, trades, ledger entries and outbox rows atomically.
// Every entry carries an idempotency key; replaying a key returns the
// original entry and changes nothing.
package ledger

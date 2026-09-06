// Package matching is the pure, deterministic order book of the exchange
// (docs/plan-v1.0.md §6.3, ADR-0002). It has no I/O, no clock, no
// randomness and no goroutines: the engine (internal/trading, Phase 3)
// assigns every command a strictly increasing seq and a timestamp, calls
// Apply inside a Postgres transaction, and persists the returned events.
// Replaying the same command sequence on an empty Book therefore yields the
// same events and a bit-identical Snapshot, which is what recovery after a
// restart relies on.
//
// Two design notes for readers new to matching engines:
//
// Why a FIFO per price level instead of one sorted slice of orders: a
// level is the unit of price-time priority. Inserting a resting order is
// "append to its level"; matching consumes from the head of the best level;
// cancelling removes one order from one level. A single slice sorted by
// (price, seq) would make every insert and cancel a shift of the whole
// tail and would hide the level quantity that market data (depth) needs.
//
// Why Apply must never call time.Now(): the engine replays commands to
// rebuild the book after a crash. Any value the book reads from the outside
// world (clock, random ids, map iteration order) would make the replayed
// book differ from the original one. Timestamps travel inside Command; map
// lookups are only used for O(1) access, iteration always walks sorted
// slices.
package matching

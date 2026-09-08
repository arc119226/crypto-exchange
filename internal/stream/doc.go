// Package stream is the WebSocket server of docs/plan-v1.0.md §7.5 and
// docs/ws-api.md: public market-data channels (depth, trades, ticker,
// kline.*) fed by the shadow book, and private per-account channels
// (orders, fills, balances, deposits, withdrawals) with a resume that
// replays from the outbox.
//
// One goroutine writes to each connection through a bounded buffer; a
// client that cannot keep up is disconnected rather than allowed to slow
// the broadcast (§7.5). Everything it serves is derived from events and
// rebuilt from Postgres on a gap (design principle 5).
package stream

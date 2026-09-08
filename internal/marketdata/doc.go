// Package marketdata derives market data from the engine's events
// (docs/plan-v1.0.md §7.5, §12 Phase 6): a shadow order book that turns
// order.* / trade.* events into depth deltas, candle (K-line) aggregation,
// the 24-hour ticker, and the caches that keep the api role from asking the
// engine for every snapshot.
//
// Everything here is a derived view. Postgres is the truth (design principle
// 5): the shadow book is rebuilt from trading.orders whenever a sequence gap
// shows up, and candles are folded from trading.trades, never from memory
// alone.
package marketdata

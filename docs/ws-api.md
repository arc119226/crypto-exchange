# WebSocket API

The stream role serves market data and account events over WebSocket
(docs/plan-v1.0.md §7.5). Two endpoints:

| Endpoint | What it carries | Authentication |
|---|---|---|
| `ws://host:8081/ws/v1/public` | order book depth, trades, ticker, candles | none |
| `ws://host:8081/ws/v1/private` | the account's orders, fills, balances, deposits, withdrawals | the first frame must be `auth` with a JWT access token |

Every frame is one JSON object. Amounts are **decimal strings**
(`docs/api-conventions.md`); prices and quantities in the public channels
are `[price, qty]` string tuples. Times are RFC 3339 UTC.

The same events reach customers as webhooks (`docs/webhooks.md`) and the
event contract itself is in `docs/events.md`; this page is the wire format
of the stream only.

## Client operations

| Frame | Where | Effect |
|---|---|---|
| `{"op":"subscribe","channel":"depth","market":"ETH-USDC"}` | public | start a channel; the first message is the snapshot (depth) or the current value (ticker, candle) |
| `{"op":"unsubscribe","channel":"depth","market":"ETH-USDC"}` | public | stop it |
| `{"op":"ping"}` | both | answered with `{"type":"pong"}` |
| `{"op":"auth","token":"<jwt>"}` | private | authenticate; must be the first frame, within `STREAM_AUTH_TIMEOUT` (5 s) |
| `{"op":"resume","since_seq":N}` | private | replay account events with `account_seq > N`, then go live |

Public channels: `depth`, `trades`, `ticker`, `kline.1m`, `kline.5m`,
`kline.15m`, `kline.1h`, `kline.1d`. A connection may hold at most
`STREAM_MAX_SUBSCRIPTIONS` (64) subscriptions. Private channels need no
subscription: after `auth` the connection receives all of them.

Every operation is acknowledged: `{"type":"subscribed","channel":"depth","market":"ETH-USDC"}`,
`{"type":"unsubscribed",...}`, `{"type":"pong"}`, `{"type":"auth",...}`,
`{"type":"resumed",...}`, or `{"type":"error","code":"...","message":"..."}`.

## Public channels

### depth

```json
{"channel":"depth","type":"snapshot","market":"ETH-USDC","seq":18234,"at":"2026-09-08T07:24:30.412Z",
 "bids":[["1990.00","0.4000"],["1989.50","1.2000"]],"asks":[["1990.50","0.7000"]]}
{"channel":"depth","type":"delta","market":"ETH-USDC","seq":18235,"at":"2026-09-08T07:24:30.418Z",
 "bids":[["1990.00","0"]],"asks":[["1991.00","0.1000"]]}
```

- `seq` is the engine's per-market command sequence: every accepted command
  the engine applied produces exactly one delta, even when it changed
  nothing (an empty `bids` and `asks`, e.g. a post-book rejection). Deltas
  are therefore **contiguous**.
- A delta level is the level's new total quantity, not a change; `"0"`
  removes it. Levels are the top `STREAM_DEPTH_LEVELS` (200) per side.
- `at` is the `occurred_at` of the last event in the command, so a client on
  a synchronised clock can measure push latency.

**Client rules** (docs/plan-v1.0.md §7.5, implemented in
`web/trade/src/components/OrderBook.tsx`):

1. Take the snapshot (`seq = S`). `GET /v1/markets/{symbol}/depth` is an
   equivalent starting point (`last_seq`).
2. Drop every delta with `seq <= S`.
3. Apply deltas whose `seq == S + 1`, advancing `S`.
4. Any other `seq` is a gap: unsubscribe, subscribe again, start over from
   the new snapshot. The server also re-sends a snapshot on its own when its
   shadow book had to be rebuilt.

### trades

```json
{"channel":"trades","type":"update","market":"ETH-USDC","seq":18236,"at":"2026-09-08T07:24:31.002Z",
 "trade":{"trade_id":"01M1ZY...","price":"1990.00","qty":"0.1000","quote_qty":"199.00","taker_side":"sell","executed_at":"2026-09-08T07:24:31.001Z"}}
```

One message per trade, in `seq` order; a command with several fills sends
several messages with the same `seq`.

### ticker

```json
{"channel":"ticker","type":"update","market":"ETH-USDC","at":"2026-09-08T07:24:31.100Z",
 "ticker":{"market":"ETH-USDC","last_price":"1990.00","open":"1980.00","high":"1995.00","low":"1975.50","change":"10.00","change_pct":"0.50",
           "volume":"12.3400","quote_volume":"24512.10","trades":57,"at":"2026-09-08T07:24:31.100Z"}}
```

The same object as `GET /v1/markets/{symbol}/ticker`: a rolling 24-hour
window folded from 1-minute candles. Sent on subscribe when the market has
traded, then at most once per `STREAM_TICKER_INTERVAL` (1 s) while trades
happen. The price fields are absent until the market has traded in the
window.

### kline.{interval}

```json
{"channel":"kline.1m","type":"update","market":"ETH-USDC","at":"2026-09-08T07:24:31.100Z",
 "candle":{"market":"ETH-USDC","interval":"1m","start":"2026-09-08T07:24:00Z","open":"1990.00","high":"1990.00","low":"1989.50","close":"1989.50",
           "volume":"0.3000","quote_volume":"596.95","trades":3}}
```

The current bucket, sent on subscribe and then at most once per
`STREAM_KLINE_INTERVAL` (250 ms) per market and interval while it changes.
Buckets are UTC aligned. History comes from `GET /v1/markets/{symbol}/klines`;
a chart loads history, subscribes, and applies updates whose `start` is
greater than or equal to its last bar.

## Private channels

```json
{"op":"auth","token":"eyJ..."}
{"type":"auth","account_id":"3ecd4661-...","account_seq":412}
```

The token is the same JWT access token the REST API takes (`aud`
`exchange`); API-key HMAC is not accepted on the stream in v1. A frozen
user's connection lives until the token expires (15 minutes), like any
bearer token. `account_seq` in the acknowledgement is the account's
sequence at that moment: everything up to it is already visible through
the REST lists, everything after it will arrive as frames.

Frames:

```json
{"channel":"orders","type":"order.filled","seq":18236,"account_seq":413,"event_id":"01M1ZY...","occurred_at":"2026-09-08T07:24:31.001Z",
 "data":{"order_id":"01M1ZY...","market":"ETH-USDC","status":"filled","filled_qty":"0.1000","...":"..."}}
{"channel":"fills","type":"trade.executed","role":"taker","seq":18236,"account_seq":null,"event_id":"01M1ZY...","occurred_at":"...","data":{"trade_id":"...","...":"..."}}
{"channel":"balances","type":"balance.updated","seq":null,"account_seq":414,"event_id":"...","occurred_at":"...","data":{"asset":"USDC","available":"...","hold":"..."}}
```

| Channel | Event types | `account_seq` | Resumable |
|---|---|---|---|
| `orders` | `order.accepted`, `order.updated`, `order.filled`, `order.cancelled`, `order.rejected` | yes | yes |
| `balances` | `balance.updated` | yes | yes |
| `deposits` | `deposit.detected`, `deposit.credited`, `deposit.orphaned`, `deposit.dropped`, `deposit.reversed` | yes | yes |
| `withdrawals` | `withdrawal.requested`, `withdrawal.state_changed` | yes | yes |
| `fills` | `trade.executed` with `role` `maker` or `taker` | `null` | **no** — live only |

`data` is the event payload of `docs/events.md` for that type, unchanged.

Order account events by `account_seq`; it is contiguous per account. `fills`
carries no `account_seq` because a trade belongs to two accounts and to the
market sequence; reconcile fills after a reconnect with `GET /v1/fills`.
`balance.updated` is the engine's: deposits, withdrawals and administrative
adjustments do not emit it, which is why the `deposits` and `withdrawals`
channels exist — on either, refetch `GET /v1/balances`.

### Resuming

After `auth` the server holds the account's live frames for
`STREAM_RESUME_WINDOW` (1 s) waiting for the client's first operation:

- `{"op":"resume","since_seq":N}` replays outbox rows with `account_seq > N`
  (pages of `STREAM_RESUME_PAGE_SIZE`), answers
  `{"type":"resumed","since_seq":N,"replayed":K}`, then releases the held
  frames whose `account_seq` is greater than the last replayed one, then
  goes live. No frame is lost or duplicated across the join.
- any other operation, or the window expiring, goes live from the
  acknowledgement's `account_seq` without replay.

A `resume` sent once the connection is live is answered with
`resume_too_late`: reconnect and resume on the new connection. The outbox
keeps 30 days, so `since_seq` older than that is answered with
`resume_failed`; the client then reloads its lists over REST. The reference
front end (`web/trade/src/ws/private.ts`) remembers the highest
`account_seq` it saw and resumes from it after every reconnect.

## Errors and disconnects

`{"type":"error","code":"...","message":"..."}` codes:

| Code | Meaning |
|---|---|
| `bad_message` | not JSON, unknown `op`, or a field of the wrong shape; five in a row close the connection |
| `unknown_channel` | not one of the public channels |
| `unknown_market` | the registry has no such market |
| `too_many_subscriptions` | over `STREAM_MAX_SUBSCRIPTIONS` |
| `auth_required` | a private operation before `auth` |
| `auth_failed` | the token did not verify (expired, wrong audience, wrong tenant) |
| `not_authenticated` | `resume` before `auth` |
| `resume_too_late` | `resume` after the connection went live |
| `resume_failed` | the replay could not be served; reload over REST |
| `not_available` | the server has no shadow book for the market yet (rebuilding) |

Close codes: `1008` with reason `auth_required` (no `auth` within the
timeout), `auth_failed`, `slow_consumer`, `bad_message` or `pong_timeout`;
`1001` `shutdown` when the server stops (reconnect after a moment).

**Slow consumers.** Each connection has a send queue of `STREAM_WRITE_BUFFER`
(256) frames. A client that lets it fill is closed with `slow_consumer`
rather than slowing everyone else down; on reconnect it takes a fresh
snapshot. Depth deltas are never coalesced (the contract needs contiguous
`seq`); `docs/loadtest.md` records what one server fanned out.

**Heartbeat.** The server pings every `STREAM_PING_INTERVAL` (15 s); a
browser answers automatically. No pong within `STREAM_PONG_TIMEOUT` (15 s)
closes the connection with `pong_timeout`.

**Origins.** Browsers are admitted by `STREAM_ALLOWED_ORIGINS` (patterns;
`*` is allowed only with `EXCHANGE_ENV=dev`). Non-browser clients send no
`Origin` and are always admitted. The reference front end reaches the
stream through its own origin (Vite proxy in development, the same host in
a deployment), so a deployment lists nothing here.

## Trying it

```sh
# any WebSocket client; websocat shown
websocat ws://localhost:8081/ws/v1/public <<< '{"op":"subscribe","channel":"depth","market":"ETH-USDC"}'
websocat ws://localhost:8081/ws/v1/private <<< "{\"op\":\"auth\",\"token\":\"$EXCHANGE_TOKEN\"}"

# the load generator is also a client: it subscribes depth + trades and times the pushes
go run ./cmd/exchangectl loadgen --market ETH-USDC --rate 20 --duration 10s --accounts 2 --ws-clients 1
```

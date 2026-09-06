# Event contract

The exchange publishes every state change as an event. This page and the
JSON Schemas in `api/events/v1/` are the contract; `test/contract` fails if
the code and these files drift apart.

Events are **not** how money moves. Balances, orders and trades are written
in one Postgres transaction together with the outbox row that carries the
event, so an event exists exactly when the change it describes is committed
(ADR-0002). Events are the fan-out: market data, private streams, webhooks,
the back office, and the engine's own registry reload.

## Envelope

Every message has the same envelope (`api/events/v1/envelope.json`):

```json
{
  "event_id": "01J8Z2K3M4N5P6Q7R8S9T0V1W7",
  "event_type": "trade.executed",
  "schema_version": 1,
  "tenant_id": "default",
  "market_id": "ETH-USDC",
  "account_id": null,
  "seq": 18234,
  "account_seq": null,
  "occurred_at": "2026-09-05T08:15:23.412Z",
  "correlation_id": "req_01J8Z2K2000000000000000000",
  "payload": { "trade_id": "01J8Z2K3M4N5P6Q7R8S9T0V400", "price": "1990", "...": "..." }
}
```

| Field | Meaning |
|---|---|
| `event_id` | ULID, unique per event. **Deduplicate on this.** Delivery is at least once. |
| `event_type` | `<domain>.<type>`; neither half contains a dot, so it maps onto the subject. |
| `schema_version` | Version of `payload` for this `event_type`. |
| `tenant_id` | Always `default` in v1 (single tenant, ADR-0003). |
| `market_id` | Market symbol for market-domain events, `null` otherwise. |
| `account_id` | The account the event belongs to, `null` for market-only events. |
| `seq` | Per-market engine sequence. **Order market-domain events by this**, never by arrival. |
| `account_seq` | Per-account sequence. Order account-domain events by this; a gap means you missed events. |
| `occurred_at` | When the producing transaction committed (UTC, microsecond precision). |
| `correlation_id` | The `X-Request-Id` of the API call that caused it. Quote it in bug reports. |
| `causation_id` | The event that caused this one, when there is one. |
| `payload` | Type-specific body; see the schema named after `event_type`. |

Amounts in payloads are **decimal strings**, never JSON numbers
(`docs/api-conventions.md`).

## Ordering, gaps and duplicates

- Two counters, two scopes. `seq` is per market and advances once per engine
  command; `account_seq` is per account and advances once per event that
  concerns it. An event can carry both.
- **Never order by receipt.** The relay publishes in outbox id order, but
  ids are taken before commit, so two transactions can be published out of
  order relative to each other. Within one market or one account the
  sequences are authoritative.
- A gap in `seq` for a market you are following means you missed events:
  re-snapshot (`GET /v1/markets/{symbol}/depth` carries `last_seq`). A gap in
  `account_seq` means the same for that account.
- Duplicates are normal and expected on redelivery. `event_id` is the
  deduplication key; JetStream also drops republished duplicates inside its
  window.

## Subjects and streams

Subject layout: `ex.v1.<domain>.<type>.<tenant>.<scope>`, where `scope` is the
market symbol for market-domain events, the account id for account-only
events, and `_` when neither applies.

| Stream | Subjects | Retention |
|---|---|---|
| `EX_TRADING` | `ex.v1.order.*.*.*`, `ex.v1.trade.*.*.*`, `ex.v1.ledger.*.*.*`, `ex.v1.balance.*.*.*` | 30 days, file |
| `EX_CHAIN` | `ex.v1.deposit.*.*.*`, `ex.v1.withdrawal.*.*.*`, `ex.v1.sweep.*.*.*`, `ex.v1.alert.*.*.*` | 30 days |
| `EX_REGISTRY` | `ex.v1.market.*.*.*`, `ex.v1.asset.*.*.*`, `ex.v1.fee_schedule.*.*.*`, `ex.v1.user.*.*.*`, `ex.v1.reconciliation.*.*.*` | 90 days |

## Consumers

Two kinds, and picking the wrong one is a silent bug:

- **Fan-out** — every replica needs every event (the WebSocket servers).
  Use an *ephemeral ordered* consumer per replica and deduplicate on `seq` /
  `account_seq`. A shared durable consumer is a work queue: each replica
  would receive only a slice of the stream.
- **Processing** — the work must happen exactly once (webhook delivery,
  K-line aggregation, the back-office projection). Use a *durable* consumer
  with explicit ack (`eventbus.Subscribe`), write your effect and
  `eventbus.MarkProcessed` in the same transaction, and treat a duplicate
  key as "already done" and ack. A handler whose effect is idempotent by
  construction — the engine's registry reload, which only re-reads Postgres —
  needs no `processed_events` row.

## Catalog

`shipped` means the code publishes it today and `api/events/v1/` holds its
schema; `planned` means the phase that adds it will add the schema with it.

| event_type | Producer | Consumers | Status |
|---|---|---|---|
| `order.accepted` | engine | stream (private, depth), webhook | shipped |
| `order.updated` | engine | stream, webhook | shipped |
| `order.filled` | engine | stream, webhook | shipped |
| `order.cancelled` | engine | stream (private, depth), webhook | shipped |
| `order.rejected` | engine | stream, webhook | shipped |
| `trade.executed` | engine | stream (trades, ticker, kline), worker, webhook | shipped |
| `balance.updated` | engine, chain, admin | stream (private), webhook | shipped |
| `market.updated` | admin | **engine (reload)**, stream, webhook | shipped |
| `ledger.posted` | engine, chain, admin | stream (balances), back-office projection | planned (Phase 5) |
| `asset.updated`, `fee_schedule.updated` | admin | engine (reload), webhook | planned (Phase 5) |
| `deposit.detected`, `deposit.credited`, `deposit.orphaned`, `deposit.dropped`, `deposit.reversed` | chain | stream, webhook, admin | shipped |
| `withdrawal.requested` | api | stream, webhook, admin | shipped |
| `withdrawal.state_changed` | api, chain, admin | stream, webhook, admin | shipped |
| `sweep.completed`, `sweep.failed` | chain | admin | planned (Phase 4c) |
| `alert.hot_wallet_low` | chain | webhook, admin | planned (Phase 4c) |
| `reconciliation.break_detected` | worker | webhook, admin | planned (Phase 4c) |
| `user.kyc_level_updated`, `user.status_updated` | admin | webhook | planned (Phase 5) |

`deposit.*` events all share one payload: what a consumer needs about a
deposit does not change with the way it moved, and the difference lives in the
event type. `deposit.detected` is **not** money — nothing is credited until
`deposit.credited`, and a reorg before that point produces `deposit.orphaned`
instead. The same deposit can be detected more than once: after an orphan it
reappears under the same `deposit_id`, with a different `block_number`.
`deposit.reversed` is an alert rather than a completed reversal — the
reversing entry is made by hand once an operator confirms it (§6.4.1).

`withdrawal.*` is two types rather than one per state. `withdrawal.requested`
marks the row coming into existence and `withdrawal.state_changed` carries
every move after it, naming both ends in `previous_status` and `status`. The
state machine is still growing — signing, broadcasting and confirmation join
it with the signer role — and a type per state would multiply the contract by
the length of the machine for consumers that subscribe to all of them anyway.
`withdrawal.requested` is the one event the **api** role produces; every other
transition comes from `chain` or `admin`.

`tx_hash` appears on `withdrawal.state_changed` once a transaction exists. It
is the transaction *now representing* the withdrawal, which is not always the
withdrawal's own: while a stuck withdrawal is being cancelled with a same-nonce
transfer, this is the displacing transaction, because that is the one whose
fate decides the money. A consumer that wants to show a user "your withdrawal
is on chain" should link this hash and no other.

The chain role writes these to the outbox; the **relay that publishes them
runs in the engine role**, so a deployment without an engine leaves deposit
and withdrawal events sitting in `eventbus.outbox`.

`order.*` and `trade.executed` carry `seq`; `order.*` and `balance.updated`
carry `account_seq`. `order.accepted` is emitted for **every** order that was
not rejected — including one that fills in the same command — so a private
stream client can map every later event back to its `client_order_id`.

## Compatibility

- **Adding a field is not a breaking change.** Ignore fields you do not know;
  do not fail on them. Enumerations may gain values (`reject_reason` is an
  open string for exactly this reason).
- Removing or renaming a field, or changing what one means, bumps
  `schema_version` and adds a new schema file next to the old one. Both
  versions are then published until the old one is retired.
- The golden files in `test/golden/events/` pin the exact serialisation of
  every shipped event. Changing one is a contract change, and the diff is
  meant to be noticed in review; regenerate deliberately with
  `UPDATE_GOLDEN=1 go test ./test/contract/`.

## Delivery to customers

Customers do not subscribe to NATS. They receive events through:

- the **private WebSocket** channels (`orders`, `fills`, `balances`), with
  `account_seq` and a `resume` from the outbox — Phase 6;
- **outbound webhooks**, HMAC-signed with retries and a delivery log —
  Phase 5 (`docs/webhooks.md`).

Both carry the same envelope as above.

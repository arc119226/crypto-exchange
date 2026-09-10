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
  Use an *ephemeral ordered* consumer per replica (`eventbus.SubscribeOrdered`)
  and deduplicate on `seq` / `account_seq`. A shared durable consumer is a
  work queue: each replica would receive only a slice of the stream. An
  ordered consumer starts from "now" and is not durable; the stream role
  rebuilds its shadow books from `trading.orders` and its candles from
  `marketdata.klines` after it subscribes, so nothing committed in between
  is lost (`docs/domain.md` §25).
- **Processing** — the work must happen exactly once (webhook delivery,
  K-line aggregation, the back-office projection). Use a *durable* consumer
  with explicit ack (`eventbus.Subscribe`), write your effect and
  `eventbus.MarkProcessed` in the same transaction, and treat a duplicate
  key as "already done" and ack. A handler whose effect is idempotent by
  construction — the engine's registry reload, which only re-reads Postgres —
  needs no `processed_events` row. K-line aggregation is the exception that
  uses neither: the worker polls `trading.trades` by sequence and writes the
  candles and its cursor in one transaction, because a durable consumer
  that naks one trade lets the ones behind it through.

Both consumer kinds hand the handler a context carrying the event's
correlation id and its W3C trace context: the outbox row stores
`Correlation-Id` and `traceparent` in `headers`, the relay copies them onto
the NATS message, and the consumer opens its span under the request that
produced the event (`docs/plan-v1.0.md` §15).

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
| `trade.executed` | engine | stream (trades, ticker, kline; private `fills`), worker (klines), webhook | shipped |
| `balance.updated` | engine | stream (private `balances`), webhook | shipped |
| `market.updated` | admin | **engine (reload)**, stream, webhook | shipped |
| `ledger.posted` | — | — | not in v1: the private `balances` channel carries the engine's `balance.updated`; deposits, withdrawals and adjustments reach the stream as their own events and the client refetches balances (`docs/ws-api.md`) |
| `asset.updated`, `fee_schedule.updated` | admin | engine (reload), webhook | shipped |
| `registry.reload` | admin | engine (reload), webhook | shipped |
| `deposit.detected`, `deposit.credited`, `deposit.orphaned`, `deposit.dropped`, `deposit.reversed` | chain | stream (private `deposits`), webhook, admin | shipped |
| `withdrawal.requested` | api | stream (private `withdrawals`), webhook, admin | shipped |
| `withdrawal.state_changed` | api, chain, admin | stream (private `withdrawals`), webhook, admin | shipped |
| `sweep.completed`, `sweep.failed` | chain | admin | shipped |
| `alert.hot_wallet_low` | chain | webhook, admin | shipped |
| `reconciliation.break_detected` | **chain** | webhook, admin | shipped |
| `reconciliation.ledger_break_detected` | **admin** | webhook, admin | shipped |
| `user.status_updated`, `user.kyc_level_updated` | admin | webhook, admin | shipped |

`deposit.*` events all share one payload: what a consumer needs about a
deposit does not change with the way it moved, and the difference lives in the
event type. `deposit.detected` is **not** money — nothing is credited until
`deposit.credited`, and a reorg before that point produces `deposit.orphaned`
instead. The same deposit can be detected more than once: after an orphan it
reappears under the same `deposit_id`, with a different `block_number`.
`deposit.reversed` reports a completed reversal, not a warning about one: by
the time it is emitted the reversing entry has been posted, mirroring the three
postings that credited the deposit. Getting there takes a person. A reorg
deeper than the asset's confirmations leaves the deposit `credited` and marks
it; the alert is `DepositAwaitingReversal` on
`deposits_awaiting_reversal`, and an operator confirms in the back office
(§6.4.1). The machine never decides it, because the account may already have
spent the money — and if it has, the reversal is refused and the choice goes
back to a person.

`fee` and `credited_amount` appear once the deposit is credited and are absent
before that: a deposit fee comes *out of* what arrived (§6.1.4 i), so `amount`
stays what the chain delivered and `credited_amount` is what became the
balance. Both ship at zero and a fee of nothing is not the same fact as no fee
computed yet, which is why the fields are absent rather than `"0"` on
`deposit.detected`.

`withdrawal.*` is two types rather than one per state. `withdrawal.requested`
marks the row coming into existence and `withdrawal.state_changed` carries
every move after it, naming both ends in `previous_status` and `status`. The
state machine is still growing — signing, broadcasting and confirmation join
it with the signer role — and a type per state would multiply the contract by
the length of the machine for consumers that subscribe to all of them anyway.
`withdrawal.requested` is the one event the **api** role produces; every other
transition comes from `chain` or `admin`.

`fee` and `fee_asset` are on both types and always present. Unlike a deposit
fee this one is charged *on top*: the destination receives `amount` and the
account is debited `amount + fee`. It is quoted when the request is accepted
and snapshotted on the row, so raising a rate never reprices a withdrawal
already in the queue; and it stays the account's money until the transaction
confirms, so every ending that is not `confirmed` returns it.

`tx_hash` appears on `withdrawal.state_changed` once a transaction exists. It
is the transaction *now representing* the withdrawal, which is not always the
withdrawal's own: while a stuck withdrawal is being cancelled with a same-nonce
transfer, this is the displacing transaction, because that is the one whose
fate decides the money. A consumer that wants to show a user "your withdrawal
is on chain" should link this hash and no other.

`sweep.*` is the one pair with **no `account_id` and no sequence**. A sweep
belongs to no user: it moves the exchange's own custody from the address a
deposit landed on to the hot wallet withdrawals are paid from, and the account
that owns that address sees no change at all. Its subject therefore ends in the
house scope `_`, like `market.updated` but for the opposite reason — that one
is scoped to every account, this one to none. Only the two ends are published;
the intermediate states are the exchange rearranging its own money and live in
the audit trail.

`gas_cost` on a sweep is in the chain's native coin, which is not necessarily
`asset`: a USDC sweep burns ETH. A consumer that adds the two together is
adding two currencies.

`reconciliation.break_detected` is listed against **chain**, not the worker
role §6.4.4 names. Reconciliation reads on-chain balances, and the chain role
is the only one that dials a node; giving the worker its own connection to
match a word would buy nothing. The role that displays a report still cannot
produce one, which is the split withdrawal review already has.

Both `reconciliation.break_detected` and `alert.hot_wallet_low` are
**edge-triggered**, and for the same reason: each describes a condition rather
than an occurrence. A break that stays open is real and stays visible in the
report and in `reconciliation_diff`, so re-announcing it every few minutes
would only teach the people who receive it to filter it out. A break is
therefore published when it appears or when its size changes, and a low hot
wallet when the balance crosses the line — remembered in
`chain.hot_wallets.low_alerted_at`, so a restart does not re-announce it
either. A consumer that wants the current state should read
`GET /admin/v1/reconciliation`, not count events.

`reconciliation.ledger_break_detected` is the other half of §6.4.4, the one
that needs no node: the ledger against itself, one asset whose debits and
credits disagree. The **admin** role finds it on the same thirty-second loop
that refreshes `ledger_trial_balance_diff`, records it in
`admin.ledger_breaks`, and publishes it from the same transaction. The two
break events are deliberately two types rather than one with optional fields:
they are found by different roles, argued about against different terms, and
a consumer that subscribes to one should not have to check which kind arrived.
Like its sibling it is edge-triggered -- a break that stays out by the same
amount is not re-announced, one whose size changes is (the old row is resolved
and a new one opened, so the history keeps every size it had) -- and it closes
on its own when the books agree again.

`reconciliation.break_detected` carries every term of the comparison and not
just `diff`, because the terms are what say where to look: a difference the
same size as an uncredited deposit and a difference nothing explains want very
different people woken up.

The chain role writes these to the outbox; the **relay that publishes them
runs in the engine role**, so a deployment without an engine leaves deposit,
withdrawal, sweep, reconciliation and alert events sitting in
`eventbus.outbox`.

`asset.updated` and `fee_schedule.updated` complete the registry set, and
`market.updated` is now also sent when a market is created or edited (with
`changed_fields` naming what), not only when its status moves. A fee schedule
change sends **no** `market.updated` for the markets on it: the engine reloads
its whole cache on any registry event, so one event is enough for it, and a
consumer that shows per-market fees should reload on `fee_schedule.updated`
rather than expect one event per market. `registry.reload` is an operator
asking every engine to reload with no row changed -- after a seed, or when in
doubt -- and carries only the reason; the audit trail has who asked.

`user.*` is what an operator does to a person from the back office, and is
the other pair with **no `account_id` and no sequence**: a user is not an
account, so the subject ends in the house scope. `user.status_updated` names
both ends. A freeze also freezes the user's spot account in the same
transaction, and that account change is *not* a separate event: the ledger
does not announce status, and a consumer that wants the account's state reads
`GET /admin/v1/accounts/{id}`. `user.kyc_level_updated` changes what the
withdrawal policy will decide next; nothing already pending is re-decided.
Both are produced by the **admin** role, which has no relay of its own — the
engine's relay publishes them, like every other outbox row.

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

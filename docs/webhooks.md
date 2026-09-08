# Outbound webhooks

The exchange can POST every event it publishes to an HTTP endpoint you
control. It is one of the three integration surfaces — REST, WebSocket,
webhooks — and the only one where the exchange calls you.

The events themselves are the ones in [`docs/events.md`](events.md); this page
is about how they arrive, how to prove they came from the exchange, and what
happens when your endpoint is down.

## The delivery

A delivery is a `POST` to your URL whose body is the event envelope, byte for
byte as it was published:

```
POST /your/hook HTTP/1.1
Content-Type: application/json
X-Exchange-Event-Id: 01J8Z2K3M4N5P6Q7R8S9T0V1W7
X-Exchange-Event-Type: trade.executed
X-Exchange-Timestamp: 1757068523412
X-Exchange-Signature: v1=9f2c...  (64 hex characters)

{"event_id":"01J8Z2K3M4N5P6Q7R8S9T0V1W7","event_type":"trade.executed",...}
```

Any 2xx means you have it. Anything else — a 4xx, a 5xx, a timeout, a refused
connection — is a failure and will be retried.

Answer fast. The exchange gives up on one attempt after `WEBHOOK_TIMEOUT`
(10 seconds by default) and counts it as a failure, so do your work after you
have answered, not before.

**Redirects are never followed.** A 301 or 302 is a failed attempt. Register
the address you actually serve on.

## Verifying the signature

`X-Exchange-Signature` is `v1=` followed by the hex of

```
HMAC-SHA256(secret, timestamp + "." + body)
```

where `timestamp` is the exact string in `X-Exchange-Timestamp` (unix
milliseconds) and `body` is the raw request bytes. Sign the bytes you
received, not a re-serialisation of them — any JSON library that reorders keys
or changes whitespace will produce a different signature.

```python
import hashlib, hmac, time

def verify(secret: str, headers, body: bytes, tolerance_s: int = 300) -> bool:
    sig = headers["X-Exchange-Signature"]                     # "v1=<hex>"
    ts  = headers["X-Exchange-Timestamp"]                     # "1757068523412"
    if abs(time.time() * 1000 - int(ts)) > tolerance_s * 1000:
        return False                                          # replay window
    parts = dict(p.split("=", 1) for p in sig.split(","))
    want  = hmac.new(secret.encode(), f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(parts.get("v1", ""), want)
```

Compare in constant time, and reject a timestamp outside your tolerance —
without that check a signature stays valid forever and an attacker who
captured one delivery can send it back to you at any point.

The signature format is versioned in its own value (`v1=`) and the header can
carry several, so a future scheme can be added and both sent during a
migration. Read the one you know and ignore the rest.

`exchangectl webhook-sink --port 9999 --secret <secret>` is a receiver that
does all of this and prints what arrived, for trying an integration before
writing one. It uses the same verification code the exchange signs with.

## Retries

The retry schedule is six steps over roughly a day:

| After attempt | Next attempt in |
|---|---|
| 1 | 1 minute |
| 2 | 5 minutes |
| 3 | 30 minutes |
| 4 | 2 hours |
| 5 | 12 hours |
| 6 | 24 hours |

After the seventh failure the delivery is `dead` and is not tried again unless
an operator replays it. The schedule is `WEBHOOK_BACKOFF` and a deployment can
change it; the length of the list is the attempt budget.

Every attempt is recorded — status code, duration, error — and the operator
can see all of them. An attempt with no status code at all is one where you
never answered: a timeout, a refused connection, a name that does not resolve.

## Delivery is at-least-once

**You will sometimes receive the same event twice. Deduplicate on
`event_id`.**

This is not a caveat about a rare failure; it is the contract, and there are
two ordinary ways it happens.

The first is redelivery. Events reach the delivery path through JetStream,
which is itself at-least-once. While an event is still waiting to be sent to
you, receiving it a second time is a no-op. But once a delivery has finished,
the work item is gone — so a redelivery arriving after that queues a fresh one
and you are POSTed again, with the same `event_id`.

The second is replay: an operator can ask for an event to be sent to you
again, and usually will because you asked them to. It is a deliberate second
copy of something you may already have.

There is no exactly-once mode and there is not going to be one. A receiver
that is idempotent on `event_id` is correct under both; one that is not will
double-count a fill sooner or later.

Ordering is not guaranteed either. Order market events by `seq` and account
events by `account_seq` (`docs/events.md`), never by the order they arrive.

## Managing endpoints

Through the admin API, or `exchangectl admin webhooks`:

```sh
exchangectl admin webhooks create --url https://example.com/hooks --events 'trade.executed,withdrawal.state_changed'
exchangectl admin webhooks list
exchangectl admin webhooks deliveries <endpoint-id>
exchangectl admin webhooks replay <endpoint-id> <delivery-id>
exchangectl admin webhooks disable <endpoint-id> --reason 'customer paused the integration'
```

Three things about this are deliberate and worth knowing before you build
around them.

**The signing secret is shown exactly once**, in the response that creates the
endpoint. It is stored encrypted and no later call can produce it. An operator
who loses it has to create a new endpoint.

**Endpoints are disabled, never deleted.** The delivery history has to outlive
the integration — "we sent it, here is when, here is what you answered" is the
whole reason the records exist — so there is no delete, and the database does
not grant the admin role one. Disabling also drops whatever was still queued
for the endpoint: those deliveries are not owed any more, and keeping them
would fire a batch of stale events at you the day the integration came back.
Anything you still need after re-enabling can be replayed.

**Replay queues a run; it does not send anything itself.** The call returns
202 with the time the first attempt is due. Replaying restarts the schedule
from its first step, the original delivery records are untouched, and the new
attempts appear alongside them under a new run id. The payload is exactly what
was published — the admin role cannot write event bodies, only queue existing
ones, so a replay can never be a rewrite.

A replay is refused if the event is still queued for you (the schedule is
going to send it anyway) or if the endpoint is disabled (nothing would pick
the work up).

## Rotating the secret

```sh
exchangectl admin webhooks rotate-secret <endpoint-id> --grace-hours 24 --reason 'quarterly rotation'
```

`POST /admin/v1/webhooks/{id}/rotate-secret` issues a new secret and prints
it once, like the create did. Nothing has to happen at the same instant on
your side: for the grace period (24 hours by default, up to a week) every
delivery carries **two** signatures,

```
X-Exchange-Signature: v1=<with the new secret>,v1=<with the old secret>
```

and a receiver that verifies with either passes. That is why the header has
always been described as "one or more `v1=` values, any of which may match":
verify against each `v1=` you find and accept the delivery if any one of them
matches. A receiver that compares the whole header string, or only the first
value, breaks on the day of a rotation.

When the grace period ends the old secret stops signing and deliveries go
back to a single `v1=`. Rotating again inside the grace period replaces the
old secret rather than adding a third: the newest two are the only secrets
ever valid. The audit trail records who rotated and until when the old secret
signs, and never a secret.

# Public API conventions

Applies to `api/public/v1/openapi.yaml` (served under `/v1`, plus
`/.well-known/jwks.json`). The OpenAPI document is the contract; this page
explains the parts a client has to get right that a schema cannot express.

## Amounts

Every monetary value (`price`, `qty`, `quote_qty`, `fee`, balances) is a JSON
**string** holding a decimal with at most 18 integer and 18 fractional digits
(`^-?\d{1,18}(\.\d{1,18})?$`). JSON numbers are rejected with 400. Prices must
be an exact multiple of the market's `price_tick` and quantities of `qty_step`;
the engine rejects the order (see *Order status codes*) instead of rounding.

## Errors

Non-2xx responses are RFC 7807 problems (`Content-Type: application/problem+json`):

```json
{"type":"about:blank","title":"Unauthorized","status":401,
 "detail":"invalid or expired access token","instance":"/v1/balances",
 "correlation_id":"req_01J8Z2K2…"}
```

`correlation_id` echoes the request's `X-Request-Id` (generated when absent)
and appears in every server log line for that request; quote it in bug reports.

## Authentication

Two credentials are accepted; a request carries exactly one.

### Session (Bearer JWT)

`POST /v1/auth/register` or `POST /v1/auth/login` returns a `Session`:

```json
{"user_id":"…","account_id":"…","role":"user","token_type":"Bearer",
 "access_token":"<jwt>","expires_in":900,"expires_at":"…","refresh_token":"<opaque>"}
```

Send `Authorization: Bearer <access_token>`. Access tokens are Ed25519 (EdDSA)
JWTs, valid 15 minutes, verifiable offline against `GET /.well-known/jwks.json`
(`iss=exchange`, `aud=exchange`, claims `sub`, `account_id`, `tenant_id`,
`role`, `scopes`, `method`). Refresh tokens are opaque, single-use and valid
7 days: `POST /v1/auth/refresh` returns a new session and invalidates the token
you sent. **Presenting an already-rotated refresh token revokes every refresh
token of that user** (reuse is treated as theft); log in again. `POST
/v1/auth/logout` revokes a refresh token and is idempotent. Sessions hold all
scopes.

### API key (HMAC)

Create keys from a session with `POST /v1/api-keys` (`scopes` ⊆ `read`,
`trade`, `withdraw`; optional `ip_allowlist` of IPs or CIDRs). The response is
the only time the `secret` is shown. Sign every request with three headers:

| Header | Value |
|---|---|
| `X-API-KEY` | the key id (`ak_…`) |
| `X-API-TIMESTAMP` | Unix time in **milliseconds**; must be within ±30 s of server time |
| `X-API-SIGNATURE` | lowercase hex of `HMAC-SHA256(secret, canonical)` |

```
canonical = timestamp + "\n" + METHOD + "\n" + requestURI + "\n" + body
```

`METHOD` is upper-case; `requestURI` is the path **including the query
string**, exactly as sent (`/v1/orders?status=open&limit=5`); `body` is the
raw request body bytes (empty for GET/DELETE). Example in Go:

```go
ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
mac := hmac.New(sha256.New, []byte(secret))
mac.Write([]byte(ts + "\n" + "POST" + "\n" + "/v1/orders" + "\n" + string(body)))
sig := hex.EncodeToString(mac.Sum(nil))
```

`internal/auth.SignRequest` and `exchangectl --api-key/--api-secret` implement
the same string. Failures: 401 for an unknown or revoked key, a bad signature,
a stale or unparsable timestamp, or a body that differs from the signed one;
403 for a key used from an address outside its allowlist or for an endpoint
whose scope the key lacks. API keys cannot create or revoke API keys.
Signed bodies are limited to 1 MiB.

## Rate limits

Token buckets, one token per `window / n`. Defaults (configurable with
`RATELIMIT_*`):

| Scope | Limit | Key |
|---|---|---|
| `POST /v1/auth/login`, `POST /v1/auth/register` | 10 / minute | client IP |
| `POST /v1/auth/login` | 5 / minute | email (any outcome counts) |
| `POST /v1/orders`, `DELETE /v1/orders/{id}` | 20 / second | account |

Exceeding a limit returns 429 with `Retry-After: <seconds>`. Five failed
logins in a minute lock that email until a token refills, even for the right
password.

## Idempotency and order status codes

`client_order_id` is required on `POST /v1/orders` and unique per account.

| Situation | Status | Body |
|---|---|---|
| New order accepted, filled, or **rejected by the engine** (`status: rejected`, `reject_reason` set) | 201 | `OrderResult` |
| Same `client_order_id` with the same parameters (replay) | 200 | the original `OrderResult` |
| Same `client_order_id` with different parameters | 422 | problem |
| Malformed request (bad enum, JSON number for an amount, missing field) | 400 | problem |
| Unknown market | 404 | problem |
| Engine not reachable from this role | 503 | problem; retry with the same `client_order_id` |

A rejection is an order, not an error: it is persisted with its reason and
occupies the `client_order_id`. `DELETE /v1/orders/{id}` on a terminal order
returns 200 with the current state (idempotent). Resources of other accounts
are indistinguishable from missing ones (404, or an empty list).

## Reads

`GET /v1/orders`, `/v1/fills`, `/v1/ledger/entries`, `/v1/markets/{symbol}/trades`
page with `limit` (default 100, max 1000) and `offset`, newest first.
`GET /v1/ledger/entries` returns only the caller's postings of each entry.
`GET /v1/markets/{symbol}/depth` returns aggregated levels and `last_seq`; a
`limit` outside 1..200 falls back to the default of 20.

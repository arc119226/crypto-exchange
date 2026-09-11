# Limitations

This is the list of things this software does not do, does not protect against, or
does not do well. It exists because the rest of the documentation is written in
Traditional Chinese, and an English-speaking reader who clones this repository can
run it end to end without ever meeting a single one of these sentences.

Nothing here is a surprise to the project. Every item below is a decision recorded
somewhere in `docs/`, and the Chinese source for most of it is `docs/plan-v1.0.md`
section 18. This page is the English version, corrected where the project has moved
on since that section was written.

## The four that matter most

**There has never been a security audit or a penetration test.** Not a light one,
not an internal one. No third party has looked at the signing path, the withdrawal
policy, the admin login, or the public API with an attacker's intent.

**There is no compliance capability of any kind.** No KYC, no AML, no
sanctions-list screening, no Travel Rule reporting. `auth.users.kyc_level` is a
column an operator can write; nothing verifies an identity, and nothing consults a
list before money leaves. Licensing, screening and reporting belong to whoever
operates a deployment.

**The signer is an encrypted file and a passphrase.** `KeystoreSigner` holds the
hot-wallet key in an encrypted keystore on disk, unlocked with a passphrase from
the environment. That is appropriate for test assets and a closed beta and for
nothing else. A production deployment needs KMS, an HSM, or MPC behind the `Signer`
interface. This build ships a `KMSSigner` that is a compiled stub and refuses every
request, and configuration refuses to start if you select it -- deliberately, so the
gap is a startup failure rather than a runtime surprise.

**It is not built to touch real money, and part of that is enforced.** Set
`ETH_CHAIN_ID` to a chain id the binary recognises as a mainnet and it refuses to
start unless three conditions hold at once, one of which cannot be satisfied in this
build. But the list of recognised mainnets can never be complete -- anyone can stand
up an EVM chain and choose an id -- so pointing this at an unlisted mainnet is not
blocked by code. See `internal/app/config.go`.

## Availability and scale

- **The matching engine is a single instance with no hot standby.** One process
  holds a Postgres advisory lock; scale within it is one goroutine per market, not
  more processes. While it restarts, that market cannot accept orders. The recovery
  target is under 30 seconds. This is a design choice, not a defect -- but it is not
  high availability either.
- **The admin role assumes exactly one replica.** Login throttling is an in-process
  bucket, and the one-time reveal of a newly created webhook signing secret rides
  across a redirect in a per-process map. Running more than one replica silently
  multiplies the throttle allowance and can lose a signing secret permanently. TOTP
  lockout is per-user in the database and is not affected.
- **One deployment serves one chain.** The configuration is singular (`ETH_*`), and
  reconciliation sums house balances across all chains, which is only correct while
  there is one. Adding a second chain does not break loudly; it produces a
  plausible-looking wrong number until the custody accounts are split per chain.
- **Single tenant.** Every table and every event subject carries `tenant_id`, but
  there is no isolation logic and no tenant-level permissions behind it.
- **One Postgres instance, no point-in-time recovery.** The beta posture is a daily
  backup plus WAL archiving, with a rehearsed restore. PITR is documented, not built.

## Security posture

- **Internal services trust each other.** There is no mTLS and no service identity
  between roles. What stands between the outside and the internals is the single
  entry point (`api`, `stream`, `admin`) and per-role database privileges.
- **`X-Forwarded-For` is deliberately not trusted.** The client address is taken
  from the socket. Behind a reverse proxy -- which is how the beta deploys -- every
  per-IP rate limit collapses into one shared bucket and every audited IP is the
  proxy's. Per-account login throttling is unaffected and is the control that still
  works. See `docs/runbooks/beta-deploy.md`.
- **No secrets have ever been committed**, and `gitleaks` runs over the full history
  on every pull request. That is a claim you can check rather than trust.

## Performance

The targets in `docs/plan-v1.0.md` section 3.3 are stated there as design targets,
not promises. Four of them are not met, and they are four faces of one tradeoff:
Phase 7 introduced group commit, which bought throughput by spending latency.

| Target | Measured |
|---|---|
| `POST /v1/orders` p99 under 50 ms | 548 ms at 100/s steady, 1,127 ms at saturation |
| One market at 1,000 orders/s or better | 266.5 orders/s |
| Private WebSocket push p99 under 200 ms | 648 ms at saturation |
| Public depth delta p99 under 300 ms | 638 ms at saturation |

`ENGINE_BATCH_SIZE` is the knob. All four targets hold at low load and all four fail
at saturation. The numbers were measured on a single machine under Docker Compose;
nothing has been measured on Kubernetes or over a cloud network.

## Not supported at all

Deposits made by an internal transfer inside another contract. Non-EVM chains. Token
blacklists. Tax reporting. External liquidity or market-making integration -- there
is no code for it. Promotion, referral or rebate campaigns -- likewise none.

## Expected to be rebuilt before production

Key management. Network isolation and service identity. A multi-replica engine with
leader election. Database separation and point-in-time recovery. A chain node, self-run
or from a paid provider. Alert delivery channels. An event schema registry.

## No warranty

This software is provided as-is, with no warranty of any kind, express or implied.
The authors accept no liability for any loss arising from its use. Operating an
exchange is a regulated activity in most jurisdictions; obtaining a licence or an
exemption, and meeting the obligations that come with it, is the operator's
responsibility and not this project's.

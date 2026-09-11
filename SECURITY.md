# Security policy

## Status of this project

This is pre-1.0 software that has never had a security audit or a penetration test.
It is built to run against a local test chain or a testnet and is not intended to
hold real customer funds. `docs/limitations.md` is the full list of what it does not
do; please read it before reporting, because several things that look like findings
are documented decisions.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting on this repository: the **Security**
tab, then **Report a vulnerability**. That opens a private advisory visible only to
the maintainers, which is the right channel for anything that would let someone move
money they do not own, read another account's data, or bypass authentication.

Please do not open a public issue for those. A public issue is the right place for
everything else, including hardening suggestions and anything already listed in
`docs/limitations.md`.

What helps, in rough order of usefulness: the commit or tag you looked at; a
reproduction against the local stack from the quickstart in `README.en.md`; which
role is affected (`api`, `engine`, `chain`, `signer`, `stream`, `admin`, `worker`);
and what an attacker gets out of it.

This project is maintained by one person in their own time. Expect an acknowledgement
within a week. There is no bounty.

## Threat model

Nothing else in this repository states the boundary, so it is stated here. These are
deliberate positions, not oversights. A report that one of them is true is a report
that the design is what it says it is.

**Inside the deployment is a trusted network.** The seven roles authenticate nothing
to each other. There is no mTLS and no service identity. Anything that can reach the
NATS subjects can issue engine commands; anything that can reach Postgres has
whatever its role's grants allow. What separates the outside from the inside is the
single public entry point and per-role database privileges -- `api` cannot update a
withdrawal row, for instance, because the grant does not exist, not because code
checks.

**The signing key is a file.** `KeystoreSigner` decrypts a keystore with a passphrase
supplied through the environment. Anyone with the file and the passphrase can sign.
The mitigation is deployment shape, not cryptography: the `signer` role has no
inbound HTTP listener and accepts only signing intents over the internal bus, and it
constructs the transaction itself rather than signing bytes another service composed.
An intent can be signed exactly once, enforced by a database unique key.

**The back office is expected to be unreachable from the internet.** It binds to
`127.0.0.1` and the documented access path is an SSH tunnel. It has no rate limiting
that would survive being exposed, and its login throttle is in-process and therefore
per-replica. Putting it behind a public ingress is outside the threat model.

**Client addresses are not trusted.** `X-Forwarded-For` is ignored on purpose, so
behind a reverse proxy every per-IP control collapses to one bucket. Per-account
throttling is the control that still works. An attacker who can exhaust the shared
bucket can deny logins to everyone; that is a known consequence, described in
`docs/runbooks/beta-deploy.md`.

**The chain is assumed honest up to the configured confirmation depth.** Reorgs
deeper than `ETH_CONFIRMATIONS` are handled as a reversal path, but a chain-level
attack is not something this software defends against.

**Operators are trusted.** An administrator with a session and a TOTP code can adjust
the ledger and approve withdrawals. Every such action is written to an audit log in
the same transaction as the change, so it is attributable after the fact -- but it is
not prevented, and there is no four-eyes requirement in this version.

## Out of scope

Findings that amount to one of the documented limitations, denial of service by
volume against a local development stack, missing hardening headers on the back
office given the binding above, and anything requiring an attacker who already has
the contents of `secrets/`.

# Contributing

Thanks for looking. This is a spot-exchange engine built by one person, and it
is open because more people reading it makes it better, not because there is a
roadmap anyone is obliged to follow. Small, well-argued changes are very
welcome. So are issues that say "this is wrong and here is why".

## Read this first

**`docs/limitations.md`.** It is short and it will save you filing an issue for
something that is a decision rather than a bug. The headline: this software has
never had a security audit or a penetration test, it has no KYC/AML or
sanctions screening, and it is built for a local test chain or a testnet. Do
not run it with real customer funds.

**The design documents are in Traditional Chinese.** The code, its comments,
the API contracts (`docs/api-conventions.md`, `docs/events.md`,
`docs/webhooks.md`, `docs/ws-api.md`), this file, `SECURITY.md` and
`docs/limitations.md` are English. `docs/plan-v1.0.md` and `docs/domain.md` --
where most of the reasoning lives -- are not. That is a real barrier and we are
not pretending otherwise; if a decision matters to your change and you cannot
read it, open an issue and ask.

## Getting it running

The two READMEs at the root are the tutorial, and they assume no programming
experience: [`README.md`](README.md) in Traditional Chinese,
[`README.en.md`](README.en.md) in English. Three commands and a browser.

To *develop* rather than run, you need more than the tutorial does:

| Tool | Version | Why |
|---|---|---|
| Go | 1.26 (see the `go` directive in `go.mod`) | everything |
| Docker | any recent | the integration and end-to-end suites start real Postgres, NATS and anvil |
| A C compiler | `build-essential` or equivalent | `make test` runs `-race`, which needs cgo |
| Node | 22 | the trading front end and its Playwright smoke |
| foundry | `FOUNDRY_TAG` in `.env.example` | the Solidity contracts; `make contracts-test` runs it in a container instead, if you would rather not install it |
| helm | `HELM_VERSION` in `.github/workflows/ci.yml` | optional, but see the note under `make check` |

Every pinned version is single-sourced; `make tools` prints the rest.

## Before you open a pull request

```sh
make check
```

One command. It runs lint (including gitleaks over the whole history), the
module tidiness assertion, shell syntax, the generated-code sync check, the
documentation tests, unit and property tests with the race detector, the
coverage gate, compose rendering, the front-end checks and the contracts.

Two things it does **not** do, on purpose:

- It skips the Docker-backed suites. `make test-integration` and `make e2e`
  are separate and take around twelve minutes between them. CI runs both.
- `deploy/helm/helm_test.go` skips the chart lint and the kubeconform render
  when `helm` is not on your `PATH`, while CI fails on them. So a green
  `make check` can still meet a red `checks` job. Install helm, or set `HELM`,
  to close that gap.

`make` on its own lists every target.

## What CI runs

Three workflows. A pull request that touches code runs five jobs from `ci.yml`
plus `dco`; a pull request that touches only markdown skips `ci.yml` entirely
(it carries `paths-ignore`) and runs `docs.yml` plus `dco` instead.

| Job | What it proves |
|---|---|
| `checks` | lint, secrets, generated code, unit and property tests, coverage, both binaries start, front end builds, contracts compile and pass |
| `integration` | real Postgres and NATS in containers: migrations, ledger, admin API, matching, the outbox relay |
| `e2e` | the split deployment -- api, engine and admin in separate containers talking over NATS -- from an on-chain deposit through to a withdrawal, plus `kill -9` on the engine and a backup restore drill |
| `helm` | installs the chart into a real kind cluster and runs an in-cluster end-to-end, then deletes the engine pod and checks the order book comes back identical |
| `image` | the three images build and the binaries inside them actually run |
| `fuzz-smoke` | `FuzzApply` against the matching engine; push only |
| `release` | tag only |
| `dco` | every commit in the pull request carries a `Signed-off-by` line (own workflow, always on GitHub's runners) |
| `docs` | `make docs-test` on markdown-only pull requests, which `ci.yml` declines |

## Commits

The history uses conventional-commit prefixes -- `feat:`, `fix:`, `docs:`,
`ci:`, `test:`, `chore:` -- with an optional scope, e.g. `fix(config):`.

The subject line says **what was wrong**, not what you did. Compare:

```
fix(config): the known-mainnet list was five ids, so BNB and Avalanche started
```

against "update chain id list". The first tells a reader six months from now
why the commit exists. There is no hard length limit; the median in this
repository is 68 characters.

The body is for the reasoning: what you measured, what you ruled out, what you
decided not to do. Long bodies are normal here and are not a sign you have done
something wrong.

Branch names look like `fix/short-description` or `feat/short-description`.

## Sign your commits off (DCO)

This project uses the [Developer Certificate of
Origin](https://developercertificate.org/). There is no CLA to sign and nothing
to email. You keep the copyright in what you write; the sign-off is you
stating that you have the right to contribute it under Apache-2.0.

Add `-s` and git appends the line for you:

```sh
git commit -s -m "fix(ledger): ..."
```

which produces:

```
Signed-off-by: Your Name <your.email@example.com>
```

A CI job checks every commit in the pull request has one. If you forget:

```sh
git commit -s --amend            # the last commit
git rebase --signoff origin/main # every commit on the branch
```

Add yourself to `AUTHORS` in the same pull request if you like.

**One consequence worth knowing**: because contributors keep their copyright
and there is no CLA, this project cannot change its licence later. Apache-2.0
is permanent. That was the deliberate trade (see
`docs/adr/0014-open-source-under-apache-2.md`).

## What gets merged

Anything that makes the thing more correct, more honest, or easier to pick up.
Bug fixes with a failing test first are the easiest thing to say yes to.

Things that will get a conversation rather than a merge: new external
dependencies (the linter enforces module boundaries -- see the `depguard`
rules in `.golangci.yml`, and note `internal/matching` and `internal/money` are
deliberately locked to the standard library plus one decimal package); anything
that puts floating point on a money path; anything that widens what the project
claims it does. Generated code is never hand-edited -- run `make gen`.

The maintainer works on this in spare time. A slow reply is not a verdict.

## Security

Do not open a public issue for anything that would let someone move money they
do not own, read another account's data, or bypass authentication. See
[`SECURITY.md`](SECURITY.md), which also states the threat model -- several
things that look like findings are documented positions.

## Code of conduct

[`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

# deploy/seed-params

Optional overlays for `exchange seed --params <file>`. Absent keys keep the
built-in values in `internal/registry/seed.go`, so a file says only what this
chain does differently. An unknown asset symbol or JSON key is rejected rather
than ignored — a typo that silently changes nothing is the failure this format
exists to avoid.

- **`anvil.json`** restates the built-in values exactly. It changes nothing and
  is not used by compose; it is here so the defaults can be read in one place,
  and so `sepolia.json` can be diffed against something.
- **`sepolia.json`** is the same exchange sized for a chain funded by faucets.

## Why Sepolia needs different numbers

The built-in values assume anvil, where an account starts with 10,000 ether.
A Sepolia faucet gives 0.05 a day. With the anvil numbers on Sepolia:

| | anvil | what it means on Sepolia |
|---|---|---|
| `ETH.sweepThreshold` 0.05 | a 1 ETH dev deposit is swept | a whole day's faucet lands exactly on the line, so nothing is collected |
| `ETH.minWithdrawal` 0.01 | keeps dust withdrawals out | a fifth of a faucet claim, so there is nothing left to test the flow twice |
| `ETH` level-0 auto-approve 0.1 | above it, review; below it, automatic | every realistic amount is below it, so the manual-review branch is never exercised |

`sepolia.json` moves all three down by roughly the ratio of the funding, which
keeps both branches of the withdrawal policy reachable: 0.005 auto-approves and
0.01 queues for a person. USDC is left alone — `MockUSDC.mint` is unrestricted,
so token supply is never the constraint.

The gas figures behind the sweep threshold: at Sepolia's ~1 gwei base fee a
21,000-gas transfer costs about 0.00005 ETH, so a 0.01 threshold still recovers
roughly 200× what it spends.

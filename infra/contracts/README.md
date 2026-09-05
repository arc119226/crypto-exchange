# infra/contracts

Foundry project for the local test chain (docs/plan-v1.0.md §11, §12 Phase 0).

- `src/MockUSDC.sol` — 6-decimal ERC-20 with an unrestricted `mint`. Test chains only.
- `script/Deploy.s.sol` — idempotent: deploys MockUSDC from anvil account #0 at nonce 0
  (address `0x5FbDB2315678afecb367f032d93F642f64180aa3`), funds `HOT_WALLET_ADDRESS`
  with 100 ETH + 1,000,000 USDC, writes `/artifacts/addresses.json` for `exchange seed`.
- `script/Vm.sol` — the handful of Foundry cheatcodes we use, declared locally so the
  project builds from a plain `git clone` without a forge-std submodule. Add a
  submodule (`forge install foundry-rs/forge-std`) if the tests outgrow it.

Run without installing Foundry (uses the pinned image from `.env.example`):

```sh
make contracts-test          # forge test -vv inside ghcr.io/foundry-rs/foundry:$FOUNDRY_TAG
make up-single               # compose runs contracts-deployer against anvil
make artifacts               # copy addresses.json out of the artifacts volume
```

`forge build` downloads solc 0.8.28 on first use; compose keeps that cache in the
`svm-cache` volume. The deployer copies the project to `/tmp/work`, so `out/`,
`cache/` and `broadcast/` never appear in the host checkout.

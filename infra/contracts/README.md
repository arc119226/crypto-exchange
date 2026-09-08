# infra/contracts

Foundry project for the local test chain (docs/plan-v1.0.md §11, §12 Phase 0).

- `src/MockUSDC.sol` — 6-decimal ERC-20 with an unrestricted `mint`. Test chains only.
- `script/Deploy.s.sol` — idempotent: deploys MockUSDC from `CONTRACT_DEPLOYER_KEY`
  at nonce 0 (anvil account #0 gives `0x5FbDB2315678afecb367f032d93F642f64180aa3`),
  funds `HOT_WALLET_ADDRESS` with 100 ETH + 1,000,000 USDC, writes
  `/artifacts/addresses.json` for `exchange seed`.

  Four optional variables make the same script work on a public testnet, where
  none of those defaults hold — the deployer's ether comes from a faucet in
  hundredths, and a key that has ever sent a transaction cannot satisfy the
  nonce-0 check:

  | variable | default | set it when |
  |---|---|---|
  | `USDC_ADDRESS` | *(deploy at nonce 0)* | the contract already exists, or the deployer had a nonce before it |
  | `HOT_WALLET_ETH_TARGET` | `100 ether` | `0` on a testnet — point a faucet at the hot wallet instead |
  | `HOT_WALLET_USDC_TARGET` | `1_000_000e6` | rarely; `mint` is unrestricted, so supply is never the constraint |
  | `ADDRESSES_OUT` | `/artifacts/addresses.json` | running by hand, outside the compose volume |

  See `docs/guides/sepolia.md`.
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

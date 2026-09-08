# deploy/compose/sepolia

Two operator-provided files, mounted at `/config` in the `seed` job by
`compose.sepolia.yaml`. Both are gitignored: they name a specific deployment,
not the project. Copy the `.example` files and fill them in — where the values
come from is `docs/guides/sepolia.md` §A5.

- **`sepolia-addresses.json`** — the same shape `exchange seed --fixtures`
  always takes (`internal/registry.Fixtures`). On anvil the contract deployer
  writes it; here you write it, from the address and block your own
  `forge create` reported.
- **`seed-params.json`** — the threshold overlay. Start from
  `deploy/seed-params/sepolia.json`; its README explains why the anvil numbers
  do not survive faucet-sized funding.

Neither file holds a secret — addresses and amounts are public the moment they
are on chain. They are kept out of git because a committed one would be applied
to somebody else's deployment by accident.

package main

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/app"
)

func newSeedCmd() *cobra.Command {
	var fixtures, tenant, params string
	var chainID int64
	var confirmations int32
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Upsert registry fixtures (assets, markets, fee schedule) from a contract addresses file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dsn, err := dsnFromEnv()
			if err != nil {
				return runtimeErr(err)
			}
			return runtimeErr(app.Seed(cmd.Context(), app.SeedOptions{
				DSN: dsn, FixturesPath: fixtures, TenantID: tenant, ChainID: chainID,
				RequiredConfirmations: confirmations, ParamsPath: params,
			}, cmd.OutOrStdout()))
		},
	}
	defaultChain := int64(31337)
	if v, err := strconv.ParseInt(os.Getenv("ETH_CHAIN_ID"), 10, 64); err == nil {
		defaultChain = v
	}
	defaultTenant := os.Getenv("TENANT_ID")
	if defaultTenant == "" {
		defaultTenant = "default"
	}
	// The registry row wins over ETH_REQUIRED_CONFIRMATIONS_DEFAULT at run
	// time -- that setting is only the fallback for an asset whose row says 0
	// -- so a seed that ignores the environment quietly pins every asset to
	// one confirmation. Reading the same variable here is what makes
	// "confirmations 6" a single decision instead of two that can disagree.
	defaultConfirmations := int32(1)
	if v, err := strconv.ParseInt(os.Getenv("ETH_REQUIRED_CONFIRMATIONS_DEFAULT"), 10, 32); err == nil && v > 0 {
		defaultConfirmations = int32(v)
	}
	cmd.Flags().StringVar(&fixtures, "fixtures", "", "path to addresses.json written by the contract deployer (required)")
	cmd.Flags().StringVar(&tenant, "tenant", defaultTenant, "tenant id (env TENANT_ID)")
	cmd.Flags().Int64Var(&chainID, "chain-id", defaultChain, "expected chain id (env ETH_CHAIN_ID)")
	cmd.Flags().StringVar(&params, "params", os.Getenv("SEED_PARAMS"),
		"optional JSON overlay of seed thresholds, market increments and withdrawal limits (env SEED_PARAMS)")
	cmd.Flags().Int32Var(&confirmations, "confirmations", defaultConfirmations,
		"required confirmations for seeded assets (env ETH_REQUIRED_CONFIRMATIONS_DEFAULT; anvil 1, Sepolia 6+)")
	_ = cmd.MarkFlagRequired("fixtures")
	return cmd
}

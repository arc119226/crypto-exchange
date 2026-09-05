package main

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/app"
)

func newSeedCmd() *cobra.Command {
	var fixtures, tenant string
	var chainID int64
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Upsert registry fixtures (assets, markets, fee schedule) from a contract addresses file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dsn, err := dsnFromEnv()
			if err != nil {
				return runtimeErr(err)
			}
			return runtimeErr(app.Seed(cmd.Context(), app.SeedOptions{DSN: dsn, FixturesPath: fixtures, TenantID: tenant, ChainID: chainID}))
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
	cmd.Flags().StringVar(&fixtures, "fixtures", "", "path to addresses.json written by the contract deployer (required)")
	cmd.Flags().StringVar(&tenant, "tenant", defaultTenant, "tenant id (env TENANT_ID)")
	cmd.Flags().Int64Var(&chainID, "chain-id", defaultChain, "expected chain id (env ETH_CHAIN_ID)")
	_ = cmd.MarkFlagRequired("fixtures")
	return cmd
}

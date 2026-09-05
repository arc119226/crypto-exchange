// Command exchangectl is the operator/developer CLI: demo flows, E2E checks,
// replay and load generation against a running exchange. It talks to the
// public API only, through the generated client in internal/apiclient, so it
// doubles as a living example of how customers integrate.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "exchangectl",
		Short:         "Operator and developer CLI for the exchange engine",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("base-url", envOr("EXCHANGE_API_URL", "http://localhost:8080"), "public API base URL")
	root.PersistentFlags().String("output", "table", "output format: table|json")
	root.AddCommand(newMarketsCmd(), newAssetsCmd())
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "exchangectl %s (commit %s, built %s, %s)\n", version, commit, date, runtime.Version())
			return err
		},
	})
	return root
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

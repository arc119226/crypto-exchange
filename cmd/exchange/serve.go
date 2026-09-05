package main

import (
	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/app"
)

func newServeCmd() *cobra.Command {
	var roleFlag string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run one or more roles (api, engine, chain, signer, stream, admin, worker, all)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			roles, err := app.ParseRoles(roleFlag)
			if err != nil {
				return err // usage error → exit 2
			}
			cfg, err := app.LoadConfig()
			if err != nil {
				return runtimeErr(err)
			}
			return runtimeErr(app.Run(cmd.Context(), cfg, roles, buildInfo()))
		},
	}
	cmd.Flags().StringVar(&roleFlag, "role", "all", "comma-separated roles to run, or \"all\"")
	return cmd
}

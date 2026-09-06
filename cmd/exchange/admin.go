package main

import (
	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/app"
)

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Administrative operations"}
	cmd.AddCommand(&cobra.Command{
		Use:   "bootstrap",
		Short: "Create the first admin user from ADMIN_BOOTSTRAP_EMAIL / ADMIN_BOOTSTRAP_PASSWORD (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := app.LoadConfig()
			if err != nil {
				return runtimeErr(err)
			}
			return runtimeErr(app.BootstrapAdmin(cmd.Context(), cfg, cmd.OutOrStdout()))
		},
	})
	return cmd
}

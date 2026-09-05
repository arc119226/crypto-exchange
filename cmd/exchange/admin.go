package main

import (
	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/app"
)

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Administrative operations"}
	cmd.AddCommand(&cobra.Command{
		Use:   "bootstrap",
		Short: "Create the first admin user from ADMIN_BOOTSTRAP_* (Phase 3)",
		RunE: func(*cobra.Command, []string) error {
			return runtimeErr(app.ErrNotImplemented)
		},
	})
	return cmd
}

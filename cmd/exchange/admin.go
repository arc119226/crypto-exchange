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
	totp := &cobra.Command{Use: "totp", Short: "Administrator TOTP"}
	var qrPath string
	enroll := &cobra.Command{
		Use:   "enroll --email <admin email>",
		Short: "Issue a TOTP secret for an administrator and show it once",
		Long: "The browser cannot do this on purpose: it would let anyone with the password of\n" +
			"an administrator who has not enrolled yet finish enrolling as them. This runs with\n" +
			"database credentials instead, prints the secret once, and signs out every session\n" +
			"of that administrator. Sign in at /admin/login and enter the first code to finish.\n\n" +
			"Run it again to replace a lost or compromised secret.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			email, err := cmd.Flags().GetString("email")
			if err != nil {
				return err
			}
			cfg, err := app.LoadConfig()
			if err != nil {
				return runtimeErr(err)
			}
			return runtimeErr(app.EnrollTOTP(cmd.Context(), cfg, email, qrPath, cmd.OutOrStdout()))
		},
	}
	enroll.Flags().String("email", "", "the administrator's email")
	enroll.Flags().StringVar(&qrPath, "qr-out", "", "also write the QR code as a PNG to this path")
	_ = enroll.MarkFlagRequired("email")
	totp.AddCommand(enroll)
	cmd.AddCommand(totp)
	return cmd
}

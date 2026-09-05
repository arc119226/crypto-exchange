package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/app"
)

func newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "keys", Short: "Key management helpers"}

	var out string
	var force bool
	genJWT := &cobra.Command{
		Use:   "gen-jwt",
		Short: "Generate an Ed25519 JWT signing key (PKCS#8 PEM, mode 0600)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.GenerateJWTKey(out, force); err != nil {
				return runtimeErr(err)
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "wrote", out)
			return err
		},
	}
	genJWT.Flags().StringVar(&out, "out", "secrets/jwt/ed25519.pem", "output path")
	genJWT.Flags().BoolVar(&force, "force", false, "overwrite an existing key")

	importMnemonic := &cobra.Command{
		Use:   "import-mnemonic",
		Short: "Encrypt a BIP-39 mnemonic into the HD seed keystore (Phase 4a)",
		RunE: func(*cobra.Command, []string) error {
			return runtimeErr(app.ErrNotImplemented)
		},
	}
	cmd.AddCommand(genJWT, importMnemonic)
	return cmd
}

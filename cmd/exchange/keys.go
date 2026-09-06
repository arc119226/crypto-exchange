package main

import (
	"fmt"
	"os"

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

	var importOpts app.ImportMnemonicOptions
	importMnemonic := &cobra.Command{
		Use:   "import-mnemonic",
		Short: "Encrypt a BIP-39 mnemonic into the HD seed keystore",
		Long: "Reads a BIP-39 mnemonic and writes secrets/keystore/hd-seed.json,\n" +
			"encrypted with scrypt + AES-256-GCM under WALLET_KEYSTORE_PASSPHRASE.\n" +
			"Prints the hot wallet address (m/44'/60'/1'/0/0) so it can be checked\n" +
			"against HOT_WALLET_ADDRESS.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if importOpts.Passphrase == "" {
				importOpts.Passphrase = os.Getenv("WALLET_KEYSTORE_PASSPHRASE")
			}
			path, hot, err := app.ImportMnemonic(importOpts)
			if err != nil {
				return runtimeErr(err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\nhot wallet: %s\n", path, hot)
			return err
		},
	}
	importMnemonic.Flags().StringVar(&importOpts.From, "from", "secrets/dev-mnemonic.txt", "file holding the BIP-39 mnemonic")
	importMnemonic.Flags().StringVar(&importOpts.KeystoreDir, "keystore-dir", "secrets/keystore", "directory to write hd-seed.json into")
	importMnemonic.Flags().BoolVar(&importOpts.Force, "force", false, "replace an existing seed (orphans every address already handed out)")
	cmd.AddCommand(genJWT, importMnemonic)
	return cmd
}

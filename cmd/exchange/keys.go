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
				if err := app.ExpandSecretEnv(); err != nil {
					return runtimeErr(err)
				}
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

	var pubIn, pubOut string
	jwtPublic := &cobra.Command{
		Use:   "jwt-public",
		Short: "Print the public half of a JWT signing key as PEM, and its kid",
		Long: "Reads the PKCS#8 key gen-jwt wrote and writes a SPKI PUBLIC KEY PEM, the form\n" +
			"JWT_PREVIOUS_KEY_FILE also accepts. The kid (RFC 7638 thumbprint) goes to\n" +
			"stderr so it can be matched against /.well-known/jwks.json during a rotation\n" +
			"(docs/runbooks/key-rotation.md).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pemBytes, kid, err := app.JWTPublicKey(pubIn)
			if err != nil {
				return runtimeErr(err)
			}
			if pubOut != "" {
				if err := os.WriteFile(pubOut, pemBytes, 0o644); err != nil { //nolint:gosec // a public key
					return runtimeErr(err)
				}
			} else if _, err := cmd.OutOrStdout().Write(pemBytes); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "kid:", kid)
			return err
		},
	}
	jwtPublic.Flags().StringVar(&pubIn, "in", "secrets/jwt/ed25519.pem", "private (or public) key PEM to read")
	jwtPublic.Flags().StringVar(&pubOut, "out", "", "write the public key here instead of stdout")

	var rekeyOpts app.RekeyOptions
	rekey := &cobra.Command{
		Use:   "rekey",
		Short: "Re-encrypt the HD seed keystore under a new passphrase",
		Long: "Opens hd-seed.json with WALLET_KEYSTORE_PASSPHRASE, writes it again under\n" +
			"WALLET_KEYSTORE_NEW_PASSPHRASE (both accept a _FILE variant), checks the new\n" +
			"file opens, and only then replaces the old one. The seed and every address\n" +
			"derived from it are unchanged; the hot wallet address is printed to prove it.\n" +
			"Stop the signer first (docs/runbooks/key-rotation.md).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := app.ExpandSecretEnv(); err != nil {
				return runtimeErr(err)
			}
			rekeyOpts.Passphrase = os.Getenv("WALLET_KEYSTORE_PASSPHRASE")
			rekeyOpts.NewPassphrase = os.Getenv("WALLET_KEYSTORE_NEW_PASSPHRASE")
			hot, err := app.RekeyKeystore(rekeyOpts)
			if err != nil {
				return runtimeErr(err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "rekeyed %s\nhot wallet: %s\n", rekeyOpts.KeystoreDir, hot)
			return err
		},
	}
	rekey.Flags().StringVar(&rekeyOpts.KeystoreDir, "keystore-dir", "secrets/keystore", "directory holding hd-seed.json")

	var domain string
	rewrap := &cobra.Command{
		Use:   "rewrap --domain <api-keys|webhook|totp>",
		Short: "Re-seal stored secrets under the current master key of a domain",
		Long: "After a master key rotation (API_KEY_MASTER_KEY, WEBHOOK_SIGNING_KEY or\n" +
			"ADMIN_TOTP_KEY, each with its _PREVIOUS set to the key it replaced) every row\n" +
			"sealed under the old key is rewritten under the new one, in one transaction,\n" +
			"with an audit event. Rows already under the current key are left alone, so it\n" +
			"is safe to run again. Point DATABASE_URL at ex_migrate; the app roles cannot\n" +
			"update these columns.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := app.ParseRewrapDomain(domain)
			if err != nil {
				return err // a usage error: exit 2, like an unknown flag
			}
			cfg, err := app.LoadConfig()
			if err != nil {
				return runtimeErr(err)
			}
			opts, err := app.RewrapOptionsFor(cfg, d)
			if err != nil {
				return runtimeErr(err)
			}
			_, err = app.Rewrap(cmd.Context(), opts, cmd.OutOrStdout())
			return runtimeErr(err)
		},
	}
	rewrap.Flags().StringVar(&domain, "domain", "", "which secrets: api-keys, webhook or totp")
	_ = rewrap.MarkFlagRequired("domain")

	cmd.AddCommand(genJWT, importMnemonic, jwtPublic, rekey, rewrap)
	return cmd
}

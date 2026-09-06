package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/app"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
)

// testVector is the standard BIP-39 mnemonic. Anvil's is refused on purpose,
// so it cannot be used here.
const testVector = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

func writeMnemonic(t *testing.T, dir, words string) string {
	t.Helper()
	path := filepath.Join(dir, "mnemonic.txt")
	require.NoError(t, os.WriteFile(path, []byte(words+"\n"), 0o600))
	return path
}

func TestImportMnemonic(t *testing.T) {
	dir := t.TempDir()
	keystore := filepath.Join(dir, "keystore")
	opts := app.ImportMnemonicOptions{
		From: writeMnemonic(t, dir, testVector), KeystoreDir: keystore,
		Passphrase: "unit-passphrase", Scrypt: hdwallet.TestScrypt(),
	}

	path, hot, err := app.ImportMnemonic(opts)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(keystore, hdwallet.SeedFileName), path)
	assert.Regexp(t, `^0x[0-9a-fA-F]{40}$`, hot, "the hot wallet address is printed so it can be compared with HOT_WALLET_ADDRESS")

	t.Run("the file does not contain the words", func(t *testing.T) {
		blob, err := os.ReadFile(path) //nolint:gosec // test path
		require.NoError(t, err)
		assert.NotContains(t, string(blob), "abandon")
	})

	t.Run("the seed round-trips to the same hot wallet", func(t *testing.T) {
		w, err := hdwallet.Load(keystore, opts.Passphrase)
		require.NoError(t, err)
		defer w.Close()
		addr, err := w.Address(hdwallet.HotWalletPath())
		require.NoError(t, err)
		assert.Equal(t, hot, addr.Hex())
	})

	t.Run("replacing a seed needs --force", func(t *testing.T) {
		_, _, err := app.ImportMnemonic(opts)
		require.ErrorContains(t, err, "already exists")

		forced := opts
		forced.Force = true
		_, hot2, err := app.ImportMnemonic(forced)
		require.NoError(t, err)
		assert.Equal(t, hot, hot2, "the same mnemonic derives the same hot wallet")
	})

	t.Run("a typo'd mnemonic is rejected", func(t *testing.T) {
		bad := opts
		bad.From = writeMnemonic(t, t.TempDir(), "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon")
		bad.KeystoreDir = filepath.Join(t.TempDir(), "ks")
		_, _, err := app.ImportMnemonic(bad)
		require.Error(t, err, "a bad BIP-39 checksum must not become a keystore")
		assert.NoFileExists(t, filepath.Join(bad.KeystoreDir, hdwallet.SeedFileName))
	})

	t.Run("anvil's mnemonic is refused", func(t *testing.T) {
		bad := opts
		bad.From = writeMnemonic(t, t.TempDir(), hdwallet.AnvilDefaultMnemonic)
		bad.KeystoreDir = filepath.Join(t.TempDir(), "ks")
		_, _, err := app.ImportMnemonic(bad)
		require.ErrorIs(t, err, hdwallet.ErrAnvilMnemonic)
	})
}

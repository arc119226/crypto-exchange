package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/app"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestVersion(t *testing.T) {
	out, err := run(t, "version")
	require.NoError(t, err)
	assert.Contains(t, out, "exchange dev")
	out, err = run(t, "version", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"version":"dev"`)
}

func TestExitCodes(t *testing.T) {
	assert.Equal(t, exitOK, exitCode(nil))
	assert.Equal(t, exitOK, exitCode(context.Canceled))
	assert.Equal(t, exitUsage, exitCode(errors.New("unknown flag")))
	assert.Equal(t, exitRuntime, exitCode(runtimeErr(errors.New("boom"))))
	assert.Equal(t, exitNotImplemented, exitCode(runtimeErr(app.ErrNotImplemented)))
	assert.Nil(t, runtimeErr(nil))
}

// The happy path lives in internal/app.TestImportMnemonic, which can lower
// the scrypt cost; going through the CLI would pay the production N = 2^18
// (256 MiB, and ten seconds under -race) for every `make test`. What is left
// here is the flag wiring and the refusals, none of which reach the KDF.
func TestKeysImportMnemonic(t *testing.T) {
	dir := t.TempDir()
	mnemonic := filepath.Join(dir, "mnemonic.txt")
	require.NoError(t, os.WriteFile(mnemonic,
		[]byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about\n"), 0o600))

	t.Run("needs a passphrase", func(t *testing.T) {
		t.Setenv("WALLET_KEYSTORE_PASSPHRASE", "")
		_, err := run(t, "keys", "import-mnemonic", "--from", mnemonic, "--keystore-dir", filepath.Join(dir, "nopass"))
		require.Error(t, err)
		assert.Equal(t, exitRuntime, exitCode(err))
		assert.ErrorContains(t, err, "WALLET_KEYSTORE_PASSPHRASE")
		assert.NoFileExists(t, filepath.Join(dir, "nopass", "hd-seed.json"))
	})

	t.Run("refuses anvil's mnemonic", func(t *testing.T) {
		t.Setenv("WALLET_KEYSTORE_PASSPHRASE", "it-passphrase")
		anvil := filepath.Join(dir, "anvil.txt")
		require.NoError(t, os.WriteFile(anvil, []byte("test test test test test test test test test test test junk\n"), 0o600))
		_, err := run(t, "keys", "import-mnemonic", "--from", anvil, "--keystore-dir", filepath.Join(dir, "anvil-ks"))
		require.Error(t, err)
		assert.Equal(t, exitRuntime, exitCode(err))
		assert.NoFileExists(t, filepath.Join(dir, "anvil-ks", "hd-seed.json"))
	})

	t.Run("reports a missing mnemonic file", func(t *testing.T) {
		t.Setenv("WALLET_KEYSTORE_PASSPHRASE", "it-passphrase")
		_, err := run(t, "keys", "import-mnemonic", "--from", filepath.Join(dir, "nope.txt"), "--keystore-dir", dir)
		require.Error(t, err)
		assert.Equal(t, exitRuntime, exitCode(err))
	})
}

func TestServeRejectsUnknownRole(t *testing.T) {
	_, err := run(t, "serve", "--role=nope")
	require.Error(t, err)
	assert.Equal(t, exitUsage, exitCode(err))
}

func TestMigrateNeedsDSN(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", "")
	_, err := run(t, "migrate", "up")
	require.Error(t, err)
	assert.Equal(t, exitRuntime, exitCode(err))
}

func TestKeysGenJWT(t *testing.T) {
	path := t.TempDir() + "/jwt/ed25519.pem"
	out, err := run(t, "keys", "gen-jwt", "--out", path)
	require.NoError(t, err)
	assert.Contains(t, out, "wrote")
	_, err = run(t, "keys", "gen-jwt", "--out", path)
	require.Error(t, err, "refuses to overwrite")
	_, err = run(t, "keys", "gen-jwt", "--out", path, "--force")
	require.NoError(t, err)
}

func TestHealthcheckUnreachable(t *testing.T) {
	_, err := run(t, "healthcheck", "--url", "http://127.0.0.1:1/readyz", "--timeout", "500ms")
	require.Error(t, err)
	assert.Equal(t, exitRuntime, exitCode(err))
}

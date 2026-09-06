package hdwallet_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
)

// The vectors below are anvil's, and anvil is the oracle: it prints
//
//	Mnemonic:        test test test test test test test test test test test junk
//	Derivation path: m/44'/60'/0'/0/
//
// followed by the keys of those addresses, on every startup — including in
// the e2e job's container logs. They are public development keys, which is
// exactly why hdwallet.Encrypt refuses to persist this mnemonic.
const (
	anvilAccount0 = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	anvilAccount1 = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"
	anvilAccount2 = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC"
)

func TestDeriveMatchesAnvil(t *testing.T) {
	w, err := hdwallet.FromMnemonic(hdwallet.AnvilDefaultMnemonic)
	require.NoError(t, err)
	defer w.Close()

	for i, want := range []string{anvilAccount0, anvilAccount1, anvilAccount2} {
		path, err := hdwallet.DepositPath(uint32(i)) //nolint:gosec // i is a small loop index
		require.NoError(t, err)
		got, err := w.Address(path)
		require.NoError(t, err)
		assert.Equal(t, want, got.Hex(), "%s must match what anvil prints", path)
	}
}

// TestDeriveIsDeterministic guards the loop in Derive: it zeroes the
// intermediate keys as it walks the path, and zeroing one too many (the
// master) would make the second call return something else.
func TestDeriveIsDeterministic(t *testing.T) {
	w, err := hdwallet.FromMnemonic(hdwallet.AnvilDefaultMnemonic)
	require.NoError(t, err)
	defer w.Close()

	seven, err := hdwallet.DepositPath(7)
	require.NoError(t, err)
	first, err := w.Address(seven)
	require.NoError(t, err)
	for range 3 {
		again, err := w.Address(seven)
		require.NoError(t, err)
		require.Equal(t, first, again)
	}
	// a different index must give a different address
	eight, err := hdwallet.DepositPath(8)
	require.NoError(t, err)
	other, err := w.Address(eight)
	require.NoError(t, err)
	assert.NotEqual(t, first, other)
}

func TestPaths(t *testing.T) {
	zero, err := hdwallet.DepositPath(0)
	require.NoError(t, err)
	assert.Equal(t, "m/44'/60'/0'/0/0", zero.String())

	last, err := hdwallet.DepositPath(hdwallet.MaxDepositIndex)
	require.NoError(t, err)
	assert.Equal(t, "m/44'/60'/0'/0/2147483647", last.String())

	assert.Equal(t, "m/44'/60'/1'/0/0", hdwallet.HotWalletPath().String())
	assert.NotEqual(t, zero.String(), hdwallet.HotWalletPath().String(),
		"deposit and hot wallet must live in different BIP-44 accounts")
}

// TestDepositPathRejectsHardenedRange: index 2^31 is the same 32-bit value as
// hardened index 0, so accepting it would let two pool rows derive one
// address. The DB sequence would have to reach two billion for this to
// happen, and it must still be an error rather than a silent alias.
func TestDepositPathRejectsHardenedRange(t *testing.T) {
	_, err := hdwallet.DepositPath(hdwallet.MaxDepositIndex + 1)
	require.Error(t, err)
	_, err = hdwallet.DepositPath(^uint32(0))
	require.Error(t, err)
}

// TestFromMnemonicVerifiesChecksum: cosmos/go-bip39's IsMnemonicValid only
// checks the word count and the word list, so one word swapped for another
// valid word passes it. FromMnemonic must reject that — it would otherwise
// derive an entirely different wallet from a typo.
func TestFromMnemonicVerifiesChecksum(t *testing.T) {
	const good = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	_, err := hdwallet.FromMnemonic(good)
	require.NoError(t, err)

	// last word swapped for another valid word: the checksum no longer holds
	typo := strings.TrimSuffix(good, "about") + "abandon"
	_, err = hdwallet.FromMnemonic(typo)
	require.Error(t, err, "a bad checksum must not derive a wallet")
}

func TestFromMnemonicRejectsGarbage(t *testing.T) {
	for _, m := range []string{"", "not a mnemonic at all", "abandon about"} {
		_, err := hdwallet.FromMnemonic(m)
		assert.Error(t, err, "%q", m)
	}
}

func TestKeystoreRoundTrip(t *testing.T) {
	const mnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	const pass = "correct horse battery staple"

	blob, err := hdwallet.Encrypt(mnemonic, pass, hdwallet.TestScrypt())
	require.NoError(t, err)
	assert.NotContains(t, string(blob), mnemonic, "the words must not survive in the file")

	got, err := hdwallet.Decrypt(blob, pass)
	require.NoError(t, err)
	assert.Equal(t, mnemonic, got)

	t.Run("wrong passphrase", func(t *testing.T) {
		_, err := hdwallet.Decrypt(blob, pass+"!")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), mnemonic)
	})

	t.Run("tampered ciphertext", func(t *testing.T) {
		var f map[string]any
		require.NoError(t, json.Unmarshal(blob, &f))
		ct, _ := f["ciphertext"].(string)
		f["ciphertext"] = "ff" + ct[2:]
		bad, err := json.Marshal(f)
		require.NoError(t, err)
		_, err = hdwallet.Decrypt(bad, pass)
		assert.Error(t, err)
	})

	t.Run("lowered work factor is rejected by the AAD", func(t *testing.T) {
		// An attacker who can rewrite the file could otherwise weaken the KDF
		// and keep the ciphertext; the header is authenticated, so it fails.
		var f map[string]any
		require.NoError(t, json.Unmarshal(blob, &f))
		params, _ := f["kdfparams"].(map[string]any)
		params["r"] = 1
		bad, err := json.Marshal(f)
		require.NoError(t, err)
		_, err = hdwallet.Decrypt(bad, pass)
		assert.Error(t, err)
	})
}

func TestEncryptRefusesAnvilMnemonic(t *testing.T) {
	_, err := hdwallet.Encrypt(hdwallet.AnvilDefaultMnemonic, "pw", hdwallet.TestScrypt())
	require.ErrorIs(t, err, hdwallet.ErrAnvilMnemonic)

	// spacing must not get it past the check
	_, err = hdwallet.Encrypt("  test  test test test test test test test test test test junk\n", "pw", hdwallet.TestScrypt())
	require.ErrorIs(t, err, hdwallet.ErrAnvilMnemonic)
}

func TestEncryptRequiresPassphrase(t *testing.T) {
	_, err := hdwallet.Encrypt("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
		"", hdwallet.TestScrypt())
	require.ErrorContains(t, err, "passphrase")
}

func TestScryptParamsAreBounded(t *testing.T) {
	const mnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	for name, p := range map[string]hdwallet.ScryptParams{
		"N too small":    {N: 1 << 8, R: 8, P: 1, DKLen: 32},
		"N too large":    {N: 1 << 24, R: 8, P: 1, DKLen: 32},
		"N not a power2": {N: 5000, R: 8, P: 1, DKLen: 32},
		"r out of range": {N: 1 << 12, R: 0, P: 1, DKLen: 32},
		"p out of range": {N: 1 << 12, R: 8, P: 9, DKLen: 32},
		"short key":      {N: 1 << 12, R: 8, P: 1, DKLen: 16},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := hdwallet.Encrypt(mnemonic, "pw", p)
			assert.Error(t, err)
		})
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keystore")
	const mnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	const pass = "s3cret"

	path, err := hdwallet.Save(dir, mnemonic, pass, hdwallet.TestScrypt())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, hdwallet.SeedFileName), path)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the keystore is written private")

	w, err := hdwallet.Load(dir, pass)
	require.NoError(t, err)
	defer w.Close()
	dpath, err := hdwallet.DepositPath(0)
	require.NoError(t, err)
	addr, err := w.Address(dpath)
	require.NoError(t, err)
	assert.NotEqual(t, anvilAccount0, addr.Hex(), "a different seed must give different addresses")

	_, err = hdwallet.Load(dir, "wrong")
	assert.Error(t, err)

	_, err = hdwallet.Load(filepath.Join(dir, "nope"), pass)
	assert.Error(t, err)
}

// TestWalletIsNeverLogged is the §14 requirement that key material cannot
// reach a log line even by accident.
func TestWalletIsNeverLogged(t *testing.T) {
	w, err := hdwallet.FromMnemonic(hdwallet.AnvilDefaultMnemonic)
	require.NoError(t, err)
	defer w.Close()

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("signer ready", slog.Any("wallet", w))
	assert.Contains(t, buf.String(), "[redacted]")
	assert.NotContains(t, buf.String(), "test test")

	assert.Equal(t, "[redacted]", w.String())
}

func TestCloseIsIdempotent(t *testing.T) {
	w, err := hdwallet.FromMnemonic(hdwallet.AnvilDefaultMnemonic)
	require.NoError(t, err)
	w.Close()
	w.Close()
}

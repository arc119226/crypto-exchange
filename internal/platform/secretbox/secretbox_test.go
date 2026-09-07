package secretbox

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func masterKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := masterKey(t)
	sealed, err := Seal(k, "hunter2")
	require.NoError(t, err)

	got, err := Open(k, sealed)
	require.NoError(t, err)
	assert.Equal(t, "hunter2", got)
	assert.False(t, bytes.Contains(sealed, []byte("hunter2")), "the plaintext must not survive in the envelope")
}

// A fresh nonce per call: sealing the same secret twice must not produce the
// same bytes, or an observer of the table learns which endpoints share a
// secret.
func TestSealIsRandomised(t *testing.T) {
	k := masterKey(t)
	a, err := Seal(k, "same")
	require.NoError(t, err)
	b, err := Seal(k, "same")
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
}

func TestOpenRejectsWrongKeyAndTampering(t *testing.T) {
	k := masterKey(t)
	sealed, err := Seal(k, "hunter2")
	require.NoError(t, err)

	_, err = Open(masterKey(t), sealed)
	assert.Error(t, err, "a different master key must not open it")

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xff
	_, err = Open(k, tampered)
	assert.Error(t, err, "GCM authenticates before it decrypts")

	_, err = Open(k, sealed[:3])
	assert.Error(t, err, "shorter than a nonce")

	_, err = Seal([]byte("too short"), "x")
	assert.Error(t, err)
}

// The envelope moved here out of internal/auth, where auth.api_keys.secret_enc
// rows have been written since migration 0007. Those rows must still open, so
// this pins the format against a value built by the standard library rather
// than by Seal: 12-byte GCM nonce, then the ciphertext with its tag. If a
// future change to Seal breaks this, every API key in every deployment stops
// authenticating.
func TestOpenReadsTheFormatWrittenBeforeTheMove(t *testing.T) {
	key := bytes.Repeat([]byte{0x2a}, KeySize)
	nonce := bytes.Repeat([]byte{0x07}, 12)

	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	require.Equal(t, len(nonce), gcm.NonceSize(), "the format assumes the standard 12-byte nonce")

	legacy := append(bytes.Clone(nonce), gcm.Seal(nil, nonce, []byte("ak_secret_from_0007"), nil)...)

	got, err := Open(key, legacy)
	require.NoError(t, err)
	assert.Equal(t, "ak_secret_from_0007", got)
}

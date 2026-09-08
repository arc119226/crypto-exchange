package secretbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyringOpensUnderEitherKeyAndSealsUnderTheCurrent(t *testing.T) {
	old, current := masterKey(t), masterKey(t)
	underOld, err := Seal(old, "hunter2")
	require.NoError(t, err)

	k := Keyring{Current: current, Previous: old}
	require.NoError(t, k.Validate())
	got, err := k.Open(underOld)
	require.NoError(t, err)
	assert.Equal(t, "hunter2", got)

	sealed, err := k.Seal("hunter3")
	require.NoError(t, err)
	_, err = Open(old, sealed)
	assert.Error(t, err, "Seal uses the current key only")
	got, err = Open(current, sealed)
	require.NoError(t, err)
	assert.Equal(t, "hunter3", got)

	// once the previous key is dropped the old rows stop opening -- which
	// is why rewrap exists
	_, err = Keyring{Current: current}.Open(underOld)
	assert.Error(t, err)
}

func TestKeyringRewrapIsIdempotent(t *testing.T) {
	old, current := masterKey(t), masterKey(t)
	underOld, err := Seal(old, "s")
	require.NoError(t, err)
	k := Keyring{Current: current, Previous: old}

	out, changed, err := k.Rewrap(underOld)
	require.NoError(t, err)
	assert.True(t, changed)
	got, err := Open(current, out)
	require.NoError(t, err)
	assert.Equal(t, "s", got)

	again, changed, err := k.Rewrap(out)
	require.NoError(t, err)
	assert.False(t, changed, "already under the current key")
	assert.Equal(t, out, again, "and the bytes are returned untouched")

	_, _, err = Keyring{Current: masterKey(t), Previous: masterKey(t)}.Rewrap(underOld)
	assert.Error(t, err, "a blob neither key opens is an error, not a silent skip")
}

func TestKeyringValidate(t *testing.T) {
	k := masterKey(t)
	assert.NoError(t, Keyring{}.Validate())
	assert.NoError(t, Keyring{Current: k}.Validate())
	assert.NoError(t, Keyring{Current: k, Previous: masterKey(t)}.Validate())
	assert.ErrorContains(t, Keyring{Previous: k}.Validate(), "without a current")
	assert.ErrorContains(t, Keyring{Current: k, Previous: k}.Validate(), "is the current key")
	assert.ErrorContains(t, Keyring{Current: k[:5]}.Validate(), "32 bytes")
	assert.ErrorContains(t, Keyring{Current: k, Previous: k[:5]}.Validate(), "32 bytes")
}

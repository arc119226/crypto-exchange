package main

import (
	"bytes"
	"context"
	"errors"
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

func TestNotImplementedCommands(t *testing.T) {
	for _, args := range [][]string{{"keys", "import-mnemonic"}} {
		_, err := run(t, args...)
		assert.Equal(t, exitNotImplemented, exitCode(err), "%v", args)
	}
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

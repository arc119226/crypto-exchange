package app

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"EXCHANGE_ENV", "TENANT_ID", "LOG_LEVEL", "OPS_ADDR", "HTTP_ADDR", "DATABASE_URL", "DATABASE_URL_FILE",
		"DATABASE_MAX_CONNS", "DATABASE_CONNECT_TIMEOUT", "NATS_URL", "REDIS_ADDR", "REDIS_PASSWORD", "ETH_RPC_URL", "ETH_CHAIN_ID",
		"ETH_RECONCILE_INTERVAL", "ETH_RECONCILE_ENABLED", "ETH_HOT_WALLET_MIN", "ETH_SWEEP_INTERVAL",
		"JWT_PRIVATE_KEY_FILE", "JWT_JWKS_URL", "SHUTDOWN_DRAIN_DELAY", "SHUTDOWN_TIMEOUT"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://ex_all:pw@localhost:5432/exchange")
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "dev", cfg.Env)
	assert.Equal(t, "default", cfg.TenantID)
	assert.Equal(t, ":9100", cfg.OpsAddr)
	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, int32(10), cfg.DB.MaxConns)
	assert.Equal(t, 5*time.Second, cfg.DB.ConnectTimeout)
	assert.Equal(t, int64(31337), cfg.Chain.ChainID)
	assert.Equal(t, 5*time.Minute, cfg.Chain.ReconcileInterval)
	assert.True(t, cfg.Chain.ReconcileEnabled)
	// Zero means the low-balance alert is off until an operator picks a floor:
	// a default threshold would either fire on every fresh deployment or be so
	// low it never fires at all.
	min, err := cfg.Chain.MinHotWallet()
	require.NoError(t, err)
	assert.True(t, min.IsZero())
	assert.Equal(t, 2*time.Second, cfg.Shutdown.DrainDelay)
	assert.Equal(t, 20*time.Second, cfg.Shutdown.Timeout)
	assert.Equal(t, "postgres://ex_all:pw@localhost:5432/exchange", cfg.DB.URL.Reveal())
}

func TestLoadConfigRequiresDatabaseURL(t *testing.T) {
	clearEnv(t)
	_, err := LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}

func TestLoadConfigFileVariant(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "dsn")
	require.NoError(t, os.WriteFile(path, []byte("postgres://ex_api:filepw@db:5432/exchange\n"), 0o600))
	t.Setenv("DATABASE_URL_FILE", path)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "postgres://ex_api:filepw@db:5432/exchange", cfg.DB.URL.Reveal(), "trimmed and expanded")

	// explicit value wins over the file
	t.Setenv("DATABASE_URL", "postgres://direct@db/x")
	cfg, err = LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "postgres://direct@db/x", cfg.DB.URL.Reveal())

	// missing file is an error
	clearEnv(t)
	t.Setenv("DATABASE_URL_FILE", filepath.Join(dir, "missing"))
	_, err = LoadConfig()
	assert.Error(t, err)
}

func TestLoadConfigValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x@db/x")
	t.Setenv("LOG_LEVEL", "loud")
	_, err := LoadConfig()
	assert.ErrorContains(t, err, "LOG_LEVEL")

	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("DATABASE_MAX_CONNS", "0")
	_, err = LoadConfig()
	assert.ErrorContains(t, err, "DATABASE_MAX_CONNS")

	t.Setenv("DATABASE_MAX_CONNS", "4")
	t.Setenv("SHUTDOWN_TIMEOUT", "not-a-duration")
	_, err = LoadConfig()
	assert.Error(t, err)

	t.Setenv("SHUTDOWN_TIMEOUT", "5s")
	t.Setenv("TENANT_ID", "  ")
	_, err = LoadConfig()
	assert.ErrorContains(t, err, "TENANT_ID")

	t.Setenv("TENANT_ID", "default")
	t.Setenv("ETH_RECONCILE_INTERVAL", "0s")
	_, err = LoadConfig()
	assert.ErrorContains(t, err, "ETH_RECONCILE_INTERVAL")

	t.Setenv("ETH_RECONCILE_INTERVAL", "5m")
	t.Setenv("ETH_HOT_WALLET_MIN", "not-a-number")
	_, err = LoadConfig()
	assert.ErrorContains(t, err, "ETH_HOT_WALLET_MIN")

	// Negative is rejected rather than treated as "off": an operator who typed
	// a minus sign meant something, and silently disabling the alert is the
	// one reading that cannot be what they meant.
	t.Setenv("ETH_HOT_WALLET_MIN", "-1")
	_, err = LoadConfig()
	assert.ErrorContains(t, err, "ETH_HOT_WALLET_MIN")
}

func TestConfigLogValueRedacts(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://ex_all:hunter2@localhost:5432/exchange")
	t.Setenv("REDIS_PASSWORD", "redis-hunter2")
	cfg, err := LoadConfig()
	require.NoError(t, err)
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", slog.Any("config", cfg))
	assert.NotContains(t, buf.String(), "hunter2")
	assert.Contains(t, buf.String(), "postgres://ex_all:xxxxx@localhost:5432/exchange")
	assert.Contains(t, buf.String(), `"otel_endpoint_set":false`)
	assert.Empty(t, cfg.OTLPEndpoint, "tracing is off unless OTEL_EXPORTER_OTLP_ENDPOINT is set")
}

// A hosted RPC endpoint carries its API key in the path, not in userinfo, so
// the Postgres-shaped redaction above does not cover it. This is not
// hypothetical: the startup line printed a live Alchemy key in full.
func TestConfigLogValueRedactsTheRPCKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://ex_all:pw@localhost:5432/exchange")
	t.Setenv("ETH_RPC_URL", "https://eth-sepolia.g.alchemy.com/v2/not-a-real-key")
	cfg, err := LoadConfig()
	require.NoError(t, err)
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", slog.Any("config", cfg))
	assert.NotContains(t, buf.String(), "not-a-real-key")
	// The host survives: which provider is in use is what an operator reading
	// this line actually needs.
	assert.Contains(t, buf.String(), "https://eth-sepolia.g.alchemy.com/[redacted]")
}

func TestParseRoles(t *testing.T) {
	rs, err := ParseRoles("all")
	require.NoError(t, err)
	assert.Equal(t, AllRoles, rs)
	assert.Equal(t, "all", RolesLabel(rs))

	rs, err = ParseRoles("api, engine,api")
	require.NoError(t, err)
	assert.Equal(t, []Role{RoleAPI, RoleEngine}, rs)
	assert.Equal(t, "api,engine", RolesLabel(rs))

	_, err = ParseRoles("api,bogus")
	assert.ErrorContains(t, err, "bogus")
	_, err = ParseRoles(" , ")
	assert.Error(t, err)
}

// TestValidateForKeepsTheKeystoreSecretWithTheSigner: the passphrase is
// refused by any process that runs no signer (docs/plan-v1.0.md §14), so a
// deployment that hands it to every role fails at start, not in a later
// review.
func TestValidateForKeepsTheKeystoreSecretWithTheSigner(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x@db/x")
	t.Setenv("WALLET_KEYSTORE_PASSPHRASE", "open sesame")
	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.NoError(t, cfg.ValidateFor([]Role{RoleSigner}))
	assert.NoError(t, cfg.ValidateFor([]Role{RoleAll}))
	assert.NoError(t, cfg.ValidateFor([]Role{RoleChain, RoleSigner}))
	err = cfg.ValidateFor([]Role{RoleAPI, RoleEngine})
	assert.ErrorContains(t, err, "WALLET_KEYSTORE_PASSPHRASE")
	assert.ErrorContains(t, err, "api,engine")

	t.Setenv("WALLET_KEYSTORE_PASSPHRASE", "")
	cfg, err = LoadConfig()
	require.NoError(t, err)
	assert.NoError(t, cfg.ValidateFor([]Role{RoleAPI}), "no secret, nothing to refuse")
}

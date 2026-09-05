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

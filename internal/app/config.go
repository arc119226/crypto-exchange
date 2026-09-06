// Package app assembles roles into a running process: configuration,
// dependency connections with retry, health/readiness/metrics endpoints,
// graceful shutdown, and the facade used by the CLI.
package app

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// Config is the 12-factor configuration of the exchange binary. Every value
// comes from the environment; secrets additionally accept a *_FILE variant.
type Config struct {
	Env       string `env:"EXCHANGE_ENV" envDefault:"dev"`
	TenantID  string `env:"TENANT_ID" envDefault:"default"`
	LogLevel  string `env:"LOG_LEVEL" envDefault:"info"`
	OpsAddr   string `env:"OPS_ADDR" envDefault:":9100"`
	HTTPAddr  string `env:"HTTP_ADDR" envDefault:":8080"`
	WSAddr    string `env:"WS_ADDR" envDefault:":8081"`
	AdminAddr string `env:"ADMIN_ADDR" envDefault:":8082"`

	DB       DBConfig       `envPrefix:"DATABASE_"`
	NATS     NATSConfig     `envPrefix:"NATS_"`
	Redis    RedisConfig    `envPrefix:"REDIS_"`
	Chain    ChainConfig    `envPrefix:"ETH_"`
	JWT      JWTConfig      `envPrefix:"JWT_"`
	Admin    AdminConfig    `envPrefix:"ADMIN_"`
	Shutdown ShutdownConfig `envPrefix:"SHUTDOWN_"`
}

// AdminConfig configures the operator API (admin role). APIKey is the static
// key of Phase 2–4 (docs/plan-v1.0.md §14); Phase 5 adds sessions + TOTP.
type AdminConfig struct {
	APIKey telemetry.Secret `env:"API_KEY"`
}

// DBConfig configures the Postgres pool. URL is required for every role.
type DBConfig struct {
	URL            telemetry.Secret `env:"URL,required,notEmpty"`
	MaxConns       int32            `env:"MAX_CONNS" envDefault:"10"`
	ConnectTimeout time.Duration    `env:"CONNECT_TIMEOUT" envDefault:"5s"`
}

// NATSConfig configures JetStream; an empty URL disables NATS (dev only).
type NATSConfig struct {
	URL string `env:"URL"`
}

// RedisConfig configures the optional Redis client.
type RedisConfig struct {
	Addr     string           `env:"ADDR"`
	Password telemetry.Secret `env:"PASSWORD"`
}

// ChainConfig configures the EVM RPC endpoint (Phase 4 uses it).
type ChainConfig struct {
	RPCURL  string `env:"RPC_URL"`
	ChainID int64  `env:"CHAIN_ID" envDefault:"31337"`
}

// JWTConfig locates the signing key (api role only) and the JWKS URL.
type JWTConfig struct {
	PrivateKeyFile string `env:"PRIVATE_KEY_FILE"`
	JWKSURL        string `env:"JWKS_URL"`
}

// ShutdownConfig controls graceful shutdown.
type ShutdownConfig struct {
	DrainDelay time.Duration `env:"DRAIN_DELAY" envDefault:"2s"`
	Timeout    time.Duration `env:"TIMEOUT" envDefault:"20s"`
}

// secretsWithFileVariant lists variables that may be supplied as NAME_FILE.
var secretsWithFileVariant = []string{
	"DATABASE_URL",
	"REDIS_PASSWORD",
	"WALLET_KEYSTORE_PASSPHRASE",
	"WEBHOOK_SIGNING_KEY",
	"ADMIN_BOOTSTRAP_PASSWORD",
	"ADMIN_API_KEY",
}

// LoadConfig reads the environment (after expanding *_FILE secrets) and
// validates it.
func LoadConfig() (Config, error) {
	if err := expandFileEnv(secretsWithFileVariant); err != nil {
		return Config{}, err
	}
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// expandFileEnv sets NAME from the contents of the file named by NAME_FILE
// when NAME itself is empty. Only the listed names are considered so that
// path-valued variables such as JWT_PRIVATE_KEY_FILE are never expanded.
func expandFileEnv(names []string) error {
	for _, name := range names {
		if os.Getenv(name) != "" {
			continue
		}
		path := os.Getenv(name + "_FILE")
		if path == "" {
			continue
		}
		b, err := os.ReadFile(path) //nolint:gosec // path comes from the operator's own environment
		if err != nil {
			return fmt.Errorf("config: read %s_FILE: %w", name, err)
		}
		if err := os.Setenv(name, strings.TrimSpace(string(b))); err != nil {
			return fmt.Errorf("config: set %s: %w", name, err)
		}
	}
	return nil
}

// Validate checks cross-field constraints that struct tags cannot express.
func (c Config) Validate() error {
	if _, err := telemetry.ParseLevel(c.LogLevel); err != nil {
		return fmt.Errorf("config: LOG_LEVEL: %w", err)
	}
	if strings.TrimSpace(c.TenantID) == "" {
		return fmt.Errorf("config: TENANT_ID must not be empty")
	}
	if c.DB.MaxConns <= 0 {
		return fmt.Errorf("config: DATABASE_MAX_CONNS must be positive")
	}
	if c.Shutdown.Timeout <= 0 || c.Shutdown.DrainDelay < 0 {
		return fmt.Errorf("config: SHUTDOWN_TIMEOUT must be positive and SHUTDOWN_DRAIN_DELAY non-negative")
	}
	return nil
}

// LogValue implements slog.LogValuer; secrets are redacted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("env", c.Env),
		slog.String("tenant_id", c.TenantID),
		slog.String("log_level", c.LogLevel),
		slog.String("ops_addr", c.OpsAddr),
		slog.String("http_addr", c.HTTPAddr),
		slog.String("ws_addr", c.WSAddr),
		slog.String("admin_addr", c.AdminAddr),
		slog.String("database_url", telemetry.RedactURL(c.DB.URL.Reveal())),
		slog.Int("database_max_conns", int(c.DB.MaxConns)),
		slog.String("nats_url", c.NATS.URL),
		slog.String("redis_addr", c.Redis.Addr),
		slog.String("eth_rpc_url", c.Chain.RPCURL),
		slog.Int64("eth_chain_id", c.Chain.ChainID),
		slog.String("jwt_private_key_file", c.JWT.PrivateKeyFile),
		slog.String("jwt_jwks_url", c.JWT.JWKSURL),
		slog.Duration("shutdown_drain_delay", c.Shutdown.DrainDelay),
		slog.Duration("shutdown_timeout", c.Shutdown.Timeout),
	)
}

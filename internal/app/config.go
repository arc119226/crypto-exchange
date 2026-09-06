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

	DB     DBConfig     `envPrefix:"DATABASE_"`
	NATS   NATSConfig   `envPrefix:"NATS_"`
	Redis  RedisConfig  `envPrefix:"REDIS_"`
	Chain  ChainConfig  `envPrefix:"ETH_"`
	Wallet WalletConfig `envPrefix:"WALLET_"`
	JWT    JWTConfig    `envPrefix:"JWT_"`
	Auth   AuthConfig   `envPrefix:"AUTH_"`
	// APIKeyMasterKey (API_KEY_MASTER_KEY, 32 bytes hex) encrypts API key
	// secrets at rest; the api role needs it (ADR-0006).
	APIKeyMasterKey telemetry.Secret `env:"API_KEY_MASTER_KEY"`
	RateLimit       RateLimitConfig  `envPrefix:"RATELIMIT_"`
	Admin           AdminConfig      `envPrefix:"ADMIN_"`
	Engine          EngineConfig     `envPrefix:"ENGINE_"`
	Registry        RegistryConfig   `envPrefix:"REGISTRY_"`
	Outbox          OutboxConfig     `envPrefix:"OUTBOX_"`
	Shutdown        ShutdownConfig   `envPrefix:"SHUTDOWN_"`
}

// EngineConfig tunes the trading engine (engine role) and the command bus
// that reaches it from a separate api role (docs/plan-v1.0.md §5.2).
type EngineConfig struct {
	QueueSize int `env:"QUEUE_SIZE" envDefault:"1024"` // commands waiting per market
	// CommandSubjectPrefix is the first tokens of cmd.trading.<tenant>.<market>.
	CommandSubjectPrefix string `env:"COMMAND_SUBJECT_PREFIX" envDefault:"cmd.trading"`
	// CommandTimeout bounds one request-reply round trip; exceeding it is a 503.
	CommandTimeout time.Duration `env:"COMMAND_TIMEOUT" envDefault:"5s"`
	// InternalTokenTTL is the lifetime of the aud=internal JWT the api role
	// mints per command (docs/plan-v1.0.md §14).
	InternalTokenTTL time.Duration `env:"INTERNAL_TOKEN_TTL" envDefault:"5m"`
}

// RegistryConfig tunes the registry cache of roles that do not run the
// engine. The engine reloads on market.updated; an api role without one
// re-reads on this interval instead, so a delisted market stops being
// offered without a restart.
type RegistryConfig struct {
	RefreshInterval time.Duration `env:"REFRESH_INTERVAL" envDefault:"30s"`
}

// OutboxConfig tunes the outbox relay (engine role, needs NATS).
type OutboxConfig struct {
	PollInterval time.Duration `env:"POLL_INTERVAL" envDefault:"100ms"`
	BatchSize    int32         `env:"BATCH_SIZE" envDefault:"100"`
}

// AuthConfig tunes sessions (api role).
type AuthConfig struct {
	Issuer     string        `env:"ISSUER" envDefault:"exchange"`
	AccessTTL  time.Duration `env:"ACCESS_TTL" envDefault:"15m"`
	RefreshTTL time.Duration `env:"REFRESH_TTL" envDefault:"168h"`
}

// RateLimitConfig holds the token-bucket limits of docs/plan-v1.0.md §14 as
// "N/window" strings.
type RateLimitConfig struct {
	LoginPerIP       string `env:"LOGIN_PER_IP" envDefault:"10/1m"`
	LoginPerAccount  string `env:"LOGIN_PER_ACCOUNT" envDefault:"5/1m"`
	OrdersPerAccount string `env:"ORDERS_PER_ACCOUNT" envDefault:"20/1s"`
}

// AdminConfig configures the operator API (admin role). APIKey is the static
// key of Phase 2–4 (docs/plan-v1.0.md §14); Phase 5 adds sessions + TOTP.
type AdminConfig struct {
	APIKey            telemetry.Secret `env:"API_KEY"`
	BootstrapEmail    string           `env:"BOOTSTRAP_EMAIL"`
	BootstrapPassword telemetry.Secret `env:"BOOTSTRAP_PASSWORD"`
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

// ChainConfig configures the EVM endpoint and the deposit scanner
// (docs/plan-v1.0.md §6.4.1).
type ChainConfig struct {
	RPCURL  string `env:"RPC_URL"`
	ChainID int64  `env:"CHAIN_ID" envDefault:"31337"`
	// ScanInterval is how often the chain role polls for new blocks.
	ScanInterval time.Duration `env:"SCAN_INTERVAL" envDefault:"2s"`
	// ScanStartBlock is the last block treated as already scanned on a fresh
	// database, so scanning begins just after it. Useful when pointing at a
	// chain whose early history cannot contain a deposit of ours.
	ScanStartBlock uint64 `env:"SCAN_START_BLOCK"`
	// ScanBatchSize caps the blocks one tick covers while catching up.
	ScanBatchSize uint64 `env:"SCAN_BATCH_SIZE" envDefault:"200"`
	// BlockRingDepth is how many block hashes are remembered, and therefore
	// the deepest reorg resolvable without an operator.
	BlockRingDepth uint64 `env:"BLOCK_RING_DEPTH" envDefault:"128"`
	// OrphanExpiryBlocks is how long an orphaned deposit waits to reappear
	// before it is dropped (anvil 100, Sepolia 1000).
	OrphanExpiryBlocks uint64 `env:"ORPHAN_EXPIRY_BLOCKS" envDefault:"100"`
	// RequiredConfirmations applies to an asset whose registry row says 0;
	// normally the asset decides.
	RequiredConfirmations int32 `env:"REQUIRED_CONFIRMATIONS_DEFAULT" envDefault:"1"`
	// WithdrawalInterval is how often the chain role runs the withdrawal
	// worker. It is separate from ScanInterval because the two answer to
	// different clocks: scanning follows the chain's block time, while a
	// withdrawal only waits on a policy decision and a ledger write.
	WithdrawalInterval time.Duration `env:"WITHDRAWAL_INTERVAL" envDefault:"1s"`
	// WithdrawalBatchSize caps how many withdrawals one tick claims.
	WithdrawalBatchSize int32 `env:"WITHDRAWAL_BATCH_SIZE" envDefault:"50"`
}

// WalletConfig locates the HD seed and sizes the deposit address pool
// (docs/plan-v1.0.md §6.4.1, §14). Only the signer role reads KeystoreDir and
// Passphrase; every other role fails to start if it is given them, because
// holding them would defeat the split.
type WalletConfig struct {
	KeystoreDir string           `env:"KEYSTORE_DIR"`
	Passphrase  telemetry.Secret `env:"KEYSTORE_PASSPHRASE"`
	// AddressPoolMin is how many unassigned deposit addresses the signer keeps
	// ahead of demand; the api role returns 503 when the pool runs dry.
	AddressPoolMin int `env:"ADDRESS_POOL_MIN" envDefault:"50"`
	// AddressPoolInterval is how often the signer tops the pool up.
	AddressPoolInterval time.Duration `env:"ADDRESS_POOL_INTERVAL" envDefault:"30s"`
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
	"API_KEY_MASTER_KEY",
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
	if c.Auth.AccessTTL <= 0 || c.Auth.RefreshTTL <= 0 || strings.TrimSpace(c.Auth.Issuer) == "" {
		return fmt.Errorf("config: AUTH_ACCESS_TTL / AUTH_REFRESH_TTL must be positive and AUTH_ISSUER non-empty")
	}
	if c.Engine.QueueSize <= 0 {
		return fmt.Errorf("config: ENGINE_QUEUE_SIZE must be positive")
	}
	if c.Outbox.PollInterval <= 0 || c.Outbox.BatchSize <= 0 {
		return fmt.Errorf("config: OUTBOX_POLL_INTERVAL and OUTBOX_BATCH_SIZE must be positive")
	}
	if c.Shutdown.Timeout <= 0 || c.Shutdown.DrainDelay < 0 {
		return fmt.Errorf("config: SHUTDOWN_TIMEOUT must be positive and SHUTDOWN_DRAIN_DELAY non-negative")
	}
	if c.Wallet.AddressPoolMin <= 0 {
		return fmt.Errorf("config: WALLET_ADDRESS_POOL_MIN must be positive")
	}
	if c.Wallet.AddressPoolInterval <= 0 {
		return fmt.Errorf("config: WALLET_ADDRESS_POOL_INTERVAL must be positive")
	}
	if c.Chain.ChainID <= 0 {
		return fmt.Errorf("config: ETH_CHAIN_ID must be positive")
	}
	if c.Chain.ScanInterval <= 0 {
		return fmt.Errorf("config: ETH_SCAN_INTERVAL must be positive")
	}
	if c.Chain.ScanBatchSize == 0 || c.Chain.BlockRingDepth == 0 {
		return fmt.Errorf("config: ETH_SCAN_BATCH_SIZE and ETH_BLOCK_RING_DEPTH must be positive")
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
		slog.Duration("eth_scan_interval", c.Chain.ScanInterval),
		slog.Duration("eth_withdrawal_interval", c.Chain.WithdrawalInterval),
		slog.Uint64("eth_scan_batch_size", c.Chain.ScanBatchSize),
		slog.Uint64("eth_block_ring_depth", c.Chain.BlockRingDepth),
		slog.Uint64("eth_orphan_expiry_blocks", c.Chain.OrphanExpiryBlocks),
		slog.Int("eth_required_confirmations_default", int(c.Chain.RequiredConfirmations)),
		slog.String("wallet_keystore_dir", c.Wallet.KeystoreDir),
		slog.Bool("wallet_keystore_passphrase_set", c.Wallet.Passphrase.IsSet()),
		slog.Int("wallet_address_pool_min", c.Wallet.AddressPoolMin),
		slog.Duration("wallet_address_pool_interval", c.Wallet.AddressPoolInterval),
		slog.String("jwt_private_key_file", c.JWT.PrivateKeyFile),
		slog.String("jwt_jwks_url", c.JWT.JWKSURL),
		slog.String("auth_issuer", c.Auth.Issuer),
		slog.Duration("auth_access_ttl", c.Auth.AccessTTL),
		slog.Duration("auth_refresh_ttl", c.Auth.RefreshTTL),
		slog.Bool("api_key_master_key_set", c.APIKeyMasterKey.IsSet()),
		slog.String("ratelimit_login_per_ip", c.RateLimit.LoginPerIP),
		slog.String("ratelimit_login_per_account", c.RateLimit.LoginPerAccount),
		slog.String("ratelimit_orders_per_account", c.RateLimit.OrdersPerAccount),
		slog.Int("engine_queue_size", c.Engine.QueueSize),
		slog.Duration("outbox_poll_interval", c.Outbox.PollInterval),
		slog.Duration("shutdown_drain_delay", c.Shutdown.DrainDelay),
		slog.Duration("shutdown_timeout", c.Shutdown.Timeout),
	)
}

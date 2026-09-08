// Package app assembles roles into a running process: configuration,
// dependency connections with retry, health/readiness/metrics endpoints,
// graceful shutdown, and the facade used by the CLI.
package app

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/trading"
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
	// APIKeyMasterKeyPrevious (API_KEY_MASTER_KEY_PREVIOUS) is the key the
	// current one replaced, set for the length of a rotation: rows sealed
	// under it still open until `exchange keys rewrap --domain api-keys`
	// has rewritten them (docs/runbooks/key-rotation.md).
	APIKeyMasterKeyPrevious telemetry.Secret `env:"API_KEY_MASTER_KEY_PREVIOUS"`
	RateLimit               RateLimitConfig  `envPrefix:"RATELIMIT_"`
	Admin                   AdminConfig      `envPrefix:"ADMIN_"`
	Engine                  EngineConfig     `envPrefix:"ENGINE_"`
	Registry                RegistryConfig   `envPrefix:"REGISTRY_"`
	Outbox                  OutboxConfig     `envPrefix:"OUTBOX_"`
	Webhook                 WebhookConfig    `envPrefix:"WEBHOOK_"`
	MarketData              MarketDataConfig `envPrefix:"MARKETDATA_"`
	Stream                  StreamConfig     `envPrefix:"STREAM_"`
	Retention               RetentionConfig  `envPrefix:"RETENTION_"`
	Shutdown                ShutdownConfig   `envPrefix:"SHUTDOWN_"`
	// OTLPEndpoint enables tracing (docs/plan-v1.0.md §15): the OTLP/HTTP
	// base URL spans are exported to, e.g. http://jaeger:4318. It keeps the
	// OpenTelemetry SDK's own variable name, unprefixed, because the SDK
	// reads its siblings (OTEL_TRACES_SAMPLER, OTEL_TRACES_SAMPLER_ARG)
	// the same way. Empty means no tracing and no exporter.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
}

// StreamConfig tunes the WebSocket server (stream role, docs/plan-v1.0.md
// §7.5). The listener address is WS_ADDR.
type StreamConfig struct {
	// WriteBuffer is the per-connection send queue; a client that lets it
	// fill is disconnected (slow_consumer) rather than waited for.
	WriteBuffer int `env:"WRITE_BUFFER" envDefault:"256"`
	// PingInterval and PongTimeout: a ping every interval, and a pong that
	// does not come back within the timeout closes the connection.
	PingInterval time.Duration `env:"PING_INTERVAL" envDefault:"15s"`
	PongTimeout  time.Duration `env:"PONG_TIMEOUT" envDefault:"15s"`
	WriteTimeout time.Duration `env:"WRITE_TIMEOUT" envDefault:"5s"`
	// MaxMessageBytes caps a client frame.
	MaxMessageBytes int64 `env:"MAX_MESSAGE_BYTES" envDefault:"4096"`
	// AuthTimeout is how long a private connection may stay unauthenticated.
	AuthTimeout time.Duration `env:"AUTH_TIMEOUT" envDefault:"5s"`
	// ResumeWindow is how long after the auth acknowledgement a resume is
	// accepted; live frames are held meanwhile (docs/ws-api.md).
	ResumeWindow   time.Duration `env:"RESUME_WINDOW" envDefault:"1s"`
	ResumePageSize int32         `env:"RESUME_PAGE_SIZE" envDefault:"500"`
	// MaxSubscriptions caps public subscriptions per connection.
	MaxSubscriptions int `env:"MAX_SUBSCRIPTIONS" envDefault:"64"`
	// DepthLevels bounds the levels per side of a depth snapshot.
	DepthLevels int `env:"DEPTH_LEVELS" envDefault:"200"`
	// FlushInterval is the last-resort close of an open engine command in
	// the shadow book (marketdata.BookProjector). Longer than a relay batch
	// boundary on purpose: a command split across two batches must not be
	// closed early.
	FlushInterval time.Duration `env:"FLUSH_INTERVAL" envDefault:"50ms"`
	// TickerInterval and KlineInterval coalesce pushes per market.
	TickerInterval time.Duration `env:"TICKER_INTERVAL" envDefault:"1s"`
	KlineInterval  time.Duration `env:"KLINE_INTERVAL" envDefault:"250ms"`
	// SnapshotInterval bounds how often a market's depth is written to the
	// cache the api role serves GET /depth from.
	SnapshotInterval time.Duration `env:"SNAPSHOT_INTERVAL" envDefault:"100ms"`
	// AllowedOrigins are the browser origins accepted on upgrade, comma
	// separated. "*" accepts any and is refused outside dev; empty accepts
	// the listener's own host only. No cookie is involved -- the private
	// endpoint authenticates with an explicit token frame -- so this bounds
	// who may consume resources, not who may act as a user.
	AllowedOrigins []string `env:"ALLOWED_ORIGINS" envSeparator:","`
}

// MarketDataConfig tunes the candle writer (worker role) and the shadow
// book (stream role), docs/plan-v1.0.md §12 Phase 6.
type MarketDataConfig struct {
	// KlinePollInterval is how often the worker looks for trades to fold
	// once it has caught up; while behind it folds back to back.
	KlinePollInterval time.Duration `env:"KLINE_POLL_INTERVAL" envDefault:"1s"`
	// KlineBatchSeqs caps the engine commands one fold covers per market.
	KlineBatchSeqs int64 `env:"KLINE_BATCH_SEQS" envDefault:"500"`
	// RebuildBuffer caps the events the stream holds while it reads a
	// book snapshot; overflowing it restarts the rebuild.
	RebuildBuffer int `env:"REBUILD_BUFFER" envDefault:"10000"`
	// SnapshotTTL is how long a cached depth snapshot stays acceptable to
	// the api role; the stream refreshes it far more often while it runs.
	SnapshotTTL time.Duration `env:"SNAPSHOT_TTL" envDefault:"10s"`
}

// EngineConfig tunes the trading engine (engine role) and the command bus
// that reaches it from a separate api role (docs/plan-v1.0.md §5.2).
type EngineConfig struct {
	QueueSize int `env:"QUEUE_SIZE" envDefault:"1024"` // commands waiting per market
	// BatchSize is how many queued commands a market's runner commits in
	// one transaction at most (group commit, docs/plan-v1.0.md §5.2). 1
	// commits every command on its own; the ceiling is trading.MaxBatchSize
	// because a failed group costs one order-book rebuild.
	BatchSize int `env:"BATCH_SIZE" envDefault:"50"`
	// CommandSubjectPrefix is the first tokens of cmd.trading.<tenant>.<market>.
	CommandSubjectPrefix string `env:"COMMAND_SUBJECT_PREFIX" envDefault:"cmd.trading"`
	// CommandTimeout bounds one request-reply round trip; exceeding it is a 503.
	CommandTimeout time.Duration `env:"COMMAND_TIMEOUT" envDefault:"5s"`
	// CommandMaxInFlight caps the bus commands handed to the engine at once
	// (cmdbus.ServerConfig.MaxInFlight). 0 means QueueSize: as many as one
	// market's queue can hold.
	CommandMaxInFlight int `env:"COMMAND_MAX_INFLIGHT" envDefault:"0"`
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

// AdminConfig configures the admin role: the static API key that machine
// integrations present (docs/plan-v1.0.md §14; scoped, HMAC-signed admin keys
// are not built yet), and the browser back office, where a person logs in
// with a password and a TOTP code and holds a server-side session.
type AdminConfig struct {
	APIKey            telemetry.Secret `env:"API_KEY"`
	BootstrapEmail    string           `env:"BOOTSTRAP_EMAIL"`
	BootstrapPassword telemetry.Secret `env:"BOOTSTRAP_PASSWORD"`
	// TOTPKey (ADMIN_TOTP_KEY, 32 bytes hex) seals administrators' TOTP
	// secrets at rest. Deliberately not API_KEY_MASTER_KEY: that key opens
	// every API-key secret, and the admin role has no reason to hold it.
	TOTPKey telemetry.Secret `env:"TOTP_KEY"`
	// TOTPKeyPrevious (ADMIN_TOTP_KEY_PREVIOUS) is TOTPKey's predecessor
	// during a rotation, until `exchange keys rewrap --domain totp`.
	TOTPKeyPrevious telemetry.Secret `env:"TOTP_KEY_PREVIOUS"`
	// SessionTTL is the life of a verified back-office session.
	SessionTTL time.Duration `env:"SESSION_TTL" envDefault:"8h"`
	// CookieSecure is "true", "false", or "" for "everywhere but dev". The
	// admin listener has no TLS of its own, so "when the connection is TLS"
	// would never fire; the flag says what the deployment in front of it does.
	CookieSecure string `env:"COOKIE_SECURE" envDefault:""`
}

// validate checks the stream settings; "*" origins are a dev convenience.
func (s StreamConfig) validate(env string) error {
	if s.WriteBuffer <= 0 || s.MaxMessageBytes <= 0 || s.ResumePageSize <= 0 || s.MaxSubscriptions <= 0 || s.DepthLevels <= 0 {
		return fmt.Errorf("config: STREAM_WRITE_BUFFER, STREAM_MAX_MESSAGE_BYTES, STREAM_RESUME_PAGE_SIZE, STREAM_MAX_SUBSCRIPTIONS and STREAM_DEPTH_LEVELS must be positive")
	}
	for _, d := range []time.Duration{s.PingInterval, s.PongTimeout, s.WriteTimeout, s.AuthTimeout, s.ResumeWindow, s.FlushInterval, s.TickerInterval, s.KlineInterval, s.SnapshotInterval} {
		if d <= 0 {
			return fmt.Errorf("config: every STREAM_* interval and timeout must be positive")
		}
	}
	for _, o := range s.AllowedOrigins {
		if o == "*" && env != "dev" {
			return fmt.Errorf("config: STREAM_ALLOWED_ORIGINS=* is only allowed when EXCHANGE_ENV=dev; list the front end's origins")
		}
	}
	return nil
}

// TOTPKeys decodes TOTPKey and its predecessor. Empty is not an error here:
// only the admin role needs it, and Run refuses to start that role without
// one.
func (a AdminConfig) TOTPKeys() (secretbox.Keyring, error) {
	return keyring("ADMIN_TOTP_KEY", a.TOTPKey, a.TOTPKeyPrevious)
}

// APIKeyKeys decodes API_KEY_MASTER_KEY and its predecessor.
func (c Config) APIKeyKeys() (secretbox.Keyring, error) {
	return keyring("API_KEY_MASTER_KEY", c.APIKeyMasterKey, c.APIKeyMasterKeyPrevious)
}

// keyring decodes a 32-byte hex master key and, when set, the one it
// replaced. The pair is validated as one: a previous key without a current
// one, or equal to it, is a rotation that has gone wrong in the
// configuration and is refused before any row is sealed under it.
func keyring(name string, current, previous telemetry.Secret) (secretbox.Keyring, error) {
	var k secretbox.Keyring
	var err error
	if current.IsSet() {
		if k.Current, err = hexKey(name, current); err != nil {
			return secretbox.Keyring{}, err
		}
	}
	if previous.IsSet() {
		if k.Previous, err = hexKey(name+"_PREVIOUS", previous); err != nil {
			return secretbox.Keyring{}, err
		}
	}
	if err := k.Validate(); err != nil {
		return secretbox.Keyring{}, fmt.Errorf("config: %s_PREVIOUS: %w", name, err)
	}
	return k, nil
}

func hexKey(name string, v telemetry.Secret) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(v.Reveal()))
	if err != nil || len(k) != secretbox.KeySize {
		return nil, fmt.Errorf("config: %s must be %d bytes hex-encoded", name, secretbox.KeySize)
	}
	return k, nil
}

// SecureCookies reports whether the session cookie carries the Secure flag.
func (c Config) SecureCookies() bool {
	switch strings.ToLower(c.Admin.CookieSecure) {
	case "true":
		return true
	case "false":
		return false
	}
	return c.Env != "dev"
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
	// database, so scanning begins just after it. Leaving it at 0 means "from
	// genesis", which is right for anvil and ruinous anywhere else: Sepolia's
	// head is past 11,000,000, and at ScanBatchSize 200 that is tens of
	// thousands of ticks before the first deposit could be seen.
	//
	// It is also the chain's anchor: the hash of this block is recorded once
	// and compared on every start, so changing it on a live database is
	// refused rather than silently redefining what "already scanned" means.
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
	// ReplaceAfter is how long a broadcast transaction may sit unmined before
	// it is re-sent with a higher fee (§6.4.2: anvil 60 s, Sepolia 3 min).
	ReplaceAfter time.Duration `env:"REPLACE_AFTER" envDefault:"60s"`
	// MaxReplacements caps the automatic fee bumps before an operator is
	// asked. Past it the withdrawal waits rather than bidding forever.
	MaxReplacements int32 `env:"MAX_REPLACEMENTS" envDefault:"3"`
	// MaxFeePerGas is the operator's stop-loss on the fee market, in wei.
	// Empty means no ceiling; a transaction that would exceed it waits, and
	// keeps waiting until the market comes back down or an operator raises
	// the ceiling. Nothing is sent at a clamped price (evm.Fees.Over).
	MaxFeePerGas string `env:"MAX_FEE_PER_GAS"`
	// NativeAsset is the registry symbol of the chain's own coin, which is
	// what every gas entry is denominated in. Stated once here so the
	// withdrawal worker and the sweeper cannot disagree about it and book two
	// halves of one transaction against different assets.
	NativeAsset string `env:"NATIVE_ASSET" envDefault:"ETH"`
	// SweepInterval is how often deposit addresses are checked for balances
	// worth collecting (§6.4.3: anvil 1 min). Much slower than the other two
	// clocks on purpose: sweeping is housekeeping, nobody is waiting for it,
	// and every scan costs one balance call per address per asset.
	SweepInterval time.Duration `env:"SWEEP_INTERVAL" envDefault:"60s"`
	// SweepBatchSize caps how many sweeps one tick advances.
	SweepBatchSize int32 `env:"SWEEP_BATCH_SIZE" envDefault:"25"`
	// SweepEnabled turns collection off. A deployment that has not decided
	// where its hot wallet lives is better off leaving deposits where they
	// landed than moving them somewhere it cannot spend from.
	SweepEnabled bool `env:"SWEEP_ENABLED" envDefault:"true"`
	// ReconcileInterval is how often ledger custody is compared with on-chain
	// balances (§6.4.4). The slowest clock in the role: a pass costs one
	// balance call per address per asset, and nothing downstream reacts within
	// minutes anyway.
	ReconcileInterval time.Duration `env:"RECONCILE_INTERVAL" envDefault:"5m"`
	// ReconcileEnabled turns the comparison off. Also turns off the booking of
	// nonce-fill gas, which rides the same pass.
	ReconcileEnabled bool `env:"RECONCILE_ENABLED" envDefault:"true"`
	// HotWalletMin is the balance below which alert.hot_wallet_low fires, in
	// NativeAsset. Empty or zero disables the alert.
	//
	// §6.4.3 spells it HOT_WALLET_MIN_ETH. The leaf name here has no "ETH" in
	// it because the denomination is whatever ETH_NATIVE_ASSET says, and a
	// setting that names one coin while meaning another is a trap on the first
	// chain that is not Ethereum.
	HotWalletMin string `env:"HOT_WALLET_MIN" envDefault:"0"`
}

// MinHotWallet parses HotWalletMin. Zero means the low-balance alert is off.
func (c ChainConfig) MinHotWallet() (money.Amount, error) {
	s := strings.TrimSpace(c.HotWalletMin)
	if s == "" {
		return money.Zero, nil
	}
	v, err := money.ParseAmount(s)
	if err != nil {
		return money.Zero, fmt.Errorf("config: ETH_HOT_WALLET_MIN %q is not a decimal amount", c.HotWalletMin)
	}
	if v.IsNegative() {
		return money.Zero, fmt.Errorf("config: ETH_HOT_WALLET_MIN %q must not be negative", c.HotWalletMin)
	}
	return v, nil
}

// MaxFee parses MaxFeePerGas. An unset ceiling is nil, which every caller
// reads as "no limit".
func (c ChainConfig) MaxFee() (*big.Int, error) {
	if strings.TrimSpace(c.MaxFeePerGas) == "" {
		return nil, nil //nolint:nilnil // "no ceiling" is a value, not an error
	}
	v, ok := new(big.Int).SetString(strings.TrimSpace(c.MaxFeePerGas), 10)
	if !ok || v.Sign() < 0 {
		return nil, fmt.Errorf("config: ETH_MAX_FEE_PER_GAS %q is not a non-negative wei amount", c.MaxFeePerGas)
	}
	return v, nil
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
	// SignerSubjectPrefix is the first tokens of the signer's NATS subject;
	// the full subject is <prefix>.<tenant> (docs/plan-v1.0.md §5.1).
	SignerSubjectPrefix string `env:"SIGNER_SUBJECT_PREFIX" envDefault:"cmd.signer"`
	// SignerTimeout bounds one signing round trip. Signing is CPU-cheap; the
	// budget is for the signer being busy or restarting, not for the maths.
	SignerTimeout time.Duration `env:"SIGNER_TIMEOUT" envDefault:"10s"`
}

// JWTConfig locates the signing key (api role only) and the JWKS URL.
type JWTConfig struct {
	PrivateKeyFile string `env:"PRIVATE_KEY_FILE"`
	// PreviousKeyFile (JWT_PREVIOUS_KEY_FILE) names a second key whose
	// public half is published in the JWKS but never signs: the key just
	// rotated out, or the one about to rotate in (a PKCS#8 private key or
	// a SPKI public key PEM; docs/runbooks/key-rotation.md).
	PreviousKeyFile string `env:"PREVIOUS_KEY_FILE"`
	JWKSURL         string `env:"JWKS_URL"`
}

// ShutdownConfig controls graceful shutdown.
type ShutdownConfig struct {
	DrainDelay time.Duration `env:"DRAIN_DELAY" envDefault:"2s"`
	Timeout    time.Duration `env:"TIMEOUT" envDefault:"20s"`
}

// ValidateFor checks what depends on which roles the process runs: the
// keystore passphrase belongs to the signer alone (docs/plan-v1.0.md §14),
// so a process without one that was handed it refuses to start rather
// than carry a secret it has no use for.
func (c Config) ValidateFor(roles []Role) error {
	if !c.Wallet.Passphrase.IsSet() {
		return nil
	}
	for _, r := range roles {
		if r == RoleSigner || r == RoleAll {
			return nil
		}
	}
	return fmt.Errorf("config: WALLET_KEYSTORE_PASSPHRASE is set but roles %s include no signer; only the signer holds the keystore secret", RolesLabel(roles))
}

// secretsWithFileVariant lists variables that may be supplied as NAME_FILE.
var secretsWithFileVariant = []string{
	"DATABASE_URL",
	// NATS_URL is here because a production URL carries the login in its
	// userinfo (nats://user:password@host), which nats.go reads.
	"NATS_URL",
	"REDIS_PASSWORD",
	"WALLET_KEYSTORE_PASSPHRASE",
	"WALLET_KEYSTORE_NEW_PASSPHRASE",
	"WEBHOOK_SIGNING_KEY",
	"WEBHOOK_SIGNING_KEY_PREVIOUS",
	"ADMIN_BOOTSTRAP_PASSWORD",
	"ADMIN_API_KEY",
	"ADMIN_TOTP_KEY",
	"ADMIN_TOTP_KEY_PREVIOUS",
	"API_KEY_MASTER_KEY",
	"API_KEY_MASTER_KEY_PREVIOUS",
}

// ExpandSecretEnv resolves every NAME_FILE variant into NAME, for commands
// that read a secret from the environment without loading the whole
// configuration (`exchange keys rekey`).
func ExpandSecretEnv() error { return expandFileEnv(secretsWithFileVariant) }

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

// WebhookConfig is outbound delivery (docs/plan-v1.0.md §7.6).
type WebhookConfig struct {
	// Backoff is the wait before each retry, and its length is the attempt
	// budget: running off the end is what marks a delivery dead. §7.6 fixes
	// the default; integration tests inject a short one.
	Backoff []time.Duration `env:"BACKOFF" envDefault:"1m,5m,30m,2h,12h,24h"`
	// SigningKey (WEBHOOK_SIGNING_KEY, 32 bytes hex) encrypts each endpoint's
	// signing secret at rest. A different key from API_KEY_MASTER_KEY on
	// purpose: one master key per secret domain, so rotating webhook secrets
	// never touches API keys.
	SigningKey telemetry.Secret `env:"SIGNING_KEY"`
	// SigningKeyPrevious (WEBHOOK_SIGNING_KEY_PREVIOUS) is SigningKey's
	// predecessor during a rotation, until `exchange keys rewrap --domain
	// webhook`.
	SigningKeyPrevious telemetry.Secret `env:"SIGNING_KEY_PREVIOUS"`
	// Timeout bounds one POST to a customer.
	Timeout time.Duration `env:"TIMEOUT" envDefault:"10s"`
	// Interval is how often the queue is drained. It is not the retry
	// schedule -- a delivery due in five minutes is simply not claimed until
	// then -- so it only bounds how late a due delivery goes out.
	Interval  time.Duration `env:"INTERVAL" envDefault:"5s"`
	BatchSize int32         `env:"BATCH_SIZE" envDefault:"50"`
}

// Keys decodes SigningKey and its predecessor. An empty key is not an error
// here: a deployment with no webhook endpoints has no use for one, so the
// worker warns rather than refusing to start.
func (w WebhookConfig) Keys() (secretbox.Keyring, error) {
	return keyring("WEBHOOK_SIGNING_KEY", w.SigningKey, w.SigningKeyPrevious)
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
	if c.Engine.CommandMaxInFlight < 0 {
		return fmt.Errorf("config: ENGINE_COMMAND_MAX_INFLIGHT must not be negative")
	}
	if c.Engine.BatchSize < 1 || c.Engine.BatchSize > trading.MaxBatchSize {
		return fmt.Errorf("config: ENGINE_BATCH_SIZE must be 1..%d", trading.MaxBatchSize)
	}
	if c.Outbox.PollInterval <= 0 || c.Outbox.BatchSize <= 0 {
		return fmt.Errorf("config: OUTBOX_POLL_INTERVAL and OUTBOX_BATCH_SIZE must be positive")
	}
	if c.Shutdown.Timeout <= 0 || c.Shutdown.DrainDelay < 0 {
		return fmt.Errorf("config: SHUTDOWN_TIMEOUT must be positive and SHUTDOWN_DRAIN_DELAY non-negative")
	}
	// A previous key only means something next to the current one it
	// replaced; keyring() refuses one without the other or equal to it.
	if c.JWT.PreviousKeyFile != "" && c.JWT.PrivateKeyFile == "" {
		return fmt.Errorf("config: JWT_PREVIOUS_KEY_FILE is set without JWT_PRIVATE_KEY_FILE")
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
	if c.Chain.SweepInterval <= 0 {
		return fmt.Errorf("config: ETH_SWEEP_INTERVAL must be positive")
	}
	if c.Chain.ReconcileInterval <= 0 {
		return fmt.Errorf("config: ETH_RECONCILE_INTERVAL must be positive")
	}
	if len(c.Webhook.Backoff) == 0 {
		// An empty schedule would make the first failure the last one, which
		// is not a policy anybody chooses deliberately.
		return fmt.Errorf("config: WEBHOOK_BACKOFF must list at least one delay")
	}
	for _, d := range c.Webhook.Backoff {
		if d <= 0 {
			return fmt.Errorf("config: every WEBHOOK_BACKOFF delay must be positive, got %s", d)
		}
	}
	if c.Webhook.Interval <= 0 || c.Webhook.Timeout <= 0 {
		return fmt.Errorf("config: WEBHOOK_INTERVAL and WEBHOOK_TIMEOUT must be positive")
	}
	if c.Webhook.BatchSize <= 0 {
		return fmt.Errorf("config: WEBHOOK_BATCH_SIZE must be positive")
	}
	if c.Retention.Interval <= 0 || c.Retention.Outbox <= 0 || c.Retention.Webhook <= 0 || c.Retention.BatchSize <= 0 {
		return fmt.Errorf("config: RETENTION_INTERVAL, RETENTION_OUTBOX, RETENTION_WEBHOOK and RETENTION_BATCH_SIZE must be positive")
	}
	if c.MarketData.KlinePollInterval <= 0 || c.MarketData.KlineBatchSeqs <= 0 || c.MarketData.RebuildBuffer <= 0 || c.MarketData.SnapshotTTL <= 0 {
		return fmt.Errorf("config: MARKETDATA_KLINE_POLL_INTERVAL, MARKETDATA_KLINE_BATCH_SEQS, MARKETDATA_REBUILD_BUFFER and MARKETDATA_SNAPSHOT_TTL must be positive")
	}
	if err := c.Stream.validate(c.Env); err != nil {
		return err
	}
	for _, decode := range []func() (secretbox.Keyring, error){c.Webhook.Keys, c.Admin.TOTPKeys, c.APIKeyKeys} {
		if _, err := decode(); err != nil {
			return err
		}
	}
	if c.Admin.SessionTTL <= 0 {
		return fmt.Errorf("config: ADMIN_SESSION_TTL must be positive")
	}
	switch strings.ToLower(c.Admin.CookieSecure) {
	case "", "true", "false":
	default:
		return fmt.Errorf("config: ADMIN_COOKIE_SECURE must be true, false or empty")
	}
	if _, err := c.Chain.MinHotWallet(); err != nil {
		return err
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
		slog.String("nats_url", telemetry.RedactURL(c.NATS.URL)),
		slog.String("redis_addr", c.Redis.Addr),
		slog.String("eth_rpc_url", telemetry.RedactEndpoint(c.Chain.RPCURL)),
		slog.Int64("eth_chain_id", c.Chain.ChainID),
		slog.Duration("eth_scan_interval", c.Chain.ScanInterval),
		slog.Duration("eth_withdrawal_interval", c.Chain.WithdrawalInterval),
		slog.Duration("eth_sweep_interval", c.Chain.SweepInterval),
		slog.Duration("eth_reconcile_interval", c.Chain.ReconcileInterval),
		slog.Bool("eth_reconcile_enabled", c.Chain.ReconcileEnabled),
		slog.String("eth_hot_wallet_min", c.Chain.HotWalletMin),
		slog.Bool("eth_sweep_enabled", c.Chain.SweepEnabled),
		slog.Duration("eth_replace_after", c.Chain.ReplaceAfter),
		slog.Int("eth_max_replacements", int(c.Chain.MaxReplacements)),
		slog.String("eth_max_fee_per_gas", c.Chain.MaxFeePerGas),
		slog.Uint64("eth_scan_batch_size", c.Chain.ScanBatchSize),
		slog.Uint64("eth_block_ring_depth", c.Chain.BlockRingDepth),
		slog.Uint64("eth_orphan_expiry_blocks", c.Chain.OrphanExpiryBlocks),
		slog.Int("eth_required_confirmations_default", int(c.Chain.RequiredConfirmations)),
		slog.String("wallet_keystore_dir", c.Wallet.KeystoreDir),
		slog.Bool("wallet_keystore_passphrase_set", c.Wallet.Passphrase.IsSet()),
		slog.Int("wallet_address_pool_min", c.Wallet.AddressPoolMin),
		slog.Duration("wallet_address_pool_interval", c.Wallet.AddressPoolInterval),
		slog.String("jwt_private_key_file", c.JWT.PrivateKeyFile),
		slog.String("jwt_previous_key_file", c.JWT.PreviousKeyFile),
		slog.String("jwt_jwks_url", c.JWT.JWKSURL),
		slog.String("auth_issuer", c.Auth.Issuer),
		slog.Duration("auth_access_ttl", c.Auth.AccessTTL),
		slog.Duration("auth_refresh_ttl", c.Auth.RefreshTTL),
		slog.Bool("api_key_master_key_set", c.APIKeyMasterKey.IsSet()),
		slog.Bool("api_key_master_key_previous_set", c.APIKeyMasterKeyPrevious.IsSet()),
		slog.Bool("admin_totp_key_set", c.Admin.TOTPKey.IsSet()),
		slog.Bool("admin_totp_key_previous_set", c.Admin.TOTPKeyPrevious.IsSet()),
		slog.Duration("admin_session_ttl", c.Admin.SessionTTL),
		slog.Bool("admin_cookie_secure", c.SecureCookies()),
		slog.Bool("webhook_signing_key_set", c.Webhook.SigningKey.IsSet()),
		slog.Bool("webhook_signing_key_previous_set", c.Webhook.SigningKeyPrevious.IsSet()),
		slog.Int("webhook_attempts", len(c.Webhook.Backoff)),
		slog.Duration("marketdata_kline_poll_interval", c.MarketData.KlinePollInterval),
		slog.Int64("marketdata_kline_batch_seqs", c.MarketData.KlineBatchSeqs),
		slog.Int("marketdata_rebuild_buffer", c.MarketData.RebuildBuffer),
		slog.Duration("marketdata_snapshot_ttl", c.MarketData.SnapshotTTL),
		slog.Duration("retention_outbox", c.Retention.Outbox),
		slog.Duration("retention_webhook", c.Retention.Webhook),
		slog.Int("stream_write_buffer", c.Stream.WriteBuffer),
		slog.Duration("stream_ping_interval", c.Stream.PingInterval),
		slog.Duration("stream_resume_window", c.Stream.ResumeWindow),
		slog.Any("stream_allowed_origins", c.Stream.AllowedOrigins),
		slog.String("ratelimit_login_per_ip", c.RateLimit.LoginPerIP),
		slog.String("ratelimit_login_per_account", c.RateLimit.LoginPerAccount),
		slog.String("ratelimit_orders_per_account", c.RateLimit.OrdersPerAccount),
		slog.Int("engine_queue_size", c.Engine.QueueSize),
		slog.Int("engine_command_max_inflight", c.Engine.CommandMaxInFlight),
		slog.Int("engine_batch_size", c.Engine.BatchSize),
		slog.Duration("outbox_poll_interval", c.Outbox.PollInterval),
		slog.Duration("shutdown_drain_delay", c.Shutdown.DrainDelay),
		slog.Duration("shutdown_timeout", c.Shutdown.Timeout),
		slog.Bool("otel_endpoint_set", c.OTLPEndpoint != ""),
	)
}

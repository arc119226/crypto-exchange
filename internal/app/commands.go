package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/migrations"
)

// ErrNotImplemented is returned by subcommands whose phase has not started.
var ErrNotImplemented = errors.New("not implemented yet (see docs/plan-v1.0.md §12 for the phase that adds it)")

const pgUndefinedObject = "42704"

// openSQL opens a database/sql handle (goose needs one) via the pgx stdlib driver.
func openSQL(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("migrate: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: connect: %w", err)
	}
	return db, nil
}

func newGooseProvider(db *sql.DB) (*goose.Provider, error) {
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return nil, fmt.Errorf("migrate: provider: %w", err)
	}
	return p, nil
}

// MigrateUp applies every pending migration (idempotent). Run it with the
// ex_migrate role. Missing login roles surface as a hint instead of a raw
// 42704 error.
func MigrateUp(ctx context.Context, dsn string, out io.Writer) error {
	db, err := openSQL(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	p, err := newGooseProvider(db)
	if err != nil {
		return err
	}
	results, err := p.Up(ctx)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUndefinedObject {
			return fmt.Errorf("migrate: %w\nhint: the ex_* login roles do not exist yet; run infra/postgres/initdb/01-roles.sh (or scripts/db-roles.sql) against this database first", err)
		}
		return fmt.Errorf("migrate: %w", err)
	}
	if len(results) == 0 {
		_, _ = fmt.Fprintln(out, "migrate: no pending migrations")
	}
	for _, r := range results {
		_, _ = fmt.Fprintf(out, "migrate: applied %s (%s)\n", r.Source.Path, r.Duration.Round(time.Millisecond))
	}
	return nil
}

// MigrateStatus prints applied and pending migrations.
func MigrateStatus(ctx context.Context, dsn string, out io.Writer) error {
	db, err := openSQL(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	p, err := newGooseProvider(db)
	if err != nil {
		return err
	}
	statuses, err := p.Status(ctx)
	if err != nil {
		return fmt.Errorf("migrate: status: %w", err)
	}
	for _, st := range statuses {
		applied := "pending"
		if st.State == goose.StateApplied {
			applied = st.AppliedAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(out, "%-8s %-24s %s\n", st.State, applied, st.Source.Path)
	}
	return nil
}

// SeedOptions parameterise Seed.
type SeedOptions struct {
	DSN                   string
	FixturesPath          string
	TenantID              string
	ChainID               int64
	RequiredConfirmations int32
}

// Seed upserts the registry fixtures (fee schedule, ETH, USDC, ETH-USDC).
// Run it with the ex_admin role.
func Seed(ctx context.Context, opts SeedOptions, out io.Writer) error {
	fx, err := registry.LoadFixtures(opts.FixturesPath)
	if err != nil {
		return err
	}
	if err := fx.Validate(opts.ChainID); err != nil {
		return err
	}
	pool, err := pg.Open(ctx, pg.PoolConfig{DSN: opts.DSN, MaxConns: 2, ApplicationName: "exchange-seed"})
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	defer pool.Close()
	res, err := registry.Seed(ctx, pool, fx, registry.SeedOptions{TenantID: opts.TenantID, RequiredConfirmations: opts.RequiredConfirmations})
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	_, _ = fmt.Fprintf(out, "seed: tenant=%s chain=%d fee_schedules=%d assets=%d markets=%d (usdc=%s)\n",
		opts.TenantID, fx.ChainID, res.FeeSchedules, res.Assets, res.Markets, fx.USDC)
	return nil
}

// BootstrapAdmin creates the first administrator from ADMIN_BOOTSTRAP_EMAIL /
// ADMIN_BOOTSTRAP_PASSWORD (idempotent). Run it with the ex_admin role
// against a migrated database.
func BootstrapAdmin(ctx context.Context, cfg Config, out io.Writer) error {
	if cfg.Admin.BootstrapEmail == "" || !cfg.Admin.BootstrapPassword.IsSet() {
		return fmt.Errorf("admin bootstrap: ADMIN_BOOTSTRAP_EMAIL and ADMIN_BOOTSTRAP_PASSWORD are required")
	}
	pool, err := pg.Open(ctx, pg.PoolConfig{DSN: cfg.DB.URL.Reveal(), MaxConns: 2, ApplicationName: "exchange-bootstrap"})
	if err != nil {
		return fmt.Errorf("admin bootstrap: %w", err)
	}
	defer pool.Close()
	l := ledger.New(pool, cfg.TenantID)
	svc, err := auth.New(pool, auth.Config{Tenant: cfg.TenantID, Issuer: cfg.Auth.Issuer}, nil, nil, l, audit.NewRecorder(cfg.TenantID))
	if err != nil {
		return err
	}
	u, created, err := svc.BootstrapAdmin(ctx, cfg.Admin.BootstrapEmail, cfg.Admin.BootstrapPassword.Reveal())
	if err != nil {
		return fmt.Errorf("admin bootstrap: %w", err)
	}
	if created {
		_, _ = fmt.Fprintf(out, "admin bootstrap: created %s (user %s)\n", u.Email, u.ID)
	} else {
		_, _ = fmt.Fprintf(out, "admin bootstrap: %s already exists (user %s, role %s); nothing changed\n", u.Email, u.ID, u.Role)
	}
	return nil
}

// Healthcheck performs a GET and succeeds on any 2xx. It is the container
// HEALTHCHECK command (distroless has no shell or curl).
func Healthcheck(ctx context.Context, url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("healthcheck: %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	return nil
}

// ImportMnemonicOptions configures the HD seed import (docs/plan-v1.0.md
// §14, ADR-0007).
type ImportMnemonicOptions struct {
	// From is the file holding the BIP-39 mnemonic, one line.
	From string
	// KeystoreDir receives hd-seed.json.
	KeystoreDir string
	// Passphrase encrypts it; normally WALLET_KEYSTORE_PASSPHRASE.
	Passphrase string
	// Force overwrites an existing keystore. Without it the command refuses,
	// because replacing the seed orphans every address already handed out.
	Force bool
	// Scrypt overrides the KDF work factors. The zero value means
	// hdwallet.DefaultScrypt; tests lower it so `make test` does not pay
	// 256 MiB and a second of scrypt under -race.
	Scrypt hdwallet.ScryptParams
}

// ImportMnemonic encrypts a BIP-39 mnemonic into the HD seed keystore and
// returns the path plus the hot wallet address it derives.
//
// The address is returned so the caller can check it: scripts/gen-dev-secrets.sh
// computes the same m/44'/60'/1'/0/0 with `cast wallet address` and writes it
// to HOT_WALLET_ADDRESS, so a mismatch means this import read a different
// mnemonic than the rest of the stack expects.
func ImportMnemonic(opts ImportMnemonicOptions) (path, hotWallet string, err error) {
	if opts.From == "" || opts.KeystoreDir == "" {
		return "", "", errors.New("keys: --from and --keystore-dir are required")
	}
	if opts.Passphrase == "" {
		return "", "", errors.New("keys: WALLET_KEYSTORE_PASSPHRASE is required to encrypt the seed")
	}
	target := filepath.Join(opts.KeystoreDir, hdwallet.SeedFileName)
	if _, err := os.Stat(target); err == nil && !opts.Force {
		return "", "", fmt.Errorf("keys: %s already exists (use --force to replace the seed, which orphans every address already handed out)", target)
	}
	raw, err := os.ReadFile(opts.From) //nolint:gosec // an operator-supplied path
	if err != nil {
		return "", "", fmt.Errorf("keys: read mnemonic: %w", err)
	}
	mnemonic := strings.Join(strings.Fields(string(raw)), " ")

	// Derive before writing: an unusable seed should fail here, not at the
	// signer's next start.
	w, err := hdwallet.FromMnemonic(mnemonic)
	if err != nil {
		return "", "", err
	}
	defer w.Close()
	addr, err := w.Address(hdwallet.HotWalletPath())
	if err != nil {
		return "", "", err
	}
	kdf := opts.Scrypt
	if kdf.N == 0 {
		kdf = hdwallet.DefaultScrypt()
	}
	path, err = hdwallet.Save(opts.KeystoreDir, mnemonic, opts.Passphrase, kdf)
	if err != nil {
		return "", "", err
	}
	return path, addr.Hex(), nil
}

// GenerateJWTKey writes a new Ed25519 private key as PKCS#8 PEM with mode
// 0600. It refuses to overwrite unless force is set.
func GenerateJWTKey(path string, force bool) error {
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("keys: %s already exists (use --force to overwrite)", path)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("keys: generate: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("keys: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("keys: mkdir: %w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		return fmt.Errorf("keys: write: %w", err)
	}
	return nil
}

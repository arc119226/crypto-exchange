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
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"

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

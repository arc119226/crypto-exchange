package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ErrNotImplemented is returned by subcommands whose phase has not started.
var ErrNotImplemented = errors.New("not implemented yet (see docs/plan-v1.0.md §12 for the phase that adds it)")

// MigrateUp applies pending goose migrations. Added in the registry batch.
func MigrateUp(context.Context, string) error { return ErrNotImplemented }

// MigrateStatus prints migration status. Added in the registry batch.
func MigrateStatus(context.Context, string, io.Writer) error { return ErrNotImplemented }

// SeedOptions parameterise Seed.
type SeedOptions struct {
	DSN          string
	FixturesPath string
	TenantID     string
	ChainID      int64
}

// Seed upserts the registry fixtures. Added in the registry batch.
func Seed(context.Context, SeedOptions) error { return ErrNotImplemented }

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

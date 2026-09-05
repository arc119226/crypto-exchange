//go:build integration

// Package integration holds tests that need real infrastructure via
// testcontainers. Build tag `integration` keeps them out of `make test`;
// without a Docker daemon they skip locally and fail in CI (CI=true).
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const pgPassword = "test"

type pgHarness struct {
	host string
	port string
}

// DSN returns a connection string for one of the ex_* roles.
func (h pgHarness) DSN(role string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/exchange?sslmode=disable", role, pgPassword, h.host, h.port)
}

// startPostgres runs postgres:16.4 with the role init script. When Docker is
// unavailable it skips (local) or fails (CI) so CI can never pass by skipping.
func startPostgres(t *testing.T) pgHarness {
	t.Helper()
	ctx := context.Background()
	script, err := filepath.Abs("../../infra/postgres/initdb/01-roles.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := postgres.Run(ctx, "postgres:16.4",
		postgres.WithDatabase("exchange"),
		postgres.WithUsername("exchange"),
		postgres.WithPassword(pgPassword),
		postgres.WithInitScripts(script),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		if os.Getenv("CI") == "" {
			t.Skipf("docker not available, skipping integration test: %v", err)
		}
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate postgres: %v", err)
		}
	})
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return pgHarness{host: host, port: port.Port()}
}

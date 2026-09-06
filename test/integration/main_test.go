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
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// natsImage matches deploy/compose/compose.yaml.
const natsImage = "nats:2.10.22-alpine"

// startNATS returns the URL of a JetStream-enabled NATS server.
//
// Default: a testcontainer. TEST_NATS_URL=nats://host:port uses a running
// server instead (start one with `nats-server -js`); tests purge the streams
// they use, so a shared local server is fine.
func startNATS(t *testing.T) string {
	t.Helper()
	if url := os.Getenv("TEST_NATS_URL"); url != "" {
		return url
	}
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, natsImage, tcnats.WithArgument("js", ""))
	if err != nil {
		if os.Getenv("CI") == "" {
			t.Skipf("docker not available, skipping NATS integration test: %v", err)
		}
		t.Fatalf("start nats: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("terminate nats: %v", err)
		}
	})
	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return url
}

const pgPassword = "test"

type pgHarness struct {
	host string
	port string
	db   string
}

// DSN returns a connection string for one of the ex_* roles (or the
// `exchange` superuser).
func (h pgHarness) DSN(role string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", role, pgPassword, h.host, h.port, h.db)
}

var localDBSeq atomic.Int64

// startLocalDatabase creates a throwaway database on an existing cluster
// and hands database ownership to ex_migrate exactly like 01-roles.sh.
func startLocalDatabase(t *testing.T, ctx context.Context, adminURL string) pgHarness {
	t.Helper()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("TEST_PG_ADMIN_URL: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	name := fmt.Sprintf("exchange_it_%d_%d", os.Getpid(), localDBSeq.Add(1))
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q OWNER exchange`, name)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
	})
	for _, role := range []string{"ex_migrate", "ex_api", "ex_engine", "ex_chain", "ex_signer", "ex_stream", "ex_admin", "ex_worker", "ex_all"} {
		if _, err := admin.Exec(ctx, fmt.Sprintf(`GRANT CONNECT ON DATABASE %q TO %s`, name, role)); err != nil {
			t.Fatalf("grant connect %s: %v", role, err)
		}
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %q OWNER TO ex_migrate`, name)); err != nil {
		t.Fatalf("alter owner: %v", err)
	}
	cfg := admin.Config()
	return pgHarness{host: cfg.Host, port: fmt.Sprint(cfg.Port), db: name}
}

// startPostgres returns a fresh database with the ex_* roles.
//
// Default: a postgres:16.4 testcontainer running the role init script. When
// Docker is unavailable it skips (local) or fails (CI) so CI can never pass
// by skipping.
//
// TEST_PG_ADMIN_URL=postgres://<superuser>:<pw>@host:port/postgres: use a
// running cluster instead (no Docker needed). The roles must already exist
// (run infra/postgres/initdb/01-roles.sh once against that cluster with
// POSTGRES_PASSWORD=test); every test gets its own CREATE DATABASE, dropped
// on cleanup.
func startPostgres(t *testing.T) pgHarness {
	t.Helper()
	ctx := context.Background()
	if admin := os.Getenv("TEST_PG_ADMIN_URL"); admin != "" {
		return startLocalDatabase(t, ctx, admin)
	}
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
	return pgHarness{host: host, port: port.Port(), db: "exchange"}
}

package main

import (
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/app"
)

func dsnFromEnv() (string, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		if path := os.Getenv("DATABASE_URL_FILE"); path != "" {
			b, err := os.ReadFile(path) //nolint:gosec // operator-provided path
			if err != nil {
				return "", err
			}
			dsn = string(b)
		}
	}
	if dsn == "" {
		return "", errors.New("DATABASE_URL (or DATABASE_URL_FILE) must be set")
	}
	return dsn, nil
}

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Run or inspect database migrations (uses DATABASE_URL, normally the ex_migrate role)",
	}
	var wait time.Duration
	up := &cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dsn, err := dsnFromEnv()
			if err != nil {
				return runtimeErr(err)
			}
			return runtimeErr(app.MigrateUp(cmd.Context(), dsn, cmd.OutOrStdout(), wait))
		},
	}
	// A migrate Job that starts with its database (a Helm pre-install hook
	// on a fresh cluster, compose without depends_on) would otherwise fail
	// and restart with the Job's backoff; retrying the connection here is
	// cheaper and reads better in the logs.
	up.Flags().DurationVar(&wait, "wait", 0, "keep retrying the connection for this long before giving up (e.g. 5m)")
	cmd.AddCommand(up, &cobra.Command{
		Use:   "status",
		Short: "Show applied and pending migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dsn, err := dsnFromEnv()
			if err != nil {
				return runtimeErr(err)
			}
			return runtimeErr(app.MigrateStatus(cmd.Context(), dsn, cmd.OutOrStdout()))
		},
	})
	return cmd
}

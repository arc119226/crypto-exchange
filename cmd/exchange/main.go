// Command exchange is the single binary of the white-label exchange engine.
// `exchange serve --role=...` runs one or more roles; the other subcommands
// are operational helpers (migrate, seed, healthcheck, keys, admin).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/internal/app"
)

// Set with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Exit codes.
const (
	exitOK             = 0
	exitRuntime        = 1
	exitUsage          = 2
	exitNotImplemented = 3
)

// exitError carries an explicit exit code out of a RunE.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// runtimeErr wraps an error from an executed command (as opposed to a usage
// error raised by cobra before the command ran).
func runtimeErr(err error) error {
	if err == nil {
		return nil
	}
	code := exitRuntime
	if errors.Is(err, app.ErrNotImplemented) {
		code = exitNotImplemented
	}
	return &exitError{code: code, err: err}
}

func buildInfo() app.BuildInfo { return app.BuildInfo{Version: version, Commit: commit, Date: date} }

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "exchange",
		Short:         "White-label exchange engine (single binary, multiple roles)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd(), newVersionCmd(), newMigrateCmd(), newSeedCmd(), newHealthcheckCmd(), newKeysCmd(), newAdminCmd())
	return root
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := newRootCmd().ExecuteContext(ctx)
	os.Exit(exitCode(err))
}

func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, context.Canceled):
		return exitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		fmt.Fprintln(os.Stderr, "error:", ee.err)
		return ee.code
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return exitUsage
}

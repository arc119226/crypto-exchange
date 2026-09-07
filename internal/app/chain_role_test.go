package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A chain role that has not finished starting must answer /readyz with the
// reason. Until 4d-2b it could not answer at all: newChain did the waiting
// before the ops server was built, so nothing was listening and the operator
// got a refused connection rather than a 503. The nil client here stands for
// that -- ready must reach its verdict without dialling anything.
func TestReadyzReportsWhyTheChainRoleIsStillStarting(t *testing.T) {
	c := &chainComponents{started: make(chan struct{})}
	c.setStartErr(errors.New("waiting for the signer to name its hot wallet"))

	checker := NewChecker()
	checker.Register("chain", true, c.ready)
	code, body, raw := get(t, newOpsMux(checker, prometheus.NewRegistry(), false), "/readyz")

	assert.Equal(t, http.StatusServiceUnavailable, code, raw)
	assert.Equal(t, "unavailable", body["status"])
	chain := body["checks"].(map[string]any)["chain"].(map[string]any)
	assert.Equal(t, false, chain["ok"])
	assert.Equal(t, "still starting: waiting for the signer to name its hot wallet", chain["err"],
		"a bool could only say 'not ready'; the operator needs to know which half is waiting")
}

// The reason is live, not a fixed string: whichever half of start is waiting
// has to be the half /readyz names.
func TestStartingReportsTheCurrentReason(t *testing.T) {
	c := &chainComponents{started: make(chan struct{})}

	c.setStartErr(errors.New("connecting to the node and verifying the chain"))
	require.ErrorContains(t, c.starting(), "connecting to the node")

	c.setStartErr(errors.New("waiting for the signer to name its hot wallet"))
	require.ErrorContains(t, c.starting(), "the signer")

	c.setStartErr(nil)
	assert.NoError(t, c.starting(), "and says nothing once the role is up")
}

// start closes the gate the tick loops wait on, and reports the failure that
// must take the process down (§6.4.1 step 5).
func TestStartOpensTheGateOnlyOnSuccess(t *testing.T) {
	fatal := errors.New("the node is on a different chain")
	failed := &chainComponents{started: make(chan struct{})}
	failed.bringUp = func(context.Context) error { return fatal }
	require.ErrorIs(t, failed.start(t.Context()), fatal)
	assert.ErrorContains(t, failed.starting(), fatal.Error(), "and /readyz says so")
	select {
	case <-failed.started:
		t.Fatal("a role that did not come up must not release the tick loops")
	default:
	}

	ok := &chainComponents{started: make(chan struct{})}
	ok.bringUp = func(context.Context) error { return nil }
	require.NoError(t, ok.start(t.Context()))
	assert.NoError(t, ok.starting())
	assert.True(t, ok.awaitStart(t.Context()))
}

// The tick loops must wait for start rather than read their worker while it
// is still being assigned. A nil reconciler with the gate shut is the case
// that used to return immediately: reconciliation would have been read as
// "off" in a deployment that has it on.
func TestTickLoopsWaitForStartBeforeReadingTheirWorker(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &chainComponents{started: make(chan struct{}), reconcileInterval: time.Hour}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- c.runReconciling(ctx, log) }()

	select {
	case <-done:
		t.Fatal("runReconciling returned before start: it read c.reconciler while start was still assigning it")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "a cancelled start is a clean stop, not a failure")
	case <-time.After(2 * time.Second):
		t.Fatal("runReconciling did not stop when the context ended")
	}
}

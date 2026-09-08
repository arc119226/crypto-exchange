package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// The sink is what a customer points at while they are writing the real
// receiver, so the thing that has to be true is that it agrees with the
// exchange: a delivery the dispatcher signed passes, and anything else does
// not. It verifies with the same code the dispatcher signs with, which is why
// .golangci.yml lets cmd import internal/webhook.
func TestWebhookSinkAcceptsWhatTheDispatcherSignsAndNothingElse(t *testing.T) {
	const secret = "whsec_0123456789abcdef"
	body := []byte(`{"event_id":"ev-1","event_type":"trade.executed","payload":{"qty":"0.5"}}`)
	now := time.Now()
	sig, ts := webhook.Sign(secret, body, now)

	tamperedSig := "v1=" + strings.Repeat("0", 64)
	oldTS := strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10)

	cases := []struct {
		name string
		sig  string
		ts   string
		body []byte
		want int
	}{
		{"as signed", sig, ts, body, http.StatusOK},
		{"body changed by one byte", sig, ts, append(bytes.TrimSuffix(body, []byte("}")), []byte(` }`)...), http.StatusUnauthorized},
		{"signature changed", tamperedSig, ts, body, http.StatusUnauthorized},
		{"timestamp outside the tolerance", sig, oldTS, body, http.StatusUnauthorized},
		{"timestamp changed to match nothing", sig, ts + "1", body, http.StatusUnauthorized},
		{"no signature at all", "", ts, body, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, srv := startSink(t, secret, time.Minute)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv, bytes.NewReader(tc.body))
			require.NoError(t, err)
			req.Header.Set(webhook.SignatureHeader, tc.sig)
			req.Header.Set(webhook.TimestampHeader, tc.ts)
			req.Header.Set(webhook.EventIDHeader, "ev-1")
			req.Header.Set(webhook.EventTypeHeader, "trade.executed")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, tc.want, resp.StatusCode)

			printed := out.String()
			assert.Contains(t, printed, "ev-1", "the operator sees which event arrived either way")
			if tc.want == http.StatusOK {
				assert.Contains(t, printed, "verified")
				assert.NotContains(t, printed, "REJECTED")
			} else {
				assert.Contains(t, printed, "REJECTED")
			}
		})
	}
}

// A body that is not JSON is itself the finding, so it is shown raw rather
// than swallowed by a parse error.
func TestWebhookSinkPrintsABodyItCannotParse(t *testing.T) {
	const secret = "whsec_0123456789abcdef"
	body := []byte("not json at all")
	sig, ts := webhook.Sign(secret, body, time.Now())

	out, srv := startSink(t, secret, time.Minute)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set(webhook.SignatureHeader, sig)
	req.Header.Set(webhook.TimestampHeader, ts)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, http.StatusOK, resp.StatusCode, "it verified: the bytes are what was signed")
	assert.Contains(t, out.String(), "not json at all")
}

// The sink runs until it is interrupted, which is new for this CLI: every
// other command returns on its own. Cancelling its context has to bring the
// listener down rather than leave the process hanging.
func TestWebhookSinkStopsWhenItsContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := newWebhookSinkCmd()
	out := &safeBuffer{}
	cmd.SetOut(out)

	done := make(chan error, 1)
	go func() { done <- runWebhookSink(ctx, cmd, freePort(t), "whsec_x", time.Minute) }()

	require.Eventually(t, func() bool { return strings.Contains(out.String(), "listening") },
		2*time.Second, 10*time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
		assert.Contains(t, out.String(), "stopped")
	case <-time.After(10 * time.Second):
		t.Fatal("the sink did not shut down")
	}
}

// startSink runs one sink on a free port and returns its output buffer and
// base URL. It drives the real command function, not a copy of its handler.
func startSink(t *testing.T, secret string, tolerance time.Duration) (*safeBuffer, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	port := freePort(t)
	cmd := newWebhookSinkCmd()
	out := &safeBuffer{}
	cmd.SetOut(out)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runWebhookSink(ctx, cmd, port, secret, tolerance)
	}()
	t.Cleanup(func() { cancel(); <-done })

	// Dial rather than GET: an unsigned request would be printed as REJECTED
	// and the test would be reading its own liveness probe.
	addr := "127.0.0.1:" + strconv.Itoa(port)
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 5*time.Second, 10*time.Millisecond, "the sink never came up")
	return out, "http://" + addr
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// safeBuffer is a bytes.Buffer the handler goroutine and the test can both
// touch.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

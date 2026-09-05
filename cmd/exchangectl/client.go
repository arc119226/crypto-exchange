package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

const requestTimeout = 10 * time.Second

// newClient builds the generated API client from --base-url. Every request
// carries an X-Request-Id so `make trace ID=...` can follow it through the
// server logs; the id is printed on failures.
func newClient(cmd *cobra.Command) (*apiclient.ClientWithResponses, string, error) {
	base, err := cmd.Flags().GetString("base-url")
	if err != nil {
		return nil, "", err
	}
	c, err := apiclient.NewClientWithResponses(base,
		apiclient.WithHTTPClient(&http.Client{Timeout: requestTimeout}),
		apiclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			if req.Header.Get(telemetry.RequestIDHeader) == "" {
				req.Header.Set(telemetry.RequestIDHeader, newRequestID())
			}
			return nil
		}),
	)
	if err != nil {
		return nil, "", fmt.Errorf("invalid --base-url %q: %w", base, err)
	}
	return c, base, nil
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ctl-" + fmt.Sprint(time.Now().UnixNano())
	}
	return "ctl-" + hex.EncodeToString(b[:])
}

// transportError explains why the API could not be reached at all.
func transportError(base string, err error) error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("timed out after %s waiting for the API at %s", requestTimeout, base)
	}
	return fmt.Errorf("cannot reach the API at %s: %w (is the stack up? try `make up-single`)", base, err)
}

// apiError turns a non-2xx response into a readable error, preferring the
// RFC 7807 body when the server sent one.
func apiError(resp *http.Response, problem *apiclient.Problem) error {
	cid := resp.Header.Get(telemetry.RequestIDHeader)
	if problem != nil {
		msg := problem.Title
		if problem.Detail != "" {
			msg += ": " + problem.Detail
		}
		if problem.CorrelationID != "" {
			cid = problem.CorrelationID
		}
		return fmt.Errorf("%s (HTTP %d, correlation_id=%s)", msg, resp.StatusCode, cid)
	}
	return fmt.Errorf("unexpected HTTP %d from %s (correlation_id=%s)", resp.StatusCode, resp.Request.URL, cid)
}

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

const requestTimeout = 10 * time.Second

// credentials are the two ways exchangectl authenticates: a session access
// token (--token / EXCHANGE_TOKEN) or an API key pair (--api-key /
// --api-secret, EXCHANGE_API_KEY / EXCHANGE_API_SECRET) that signs every
// request the way docs/plan-v1.0.md §14 / ADR-0006 specify.
type credentials struct {
	token     string
	apiKey    string
	apiSecret string
}

func credentialsFrom(cmd *cobra.Command) (credentials, error) {
	var c credentials
	var err error
	if c.token, err = cmd.Flags().GetString("token"); err != nil {
		return c, err
	}
	if c.apiKey, err = cmd.Flags().GetString("api-key"); err != nil {
		return c, err
	}
	if c.apiSecret, err = cmd.Flags().GetString("api-secret"); err != nil {
		return c, err
	}
	if (c.apiKey == "") != (c.apiSecret == "") {
		return c, errors.New("--api-key and --api-secret must be given together")
	}
	return c, nil
}

// editor returns the request editor that attaches the credential.
func (c credentials) editor() apiclient.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		switch {
		case c.token != "":
			req.Header.Set("Authorization", "Bearer "+c.token)
		case c.apiKey != "":
			var body []byte
			if req.Body != nil && req.Body != http.NoBody {
				b, err := io.ReadAll(req.Body)
				if err != nil {
					return err
				}
				body = b
				req.Body = io.NopCloser(bytes.NewReader(b))
				req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
			}
			ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
			req.Header.Set(auth.HeaderAPIKey, c.apiKey)
			req.Header.Set(auth.HeaderAPITimestamp, ts)
			req.Header.Set(auth.HeaderAPISignature, auth.SignRequest(c.apiSecret, ts, req.Method, req.URL.RequestURI(), body))
		}
		return nil
	}
}

// newClient builds the generated API client from --base-url. Every request
// carries an X-Request-Id so `make trace ID=...` can follow it through the
// server logs; the id is printed on failures. Credentials, when given, are
// attached to every request.
func newClient(cmd *cobra.Command) (*apiclient.ClientWithResponses, string, error) {
	base, err := cmd.Flags().GetString("base-url")
	if err != nil {
		return nil, "", err
	}
	creds, err := credentialsFrom(cmd)
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
		apiclient.WithRequestEditorFn(creds.editor()),
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

// problemError renders any non-2xx response from its raw body (works for
// every status the generated client may not have a typed field for).
func problemError(resp *http.Response, body []byte) error {
	var p apiclient.Problem
	if err := json.Unmarshal(body, &p); err == nil && p.Title != "" {
		return apiError(resp, &p)
	}
	return apiError(resp, nil)
}

package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// jwksFetchTimeout bounds one JWKS request.
const jwksFetchTimeout = 5 * time.Second

// maxJWKSBody is far larger than a key set of a handful of Ed25519 keys.
const maxJWKSBody = 256 << 10

// RemoteVerifier verifies tokens against a JWKS served by another role
// (JWT_JWKS_URL, docs/plan-v1.0.md §14). Only the api role holds the private
// key; engine and chain verify with this.
//
// The key set is fetched lazily — on the first token, not at startup — so a
// role that verifies does not depend on the issuer being up before it can
// become ready. A token signed with an unknown kid triggers at most one
// refresh per RefreshInterval, which is what makes key rotation work without
// a restart while a stream of bogus tokens cannot turn into a fetch storm.
type RemoteVerifier struct {
	url    string
	issuer string
	client *http.Client

	// TTL after which the key set is refetched before the next verification.
	ttl time.Duration
	// RefreshInterval is the shortest gap between two failure-triggered refetches.
	refresh time.Duration

	mu        sync.Mutex
	verifier  *Verifier
	fetchedAt time.Time
}

// NewRemoteVerifier builds a verifier over the JWKS at url.
func NewRemoteVerifier(url, issuer string) (*RemoteVerifier, error) {
	if url == "" {
		return nil, errors.New("auth: jwks url required")
	}
	return &RemoteVerifier{
		url: url, issuer: issuer,
		client:  &http.Client{Timeout: jwksFetchTimeout},
		ttl:     15 * time.Minute,
		refresh: 30 * time.Second,
	}, nil
}

// WithHTTPClient overrides the client (tests, proxies).
func (v *RemoteVerifier) WithHTTPClient(c *http.Client) *RemoteVerifier {
	v.client = c
	return v
}

// Verify checks the token against the cached key set, refetching once when
// the cached keys cannot validate it (rotation).
func (v *RemoteVerifier) Verify(token string, audience string, now time.Time) (Claims, error) {
	ver, fetchedAt, err := v.keys(false, now)
	if err != nil {
		return Claims{}, err
	}
	claims, err := ver.Verify(token, audience, now)
	if err == nil {
		return claims, nil
	}
	// The signature may have been made with a key added after the last fetch.
	if now.Sub(fetchedAt) < v.refresh {
		return Claims{}, err
	}
	ver, _, ferr := v.keys(true, now)
	if ferr != nil {
		return Claims{}, err // report the token error, not the refetch error
	}
	return ver.Verify(token, audience, now)
}

// keys returns the cached verifier, fetching when it is missing, stale or
// force is set.
func (v *RemoteVerifier) keys(force bool, now time.Time) (*Verifier, time.Time, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !force && v.verifier != nil && now.Sub(v.fetchedAt) < v.ttl {
		return v.verifier, v.fetchedAt, nil
	}
	set, err := v.fetch()
	if err != nil {
		if v.verifier != nil {
			return v.verifier, v.fetchedAt, nil // serve stale keys rather than fail closed on a blip
		}
		return nil, time.Time{}, err
	}
	v.verifier, v.fetchedAt = set, now
	return v.verifier, v.fetchedAt, nil
}

func (v *RemoteVerifier) fetch() (*Verifier, error) {
	ctx, cancel := context.WithTimeout(context.Background(), jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: jwks request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: fetch jwks %s: %w", v.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: fetch jwks %s: HTTP %d", v.url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBody))
	if err != nil {
		return nil, fmt.Errorf("auth: read jwks: %w", err)
	}
	return NewVerifier(body, v.issuer)
}

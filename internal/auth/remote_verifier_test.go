package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jwksServer serves the signer's key set and counts the fetches.
func jwksServer(t *testing.T, s *Signer, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		b, err := s.JWKS()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newSigner(t *testing.T, issuer string) *Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := NewSigner(priv, issuer)
	require.NoError(t, err)
	return s
}

func internalToken(t *testing.T, s *Signer, now time.Time) string {
	t.Helper()
	tok, err := s.Issue(Claims{
		UserID: "u1", AccountID: "acct-1", TenantID: "default", Role: RoleUser,
		Scopes: []Scope{ScopeRead, ScopeTrade}, Method: MethodJWT, Audience: AudienceInternal,
	}, now, 5*time.Minute)
	require.NoError(t, err)
	return tok
}

func TestRemoteVerifierFetchesLazilyAndCaches(t *testing.T) {
	signer := newSigner(t, "exchange")
	var hits atomic.Int64
	srv := jwksServer(t, signer, &hits)

	v, err := NewRemoteVerifier(srv.URL, "exchange")
	require.NoError(t, err)
	// Nothing is fetched until a token needs verifying: a verifying role must
	// not depend on the issuer being up before it can start.
	assert.Zero(t, hits.Load())

	now := time.Now().UTC()
	tok := internalToken(t, signer, now)
	claims, err := v.Verify(tok, AudienceInternal, now)
	require.NoError(t, err)
	assert.Equal(t, "acct-1", claims.AccountID)
	assert.Equal(t, "default", claims.TenantID)
	assert.Contains(t, claims.Scopes, ScopeTrade)
	assert.EqualValues(t, 1, hits.Load())

	for range 5 {
		_, err := v.Verify(tok, AudienceInternal, now)
		require.NoError(t, err)
	}
	assert.EqualValues(t, 1, hits.Load(), "the key set is cached")

	// the wrong audience is refused without another fetch
	_, err = v.Verify(tok, AudiencePublic, now)
	assert.ErrorIs(t, err, ErrInvalidToken)
	assert.EqualValues(t, 1, hits.Load(), "a bad token within the refresh interval must not trigger a refetch")
}

func TestRemoteVerifierRefetchesAfterTTL(t *testing.T) {
	signer := newSigner(t, "exchange")
	var hits atomic.Int64
	srv := jwksServer(t, signer, &hits)
	v, err := NewRemoteVerifier(srv.URL, "exchange")
	require.NoError(t, err)

	now := time.Now().UTC()
	tok := internalToken(t, signer, now)
	_, err = v.Verify(tok, AudienceInternal, now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, hits.Load())

	later := now.Add(16 * time.Minute)
	tok2 := internalToken(t, signer, later)
	_, err = v.Verify(tok2, AudienceInternal, later)
	require.NoError(t, err)
	assert.EqualValues(t, 2, hits.Load(), "the key set is refetched after the ttl")
}

// TestRemoteVerifierPicksUpRotatedKeys is why the failure path refetches: a
// token signed by a key added after the last fetch must verify without
// restarting the role.
func TestRemoteVerifierPicksUpRotatedKeys(t *testing.T) {
	old := newSigner(t, "exchange")
	current := &atomic.Pointer[Signer]{}
	current.Store(old)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		b, err := current.Load().JWKS()
		require.NoError(t, err)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)

	v, err := NewRemoteVerifier(srv.URL, "exchange")
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = v.Verify(internalToken(t, old, now), AudienceInternal, now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, hits.Load())

	rotated := newSigner(t, "exchange")
	current.Store(rotated)
	// still inside the ttl, so only the failure path can save this
	later := now.Add(time.Minute)
	claims, err := v.Verify(internalToken(t, rotated, later), AudienceInternal, later)
	require.NoError(t, err)
	assert.Equal(t, "acct-1", claims.AccountID)
	assert.EqualValues(t, 2, hits.Load())
}

func TestRemoteVerifierServesStaleKeysWhenTheIssuerIsDown(t *testing.T) {
	signer := newSigner(t, "exchange")
	var hits atomic.Int64
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		b, err := signer.JWKS()
		require.NoError(t, err)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)

	v, err := NewRemoteVerifier(srv.URL, "exchange")
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, mustVerify(v, internalToken(t, signer, now), now))

	down.Store(true)
	later := now.Add(16 * time.Minute) // ttl expired, refetch fails
	assert.NoError(t, mustVerify(v, internalToken(t, signer, later), later),
		"a blip in the issuer must not stop the engine from accepting valid commands")
}

func TestRemoteVerifierErrorsWithoutAnyKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	v, err := NewRemoteVerifier(srv.URL, "exchange")
	require.NoError(t, err)
	_, err = v.Verify("whatever", AudienceInternal, time.Now())
	assert.ErrorContains(t, err, "HTTP 404")

	_, err = NewRemoteVerifier("", "exchange")
	assert.Error(t, err)
}

func mustVerify(v *RemoteVerifier, token string, now time.Time) error {
	_, err := v.Verify(token, AudienceInternal, now)
	return err
}

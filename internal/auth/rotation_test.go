package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSignerPublishesThePreviousKey: after a rotation the new signer serves
// both public keys under their own kids, verifies tokens signed by either,
// and signs only with the new one.
func TestSignerPublishesThePreviousKey(t *testing.T) {
	old := newSigner(t, "exchange")
	now := time.Now().UTC()
	oldToken := internalToken(t, old, now)

	rotated := newSigner(t, "exchange")
	require.NoError(t, rotated.WithPreviousKey(old.PublicKey()))
	assert.Equal(t, old.KeyID(), rotated.PreviousKeyID())
	assert.NotEqual(t, rotated.KeyID(), rotated.PreviousKeyID())

	jwks, err := rotated.JWKS()
	require.NoError(t, err)
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(jwks, &set))
	require.Len(t, set.Keys, 2)
	assert.Equal(t, rotated.KeyID(), set.Keys[0]["kid"], "the signing key comes first")
	assert.Equal(t, old.KeyID(), set.Keys[1]["kid"])
	for _, k := range set.Keys {
		assert.NotContains(t, k, "d", "no private scalar in the set")
	}

	v, err := rotated.VerifierFor()
	require.NoError(t, err)
	_, err = v.Verify(oldToken, AudienceInternal, now)
	require.NoError(t, err, "a token from before the rotation still verifies")
	newToken := internalToken(t, rotated, now)
	_, err = v.Verify(newToken, AudienceInternal, now)
	require.NoError(t, err)
	header, err := base64Segment(newToken)
	require.NoError(t, err)
	assert.Equal(t, rotated.KeyID(), header["kid"], "new tokens carry the new kid only")

	// dropping the previous key is what ends the grace period
	dropped, err := newSigner(t, "exchange").VerifierFor()
	require.NoError(t, err)
	_, err = dropped.Verify(oldToken, AudienceInternal, now)
	assert.ErrorIs(t, err, ErrInvalidToken)

	assert.ErrorContains(t, rotated.WithPreviousKey(rotated.PublicKey()), "same thumbprint",
		"the same key twice is a rotation that did not happen")
	assert.Error(t, rotated.WithPreviousKey(ed25519.PublicKey("short")))
}

// TestLoadPublicKeyReadsBothPEMForms: JWT_PREVIOUS_KEY_FILE may be the
// PKCS#8 file gen-jwt writes or the SPKI file jwt-public prints, and the
// kid is the same either way.
func TestLoadPublicKeyReadsBothPEMForms(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dir := t.TempDir()

	der, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	privPath := filepath.Join(dir, "ed25519.pem")
	require.NoError(t, os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	fromPriv, err := LoadPublicKey(privPath)
	require.NoError(t, err)
	assert.Equal(t, pub, fromPriv)

	spki, err := PublicKeyPEM(pub)
	require.NoError(t, err)
	assert.Contains(t, string(spki), "BEGIN PUBLIC KEY")
	pubPath := filepath.Join(dir, "ed25519.pub")
	require.NoError(t, os.WriteFile(pubPath, spki, 0o600))
	fromPub, err := LoadPublicKey(pubPath)
	require.NoError(t, err)
	assert.Equal(t, pub, fromPub)

	signer, err := NewSigner(priv, "exchange")
	require.NoError(t, err)
	kid, err := KeyIDOf(fromPub)
	require.NoError(t, err)
	assert.Equal(t, signer.KeyID(), kid, "the thumbprint is a function of the public key alone")

	bad := filepath.Join(dir, "bad.pem")
	require.NoError(t, os.WriteFile(bad, []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), 0o600))
	_, err = LoadPublicKey(bad)
	assert.ErrorContains(t, err, "unexpected PEM block")
	_, err = LoadPublicKey(filepath.Join(dir, "missing.pem"))
	assert.Error(t, err)
}

// TestRemoteVerifierAcceptsBothKidsDuringRotation: a role that verifies
// through the JWKS sees both keys in one fetch, so tokens from before and
// after the switch verify without the failure-path refetch.
func TestRemoteVerifierAcceptsBothKidsDuringRotation(t *testing.T) {
	old := newSigner(t, "exchange")
	rotated := newSigner(t, "exchange")
	require.NoError(t, rotated.WithPreviousKey(old.PublicKey()))
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		b, err := rotated.JWKS()
		require.NoError(t, err)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)

	v, err := NewRemoteVerifier(srv.URL, "exchange")
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = v.Verify(internalToken(t, old, now), AudienceInternal, now)
	require.NoError(t, err)
	_, err = v.Verify(internalToken(t, rotated, now), AudienceInternal, now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, hits.Load(), "one key set covers both")
}

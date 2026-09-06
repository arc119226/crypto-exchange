package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPasswordHashAndVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery", TestPasswordParams)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$"), h)
	ok, err := VerifyPassword(h, "correct horse battery")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = VerifyPassword(h, "correct horse batter")
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = HashPassword("short", TestPasswordParams)
	assert.ErrorIs(t, err, ErrWeakPassword)
	_, err = HashPassword(strings.Repeat("x", 129), TestPasswordParams)
	assert.ErrorIs(t, err, ErrWeakPassword)
	for _, bad := range []string{"", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA", "$argon2id$v=18$m=1,t=1,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=1,t=1,p=1$!!$aGFzaA"} {
		_, err := VerifyPassword(bad, "x")
		assert.ErrorIs(t, err, ErrBadHash, bad)
	}
	// two hashes of the same password differ (random salt) and both verify
	h2, err := HashPassword("correct horse battery", TestPasswordParams)
	require.NoError(t, err)
	assert.NotEqual(t, h, h2)
}

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := NewSigner(priv, "exchange-test")
	require.NoError(t, err)
	return s
}

func TestJWTRoundTripAndJWKS(t *testing.T) {
	signer := newTestSigner(t)
	now := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	tok, err := signer.Issue(Claims{UserID: "u1", AccountID: "a1", TenantID: "default", Role: RoleUser, Scopes: AllScopes, Audience: AudiencePublic}, now, 15*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(tok, "."))

	// the header carries the kid so multi-key JWKS rotation works later
	header, err := base64Segment(tok)
	require.NoError(t, err)
	assert.Equal(t, "EdDSA", header["alg"])
	assert.Equal(t, signer.KeyID(), header["kid"])

	v, err := signer.VerifierFor()
	require.NoError(t, err)
	c, err := v.Verify(tok, AudiencePublic, now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "u1", c.UserID)
	assert.Equal(t, "a1", c.AccountID)
	assert.Equal(t, "default", c.TenantID)
	assert.Equal(t, RoleUser, c.Role)
	assert.Equal(t, AllScopes, c.Scopes)
	assert.Equal(t, MethodJWT, c.Method)
	assert.Equal(t, now.Add(15*time.Minute), c.ExpiresAt)

	// expired, wrong audience, wrong key, tampered
	_, err = v.Verify(tok, AudiencePublic, now.Add(16*time.Minute))
	assert.ErrorIs(t, err, ErrInvalidToken, "expired")
	_, err = v.Verify(tok, AudienceInternal, now)
	assert.ErrorIs(t, err, ErrInvalidToken, "audience")
	other, err := newTestSigner(t).VerifierFor()
	require.NoError(t, err)
	_, err = other.Verify(tok, AudiencePublic, now)
	assert.ErrorIs(t, err, ErrInvalidToken, "unknown kid / key")
	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]
	_, err = v.Verify(tampered, AudiencePublic, now)
	assert.ErrorIs(t, err, ErrInvalidToken, "tampered")

	// JWKS is a public-only set with the same kid
	jwks, err := signer.JWKS()
	require.NoError(t, err)
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(jwks, &set))
	require.Len(t, set.Keys, 1)
	assert.Equal(t, "OKP", set.Keys[0]["kty"])
	assert.Equal(t, "Ed25519", set.Keys[0]["crv"])
	assert.Equal(t, signer.KeyID(), set.Keys[0]["kid"])
	assert.Equal(t, "sig", set.Keys[0]["use"])
	assert.NotContains(t, set.Keys[0], "d", "private scalar never leaves the signer")

	// a verifier built from the served JWKS accepts the token
	fromJWKS, err := NewVerifier(jwks, "exchange-test")
	require.NoError(t, err)
	_, err = fromJWKS.Verify(tok, AudiencePublic, now)
	require.NoError(t, err)

	_, err = signer.Issue(Claims{UserID: "u1"}, now, time.Minute)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

func base64Segment(tok string) (map[string]any, error) {
	seg := strings.Split(tok, ".")[0]
	dec, err := decodeB64URL(seg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(dec, &m)
}

func TestAPIKeySecretsAndSignatures(t *testing.T) {
	master, err := ParseMasterKey(strings.Repeat("ab", 32))
	require.NoError(t, err)
	_, err = ParseMasterKey("abcd")
	assert.Error(t, err)

	secret, err := newSecret()
	require.NoError(t, err)
	sealed, err := encryptSecret(master, secret)
	require.NoError(t, err)
	back, err := decryptSecret(master, sealed)
	require.NoError(t, err)
	assert.Equal(t, secret, back)
	sealed[len(sealed)-1] ^= 1
	_, err = decryptSecret(master, sealed)
	assert.Error(t, err, "tampered ciphertext")
	kid, err := newKeyID()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(kid, "ak_"))
	assert.Len(t, kid, 3+24)

	now := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	body := []byte(`{"client_order_id":"c1"}`)
	sig := SignRequest(secret, ts, "POST", "/v1/orders?x=1", body)
	require.NoError(t, verifySignature(secret, ts, sig, "post", "/v1/orders?x=1", body, now.Add(5*time.Second)))
	require.NoError(t, verifySignature(secret, ts, strings.ToUpper(sig), "POST", "/v1/orders?x=1", body, now), "hex case-insensitive")
	for name, tc := range map[string]struct {
		ts, sig, method, uri string
		body                 []byte
		at                   time.Time
	}{
		"skew":         {ts, sig, "POST", "/v1/orders?x=1", body, now.Add(31 * time.Second)},
		"past":         {ts, sig, "POST", "/v1/orders?x=1", body, now.Add(-31 * time.Second)},
		"method":       {ts, sig, "GET", "/v1/orders?x=1", body, now},
		"uri":          {ts, sig, "POST", "/v1/orders", body, now},
		"body":         {ts, sig, "POST", "/v1/orders?x=1", []byte(`{}`), now},
		"bad ts":       {"yesterday", sig, "POST", "/v1/orders?x=1", body, now},
		"other secret": {ts, SignRequest("other", ts, "POST", "/v1/orders?x=1", body), "POST", "/v1/orders?x=1", body, now},
	} {
		assert.ErrorIs(t, verifySignature(secret, tc.ts, tc.sig, tc.method, tc.uri, tc.body, tc.at), ErrInvalidCredentials, name)
	}
}

func TestScopesAndIPs(t *testing.T) {
	sc, err := ParseScopes([]string{"trade", "read", "trade"})
	require.NoError(t, err)
	assert.Equal(t, []Scope{ScopeRead, ScopeTrade}, sc)
	_, err = ParseScopes(nil)
	assert.ErrorIs(t, err, ErrInvalidInput)
	_, err = ParseScopes([]string{"admin"})
	assert.ErrorIs(t, err, ErrInvalidInput)
	p := Principal{Scopes: sc}
	assert.True(t, p.Has(ScopeTrade))
	assert.False(t, p.Has(ScopeWithdraw))

	assert.True(t, ipAllowed("10.0.0.7", []string{"10.0.0.0/24"}))
	assert.True(t, ipAllowed("192.168.1.1", []string{"10.0.0.0/24", "192.168.1.1"}))
	assert.False(t, ipAllowed("10.0.1.7", []string{"10.0.0.0/24"}))
	assert.False(t, ipAllowed("nope", []string{"10.0.0.0/24"}))
	_, err = parseIP("300.1.1.1")
	assert.Error(t, err)
}

func TestAuthenticateMiddlewareJWT(t *testing.T) {
	signer := newTestSigner(t)
	v, err := signer.VerifierFor()
	require.NoError(t, err)
	master, _ := ParseMasterKey(strings.Repeat("00", 32))
	svc, err := New(nil, Config{Tenant: "default", Issuer: "exchange-test", MasterKey: master, Password: TestPasswordParams}, signer, v, nil, nil)
	require.NoError(t, err)
	now := time.Now().UTC()
	tok, err := signer.Issue(Claims{UserID: "u1", AccountID: "a1", TenantID: "default", Role: RoleUser, Scopes: AllScopes, Audience: AudiencePublic}, now, time.Minute)
	require.NoError(t, err)

	var got Principal
	var seen bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, seen = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	onError := func(w http.ResponseWriter, _ *http.Request, status int, title, detail string) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(title + ": " + detail))
	}
	h := svc.Authenticate(onError)(next)

	do := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/account", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		seen = false
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := do("Bearer " + tok)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, seen)
	assert.Equal(t, "a1", got.AccountID)
	assert.Equal(t, MethodJWT, got.Method)

	rec = do("")
	assert.Equal(t, http.StatusNoContent, rec.Code, "no credential passes through")
	assert.False(t, seen, "...without a principal")

	rec = do("Bearer nope")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "invalid_token")
	rec = do("Basic abc")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// tenant mismatch is rejected even with a valid signature
	foreign, err := signer.Issue(Claims{UserID: "u1", AccountID: "a1", TenantID: "other", Role: RoleUser, Scopes: AllScopes, Audience: AudiencePublic}, now, time.Minute)
	require.NoError(t, err)
	rec = do("Bearer " + foreign)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestNormalizeEmail(t *testing.T) {
	e, err := NormalizeEmail("  Alice@Example.COM ")
	require.NoError(t, err)
	assert.Equal(t, "alice@example.com", e)
	for _, bad := range []string{"", "alice", "alice@", "Alice <alice@example.com>", "a@b"} {
		_, err := NormalizeEmail(bad)
		assert.ErrorIs(t, err, ErrInvalidInput, bad)
	}
}

package auth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// Audiences. Public tokens are what users hold; internal tokens are minted
// by the api role for API-key requests it forwards to other roles (ADR-0006).
const (
	AudiencePublic   = "exchange"
	AudienceInternal = "internal"
)

// Claim names beyond the registered ones.
const (
	claimAccountID = "account_id"
	claimTenantID  = "tenant_id"
	claimRole      = "role"
	claimScopes    = "scopes"
	claimMethod    = "method"
)

// Claims is the decoded content of an access token.
type Claims struct {
	UserID    string
	AccountID string
	TenantID  string
	Role      string
	Scopes    []Scope
	Method    Method
	Audience  string
	ExpiresAt time.Time
	TokenID   string
}

// Principal converts claims into the request principal.
func (c Claims) Principal() Principal {
	return Principal{UserID: c.UserID, AccountID: c.AccountID, TenantID: c.TenantID, Role: c.Role, Method: c.Method, Scopes: c.Scopes}
}

// Signer issues EdDSA (Ed25519) tokens. Only the api role holds one.
type Signer struct {
	priv   jwk.Key
	pub    jwk.Key
	issuer string
}

// LoadPrivateKey reads a PKCS#8 PEM Ed25519 key (exchange keys gen-jwt).
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-provided path
	if err != nil {
		return nil, fmt.Errorf("auth: read jwt key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("auth: jwt key: expected a PEM PRIVATE KEY block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("auth: jwt key: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("auth: jwt key: not an Ed25519 key")
	}
	return priv, nil
}

// NewSigner wraps an Ed25519 private key; the key id is its thumbprint.
func NewSigner(priv ed25519.PrivateKey, issuer string) (*Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("auth: invalid ed25519 private key")
	}
	if issuer == "" {
		return nil, errors.New("auth: issuer required")
	}
	pk, err := jwk.Import(priv)
	if err != nil {
		return nil, fmt.Errorf("auth: import key: %w", err)
	}
	if err := jwk.AssignKeyID(pk); err != nil {
		return nil, fmt.Errorf("auth: key id: %w", err)
	}
	for _, kv := range []struct {
		k string
		v any
	}{{jwk.AlgorithmKey, jwa.EdDSA()}, {jwk.KeyUsageKey, jwk.ForSignature}} {
		if err := pk.Set(kv.k, kv.v); err != nil {
			return nil, fmt.Errorf("auth: key %s: %w", kv.k, err)
		}
	}
	pub, err := jwk.PublicKeyOf(pk)
	if err != nil {
		return nil, fmt.Errorf("auth: public key: %w", err)
	}
	return &Signer{priv: pk, pub: pub, issuer: issuer}, nil
}

// KeyID returns the kid every issued token carries.
func (s *Signer) KeyID() string {
	kid, _ := s.priv.KeyID()
	return kid
}

// Issue signs a token for the claims, valid from now for ttl.
func (s *Signer) Issue(c Claims, now time.Time, ttl time.Duration) (string, error) {
	if c.UserID == "" || c.AccountID == "" || c.TenantID == "" || c.Audience == "" {
		return "", fmt.Errorf("%w: incomplete claims", ErrInvalidInput)
	}
	if c.Method == "" {
		c.Method = MethodJWT
	}
	scopes := make([]string, 0, len(c.Scopes))
	for _, sc := range c.Scopes {
		scopes = append(scopes, string(sc))
	}
	b := jwt.NewBuilder().
		Issuer(s.issuer).Subject(c.UserID).Audience([]string{c.Audience}).
		IssuedAt(now).NotBefore(now).Expiration(now.Add(ttl)).
		Claim(claimAccountID, c.AccountID).Claim(claimTenantID, c.TenantID).Claim(claimRole, c.Role).
		Claim(claimScopes, scopes).Claim(claimMethod, string(c.Method))
	if c.TokenID != "" {
		b = b.JwtID(c.TokenID)
	}
	tok, err := b.Build()
	if err != nil {
		return "", fmt.Errorf("auth: build token: %w", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.EdDSA(), s.priv))
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return string(signed), nil
}

// JWKS returns the public key set served at /.well-known/jwks.json.
func (s *Signer) JWKS() ([]byte, error) {
	set := jwk.NewSet()
	if err := set.AddKey(s.pub); err != nil {
		return nil, fmt.Errorf("auth: jwks: %w", err)
	}
	return json.Marshal(set)
}

// Verifier validates tokens against a public key set (the signer's own, or
// one fetched from JWT_JWKS_URL in split deployments).
type Verifier struct {
	set    jwk.Set
	issuer string
	skew   time.Duration
}

// NewVerifier builds a verifier over a JWKS document.
func NewVerifier(jwks []byte, issuer string) (*Verifier, error) {
	set, err := jwk.Parse(jwks)
	if err != nil {
		return nil, fmt.Errorf("auth: parse jwks: %w", err)
	}
	if set.Len() == 0 {
		return nil, errors.New("auth: empty jwks")
	}
	return &Verifier{set: set, issuer: issuer, skew: 30 * time.Second}, nil
}

// VerifierFor returns a verifier over the signer's public key.
func (s *Signer) VerifierFor() (*Verifier, error) {
	b, err := s.JWKS()
	if err != nil {
		return nil, err
	}
	return NewVerifier(b, s.issuer)
}

// Verify checks signature (kid → key), issuer, audience and time bounds at
// now, and returns the claims.
func (v *Verifier) Verify(token string, audience string, now time.Time) (Claims, error) {
	tok, err := jwt.Parse([]byte(token),
		jwt.WithKeySet(v.set, jws.WithRequireKid(true)),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(audience),
		jwt.WithAcceptableSkew(v.skew),
		jwt.WithClock(jwt.ClockFunc(func() time.Time { return now })),
	)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	c := Claims{Audience: audience, Method: MethodJWT}
	c.UserID, _ = tok.Subject()
	c.ExpiresAt, _ = tok.Expiration()
	c.TokenID, _ = tok.JwtID()
	for name, dst := range map[string]any{claimAccountID: &c.AccountID, claimTenantID: &c.TenantID, claimRole: &c.Role} {
		if err := tok.Get(name, dst); err != nil {
			return Claims{}, fmt.Errorf("%w: claim %s: %v", ErrInvalidToken, name, err)
		}
	}
	// JSON arrays come back as []any
	var rawScopes []any
	if err := tok.Get(claimScopes, &rawScopes); err != nil {
		return Claims{}, fmt.Errorf("%w: claim %s: %v", ErrInvalidToken, claimScopes, err)
	}
	scopes := make([]string, 0, len(rawScopes))
	for _, v := range rawScopes {
		str, ok := v.(string)
		if !ok {
			return Claims{}, fmt.Errorf("%w: claim %s: not a string list", ErrInvalidToken, claimScopes)
		}
		scopes = append(scopes, str)
	}
	var method string
	if err := tok.Get(claimMethod, &method); err == nil && method != "" {
		c.Method = Method(method)
	}
	if c.UserID == "" || c.AccountID == "" || c.TenantID == "" {
		return Claims{}, fmt.Errorf("%w: missing subject/account/tenant", ErrInvalidToken)
	}
	if c.Scopes, err = ParseScopes(scopes); err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	return c, nil
}

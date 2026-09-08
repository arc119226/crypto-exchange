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
//
// During a key rotation it also publishes the public half of one more key
// (WithPreviousKey) so that tokens signed before the switch keep verifying
// until they expire; it never signs with that key. Every key is published
// under its RFC 7638 thumbprint, which is what the kid in a token names.
type Signer struct {
	priv     jwk.Key
	pub      jwk.Key
	previous jwk.Key
	public   ed25519.PublicKey
	issuer   string
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

// LoadPublicKey reads the key JWT_PREVIOUS_KEY_FILE names: either the
// PKCS#8 file `exchange keys gen-jwt` writes, of which only the public half
// is kept, or a SPKI "PUBLIC KEY" PEM as `exchange keys jwt-public` prints.
// The second form exists for the first phase of a rotation across several
// api replicas, where the new key is published before any replica signs
// with it (docs/runbooks/key-rotation.md): a replica that only publishes a
// key has no business holding its private half.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-provided path
	if err != nil {
		return nil, fmt.Errorf("auth: read jwt key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("auth: jwt key: expected a PEM PRIVATE KEY or PUBLIC KEY block")
	}
	switch block.Type {
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: jwt key: %w", err)
		}
		priv, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("auth: jwt key: not an Ed25519 key")
		}
		return priv.Public().(ed25519.PublicKey), nil
	case "PUBLIC KEY":
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("auth: jwt key: %w", err)
		}
		pub, ok := k.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("auth: jwt key: not an Ed25519 key")
		}
		return pub, nil
	default:
		return nil, fmt.Errorf("auth: jwt key: unexpected PEM block %q", block.Type)
	}
}

// PublicKeyPEM encodes a public key as SPKI PEM, the form LoadPublicKey
// reads back.
func PublicKeyPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("auth: encode public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// KeyIDOf returns the kid a public key is published under.
func KeyIDOf(pub ed25519.PublicKey) (string, error) {
	k, err := publicJWK(pub)
	if err != nil {
		return "", err
	}
	kid, _ := k.KeyID()
	return kid, nil
}

// publicJWK imports a public key and stamps it the way the JWKS serves it:
// thumbprint kid, EdDSA, signature use.
func publicJWK(pub ed25519.PublicKey) (jwk.Key, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("auth: invalid ed25519 public key")
	}
	pk, err := jwk.Import(pub)
	if err != nil {
		return nil, fmt.Errorf("auth: import key: %w", err)
	}
	if err := stampSigningKey(pk); err != nil {
		return nil, err
	}
	return pk, nil
}

func stampSigningKey(k jwk.Key) error {
	if err := jwk.AssignKeyID(k); err != nil {
		return fmt.Errorf("auth: key id: %w", err)
	}
	for _, kv := range []struct {
		k string
		v any
	}{{jwk.AlgorithmKey, jwa.EdDSA()}, {jwk.KeyUsageKey, jwk.ForSignature}} {
		if err := k.Set(kv.k, kv.v); err != nil {
			return fmt.Errorf("auth: key %s: %w", kv.k, err)
		}
	}
	return nil
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
	if err := stampSigningKey(pk); err != nil {
		return nil, err
	}
	pub, err := jwk.PublicKeyOf(pk)
	if err != nil {
		return nil, fmt.Errorf("auth: public key: %w", err)
	}
	return &Signer{priv: pk, pub: pub, public: priv.Public().(ed25519.PublicKey), issuer: issuer}, nil
}

// WithPreviousKey publishes one more public key in the JWKS. Tokens keep
// being signed with the current key only; the extra key lets tokens signed
// before a rotation verify until they expire, and lets the next key be
// published before it is used. The same key as the current one is refused:
// that is a rotation that did not happen.
func (s *Signer) WithPreviousKey(pub ed25519.PublicKey) error {
	pk, err := publicJWK(pub)
	if err != nil {
		return err
	}
	if kid, _ := pk.KeyID(); kid == s.KeyID() {
		return errors.New("auth: the previous JWT key is the current key (same thumbprint)")
	}
	s.previous = pk
	return nil
}

// KeyID returns the kid every issued token carries.
func (s *Signer) KeyID() string {
	kid, _ := s.priv.KeyID()
	return kid
}

// PreviousKeyID returns the kid of the extra published key, or "".
func (s *Signer) PreviousKeyID() string {
	if s.previous == nil {
		return ""
	}
	kid, _ := s.previous.KeyID()
	return kid
}

// PublicKey returns the raw public half of the signing key.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.public }

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

// JWKS returns the public key set served at /.well-known/jwks.json: the
// signing key first, then the previous key while one is published.
func (s *Signer) JWKS() ([]byte, error) {
	set := jwk.NewSet()
	if err := set.AddKey(s.pub); err != nil {
		return nil, fmt.Errorf("auth: jwks: %w", err)
	}
	if s.previous != nil {
		if err := set.AddKey(s.previous); err != nil {
			return nil, fmt.Errorf("auth: jwks: %w", err)
		}
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

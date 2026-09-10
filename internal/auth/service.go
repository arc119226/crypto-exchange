package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
)

// Config tunes the service.
type Config struct {
	Tenant     string
	Issuer     string        // JWT iss; default "exchange"
	AccessTTL  time.Duration // default 15 min (ADR-0006)
	RefreshTTL time.Duration // default 7 days
	MasterKey  []byte        // AES-256 key for API secrets (ParseMasterKey)
	// PreviousMasterKey is the key MasterKey replaced, kept only until
	// `exchange keys rewrap --domain api-keys` has re-sealed every row
	// (API_KEY_MASTER_KEY_PREVIOUS, docs/runbooks/key-rotation.md).
	PreviousMasterKey []byte
	Password          PasswordParams
	// TOTPKey seals administrators' TOTP secrets (ADMIN_TOTP_KEY). A different
	// key from MasterKey: that one opens every API-key secret, and the admin
	// role has no reason to hold it.
	TOTPKey []byte
	// PreviousTOTPKey is TOTPKey's predecessor during a rotation
	// (ADMIN_TOTP_KEY_PREVIOUS).
	PreviousTOTPKey []byte
	// AdminSessionTTL is the life of a verified back-office session; default
	// 8 hours. The password-only session before the code is fixed at ten
	// minutes.
	AdminSessionTTL time.Duration
}

func (c Config) withDefaults() Config {
	if c.Tenant == "" {
		c.Tenant = "default"
	}
	if c.Issuer == "" {
		c.Issuer = "exchange"
	}
	if c.AccessTTL <= 0 {
		c.AccessTTL = 15 * time.Minute
	}
	if c.RefreshTTL <= 0 {
		c.RefreshTTL = 7 * 24 * time.Hour
	}
	if c.Password == (PasswordParams{}) {
		c.Password = DefaultPasswordParams
	}
	if c.AdminSessionTTL <= 0 {
		c.AdminSessionTTL = 8 * time.Hour
	}
	return c
}

// User is a row of auth.users without the password hash.
type User struct {
	ID       string
	TenantID string
	Email    string
	Role     string
	KYCLevel int
	Status   string
	// TOTPEnabled is only ever true for administrators.
	TOTPEnabled bool
	// Version increases on every edit; user.* events carry it so a consumer
	// can order two changes to the same user.
	Version   int32
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Session is what register / login / refresh return.
type Session struct {
	UserID       string
	AccountID    string
	Role         string
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	ExpiresIn    time.Duration
}

// APIKey is an API key as listed to its owner (never the secret).
type APIKey struct {
	ID          string
	KeyID       string
	Label       string
	Scopes      []Scope
	IPAllowlist []string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
}

// APIKeyRequest is what the middleware extracts from an API-key-signed
// HTTP request.
type APIKeyRequest struct {
	KeyID      string
	Timestamp  string
	Signature  string
	Method     string
	RequestURI string
	Body       []byte
	IP         string
}

// Service implements registration, sessions and API keys over Postgres.
// It needs the api role's pool (INSERT on auth.*, ledger.accounts,
// audit.audit_events).
type Service struct {
	pool     *pgxpool.Pool
	cfg      Config
	signer   *Signer
	verifier *Verifier
	ledger   *ledger.Service
	audit    *audit.Recorder
	now      func() time.Time
	dummy    string // hash verified for unknown emails so timing does not leak existence
	apiKeys  secretbox.Keyring
	totp     secretbox.Keyring
	// log carries the audit writes this package cannot fail on. It defaults to
	// slog.Default() rather than being injected because depguard does not let
	// internal/auth reach internal/telemetry, and widening a deliberate module
	// boundary for a log line is the wrong trade -- the cost is that these
	// lines have no correlation id.
	log *slog.Logger
}

// New wires the service. signer may be nil for roles that only verify;
// verifier must be set.
func New(pool *pgxpool.Pool, cfg Config, signer *Signer, verifier *Verifier, l *ledger.Service, a *audit.Recorder) (*Service, error) {
	cfg = cfg.withDefaults()
	apiKeys := secretbox.Keyring{Current: cfg.MasterKey, Previous: cfg.PreviousMasterKey}
	if err := apiKeys.Validate(); err != nil {
		return nil, fmt.Errorf("auth: master key (API_KEY_MASTER_KEY): %w", err)
	}
	totp := secretbox.Keyring{Current: cfg.TOTPKey, Previous: cfg.PreviousTOTPKey}
	if err := totp.Validate(); err != nil {
		return nil, fmt.Errorf("auth: totp key (ADMIN_TOTP_KEY): %w", err)
	}
	dummy, err := HashPassword("timing-equaliser-password", cfg.Password)
	if err != nil {
		return nil, err
	}
	return &Service{
		pool: pool, cfg: cfg, signer: signer, verifier: verifier, ledger: l, audit: a,
		now: func() time.Time { return time.Now().UTC() }, dummy: dummy, apiKeys: apiKeys, totp: totp,
		log: slog.Default(),
	}, nil
}

// WithClock overrides the clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Signer returns the token signer (nil when the role cannot issue).
func (s *Service) Signer() *Signer { return s.signer }

// emailPattern mirrors the CHECK constraint on auth.users.email.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// NormalizeEmail lower-cases and validates an address.
func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > 254 || !emailPattern.MatchString(email) {
		return "", fmt.Errorf("%w: invalid email", ErrInvalidInput)
	}
	return email, nil
}

// Register creates a user with a spot account and returns a session.
func (s *Service) Register(ctx context.Context, email, password, ip string) (Session, error) {
	if s.signer == nil {
		return Session{}, errors.New("auth: this role cannot issue tokens")
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return Session{}, err
	}
	hash, err := HashPassword(password, s.cfg.Password)
	if err != nil {
		return Session{}, err
	}
	var sess Session
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		u, err := sqlcgen.New(tx).CreateUser(ctx, sqlcgen.CreateUserParams{TenantID: s.cfg.Tenant, Email: email, PasswordHash: hash, Role: RoleUser})
		if err != nil {
			if pgErrorCode(err) == "23505" {
				return ErrEmailTaken
			}
			return fmt.Errorf("auth: create user: %w", err)
		}
		acct, err := s.ledger.CreateSpotAccount(ctx, tx, &u.ID)
		if err != nil {
			return err
		}
		if err := s.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorUser, ActorID: u.ID, Action: "auth.register", TargetType: "user", TargetID: u.ID,
			After: map[string]any{"email": email, "account_id": acct.ID}, IP: ip,
		}); err != nil {
			return err
		}
		sess, err = s.newSession(ctx, tx, userFromRow(u), acct.ID)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// Login verifies the password and returns a session. Unknown emails and
// wrong passwords are indistinguishable and take the same time.
func (s *Service) Login(ctx context.Context, email, password, ip string) (Session, error) {
	if s.signer == nil {
		return Session{}, errors.New("auth: this role cannot issue tokens")
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return Session{}, ErrInvalidCredentials
	}
	q := sqlcgen.New(s.pool)
	row, err := q.GetUserByEmail(ctx, sqlcgen.GetUserByEmailParams{TenantID: s.cfg.Tenant, Email: email})
	hash := s.dummy
	found := err == nil
	if found {
		hash = row.PasswordHash
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Session{}, fmt.Errorf("auth: get user: %w", err)
	}
	ok, err := VerifyPassword(hash, password)
	if err != nil {
		return Session{}, err
	}
	if !found || !ok {
		if found {
			// The login is rejected either way; an audit write that fails must
			// not turn a wrong password into a 500. But it must not vanish
			// silently either -- these rows are what a brute-force
			// investigation reads.
			if err := s.audit.Record(ctx, s.pool, audit.Event{ActorType: audit.ActorUser, ActorID: row.ID, Action: "auth.login.failed", TargetType: "user", TargetID: row.ID, IP: ip}); err != nil {
				s.log.Warn("audit write failed", "action", "auth.login.failed", "user_id", row.ID, "error", err)
			}
		}
		return Session{}, ErrInvalidCredentials
	}
	u := userFromRow(row)
	// After the password, not before: a frozen user's wrong password is still
	// a wrong password, and saying "frozen" to it would confirm the account.
	if u.Status != StatusActive {
		if err := s.audit.Record(ctx, s.pool, audit.Event{ActorType: audit.ActorUser, ActorID: u.ID, Action: "auth.login.failed", TargetType: "user", TargetID: u.ID, IP: ip, After: map[string]any{"reason": "user " + u.Status}}); err != nil {
			s.log.Warn("audit write failed", "action", "auth.login.failed", "user_id", u.ID, "reason", u.Status, "error", err)
		}
		return Session{}, ErrUserFrozen
	}
	acct, err := s.ledger.SpotAccountOf(ctx, s.pool, u.ID)
	if err != nil {
		return Session{}, err
	}
	var sess Session
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := s.audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorUser, ActorID: u.ID, Action: "auth.login", TargetType: "user", TargetID: u.ID, IP: ip}); err != nil {
			return err
		}
		var err error
		sess, err = s.newSession(ctx, tx, u, acct.ID)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// Refresh rotates a refresh token: the old one is revoked, a new pair is
// issued. Presenting an already-rotated token revokes every token of the
// user (reuse means theft).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	if s.signer == nil {
		return Session{}, errors.New("auth: this role cannot issue tokens")
	}
	q := sqlcgen.New(s.pool)
	row, err := q.GetRefreshTokenByHash(ctx, hashToken(refreshToken))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrInvalidToken
		}
		return Session{}, fmt.Errorf("auth: get refresh token: %w", err)
	}
	now := s.now()
	if row.RevokedAt.Valid {
		// A rotated token (replaced_by set) presented again means the old
		// token leaked: revoke the whole family. A token revoked by logout or
		// by an earlier family revocation is simply invalid.
		//
		// The revocation's error is returned, not swallowed. This used to read
		// `if _, err := ...; err == nil { audit }`, which used the error only
		// to decide whether to write the audit row -- so a failed revocation
		// left the thief's other tokens live, wrote nothing anywhere, and
		// answered with the same ErrInvalidToken an ordinary expired token
		// gets. A security response that could not be carried out must not
		// look like one that was: the caller gets a 500 and the operator gets
		// a real error, because the alternative is a silent, invisible
		// compromise. AdminLogout has always done it this way.
		if row.ReplacedBy != nil {
			if _, err := q.RevokeUserRefreshTokens(ctx, row.UserID); err != nil {
				return Session{}, fmt.Errorf("auth: revoke refresh token family of %s after reuse: %w", row.UserID, err)
			}
			// The family is revoked by the time we get here, so the security
			// response has happened; losing the row loses the record of why,
			// which is worth a loud line.
			if err := s.audit.Record(ctx, s.pool, audit.Event{ActorType: audit.ActorSystem, ActorID: "auth", Action: "auth.refresh.reuse_detected", TargetType: "user", TargetID: row.UserID}); err != nil {
				s.log.Error("refresh token reuse detected but the audit write failed", "user_id", row.UserID, "error", err)
			}
		}
		return Session{}, ErrInvalidToken
	}
	if !row.ExpiresAt.After(now) {
		return Session{}, ErrInvalidToken
	}
	urow, err := q.GetUser(ctx, row.UserID)
	if err != nil {
		return Session{}, fmt.Errorf("auth: get user: %w", err)
	}
	u := userFromRow(urow)
	if u.Status != StatusActive {
		return Session{}, ErrUserFrozen
	}
	acct, err := s.ledger.SpotAccountOf(ctx, s.pool, u.ID)
	if err != nil {
		return Session{}, err
	}
	var sess Session
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		sess, err = s.newSession(ctx, tx, u, acct.ID)
		if err != nil {
			return err
		}
		newRow, err := sqlcgen.New(tx).GetRefreshTokenByHash(ctx, hashToken(sess.RefreshToken))
		if err != nil {
			return fmt.Errorf("auth: new refresh token: %w", err)
		}
		n, err := sqlcgen.New(tx).RevokeRefreshToken(ctx, sqlcgen.RevokeRefreshTokenParams{ID: row.ID, ReplacedBy: &newRow.ID})
		if err != nil {
			return fmt.Errorf("auth: revoke refresh token: %w", err)
		}
		if n != 1 {
			return ErrInvalidToken // raced with another rotation
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}

// Logout revokes a refresh token (idempotent; unknown tokens are ignored).
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	q := sqlcgen.New(s.pool)
	row, err := q.GetRefreshTokenByHash(ctx, hashToken(refreshToken))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("auth: get refresh token: %w", err)
	}
	if _, err := q.RevokeRefreshToken(ctx, sqlcgen.RevokeRefreshTokenParams{ID: row.ID}); err != nil {
		return fmt.Errorf("auth: revoke refresh token: %w", err)
	}
	return nil
}

// newSession issues an access token and stores a new refresh token in tx.
func (s *Service) newSession(ctx context.Context, tx pgx.Tx, u User, accountID string) (Session, error) {
	now := s.now()
	access, err := s.signer.Issue(Claims{
		UserID: u.ID, AccountID: accountID, TenantID: u.TenantID, Role: u.Role, Scopes: AllScopes, Audience: AudiencePublic, Method: MethodJWT,
	}, now, s.cfg.AccessTTL)
	if err != nil {
		return Session{}, err
	}
	raw, err := newOpaqueToken()
	if err != nil {
		return Session{}, err
	}
	if _, err := sqlcgen.New(tx).InsertRefreshToken(ctx, sqlcgen.InsertRefreshTokenParams{
		TenantID: u.TenantID, UserID: u.ID, TokenHash: hashToken(raw), ExpiresAt: now.Add(s.cfg.RefreshTTL),
	}); err != nil {
		return Session{}, fmt.Errorf("auth: insert refresh token: %w", err)
	}
	return Session{
		UserID: u.ID, AccountID: accountID, Role: u.Role, AccessToken: access, RefreshToken: raw,
		ExpiresAt: now.Add(s.cfg.AccessTTL), ExpiresIn: s.cfg.AccessTTL,
	}, nil
}

// ErrAPIKeysDisabled is returned when API_KEY_MASTER_KEY is not configured.
var ErrAPIKeysDisabled = errors.New("auth: API keys are disabled (API_KEY_MASTER_KEY not set)")

// VerifyAccessToken authenticates a bearer token for the public audience.
func (s *Service) VerifyAccessToken(token string) (Principal, error) {
	if s.verifier == nil {
		return Principal{}, fmt.Errorf("%w: no verifier configured", ErrInvalidToken)
	}
	c, err := s.verifier.Verify(token, AudiencePublic, s.now())
	if err != nil {
		return Principal{}, err
	}
	if c.TenantID != s.cfg.Tenant {
		return Principal{}, fmt.Errorf("%w: tenant mismatch", ErrInvalidToken)
	}
	return c.Principal(), nil
}

// CreateAPIKey issues a key for the user. The secret is returned exactly
// once and stored encrypted.
func (s *Service) CreateAPIKey(ctx context.Context, userID, label string, scopes []string, ipAllowlist []string) (APIKey, string, error) {
	if len(s.cfg.MasterKey) == 0 {
		return APIKey{}, "", ErrAPIKeysDisabled
	}
	sc, err := ParseScopes(scopes)
	if err != nil {
		return APIKey{}, "", err
	}
	if len(label) > 64 {
		return APIKey{}, "", fmt.Errorf("%w: label too long", ErrInvalidInput)
	}
	ips := make([]string, 0, len(ipAllowlist))
	for _, ip := range ipAllowlist {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if _, err := parseIP(ip); err != nil {
			return APIKey{}, "", fmt.Errorf("%w: ip allowlist entry %q", ErrInvalidInput, ip)
		}
		ips = append(ips, ip)
	}
	keyID, err := newKeyID()
	if err != nil {
		return APIKey{}, "", err
	}
	secret, err := newSecret()
	if err != nil {
		return APIKey{}, "", err
	}
	sealed, err := sealSecret(s.apiKeys, secret)
	if err != nil {
		return APIKey{}, "", err
	}
	scopeStrs := make([]string, 0, len(sc))
	for _, x := range sc {
		scopeStrs = append(scopeStrs, string(x))
	}
	var out APIKey
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).InsertAPIKey(ctx, sqlcgen.InsertAPIKeyParams{
			TenantID: s.cfg.Tenant, UserID: userID, KeyID: keyID, SecretEnc: sealed, Label: label, Scopes: scopeStrs, IpAllowlist: ips,
		})
		if err != nil {
			return fmt.Errorf("auth: insert api key: %w", err)
		}
		out = apiKeyFromRow(row)
		err = s.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorUser, ActorID: userID, Action: "auth.api_key.create", TargetType: "api_key", TargetID: row.ID,
			After: map[string]any{"key_id": keyID, "scopes": scopeStrs, "ip_allowlist": ips, "label": label},
		})
		return err
	})
	if err != nil {
		return APIKey{}, "", err
	}
	return out, secret, nil
}

// ListAPIKeys lists the user's keys, revoked ones included.
func (s *Service) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := sqlcgen.New(s.pool).ListAPIKeysByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list api keys: %w", err)
	}
	out := make([]APIKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, apiKeyFromRow(r))
	}
	return out, nil
}

// RevokeAPIKey revokes one of the user's keys; ErrNotFound for unknown,
// foreign or already revoked keys.
func (s *Service) RevokeAPIKey(ctx context.Context, userID, id string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		n, err := sqlcgen.New(tx).RevokeAPIKey(ctx, sqlcgen.RevokeAPIKeyParams{ID: id, UserID: userID})
		if err != nil {
			if pgErrorCode(err) == "22P02" { // not a uuid
				return ErrNotFound
			}
			return fmt.Errorf("auth: revoke api key: %w", err)
		}
		if n != 1 {
			return ErrNotFound
		}
		err = s.audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorUser, ActorID: userID, Action: "auth.api_key.revoke", TargetType: "api_key", TargetID: id})
		return err
	})
}

// VerifyAPIKeyRequest authenticates an HMAC-signed request and returns the
// principal (scopes from the key, account from the owner).
func (s *Service) VerifyAPIKeyRequest(ctx context.Context, r APIKeyRequest) (Principal, error) {
	if len(s.cfg.MasterKey) == 0 {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidCredentials, ErrAPIKeysDisabled)
	}
	if r.KeyID == "" || r.Timestamp == "" || r.Signature == "" {
		return Principal{}, fmt.Errorf("%w: %s, %s and %s are required", ErrInvalidCredentials, HeaderAPIKey, HeaderAPITimestamp, HeaderAPISignature)
	}
	q := sqlcgen.New(s.pool)
	row, err := q.GetAPIKeyByKeyID(ctx, r.KeyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Principal{}, fmt.Errorf("%w: unknown api key", ErrInvalidCredentials)
		}
		return Principal{}, fmt.Errorf("auth: get api key: %w", err)
	}
	if row.RevokedAt.Valid || row.TenantID != s.cfg.Tenant {
		return Principal{}, fmt.Errorf("%w: api key revoked", ErrInvalidCredentials)
	}
	secret, err := openSecret(s.apiKeys, row.SecretEnc)
	if err != nil {
		return Principal{}, err
	}
	if err := verifySignature(secret, r.Timestamp, r.Signature, r.Method, r.RequestURI, r.Body, s.now()); err != nil {
		return Principal{}, err
	}
	if len(row.IpAllowlist) > 0 && !ipAllowed(r.IP, row.IpAllowlist) {
		return Principal{}, fmt.Errorf("%w: ip not allowed", ErrForbidden)
	}
	urow, err := q.GetUser(ctx, row.UserID)
	if err != nil {
		return Principal{}, fmt.Errorf("auth: get user: %w", err)
	}
	if urow.Status != StatusActive {
		return Principal{}, ErrUserFrozen
	}
	acct, err := s.ledger.SpotAccountOf(ctx, s.pool, row.UserID)
	if err != nil {
		return Principal{}, err
	}
	_ = q.TouchAPIKey(ctx, row.ID)
	scopes, err := ParseScopes(row.Scopes)
	if err != nil {
		return Principal{}, err
	}
	return Principal{
		UserID: urow.ID, AccountID: acct.ID, TenantID: urow.TenantID, Role: urow.Role, Method: MethodAPIKey, APIKeyID: row.ID, Scopes: scopes,
	}, nil
}

// User returns a user by id.
func (s *Service) User(ctx context.Context, id string) (User, error) {
	row, err := sqlcgen.New(s.pool).GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || pgErrorCode(err) == "22P02" {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("auth: get user: %w", err)
	}
	if row.TenantID != s.cfg.Tenant {
		return User{}, ErrNotFound
	}
	return userFromRow(row), nil
}

// BootstrapAdmin creates the first administrator (ADMIN_BOOTSTRAP_EMAIL /
// _PASSWORD) with a spot account. It is idempotent: an existing user with
// that email is left untouched and created=false is returned.
func (s *Service) BootstrapAdmin(ctx context.Context, email, password string) (User, bool, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return User{}, false, err
	}
	if row, err := sqlcgen.New(s.pool).GetUserByEmail(ctx, sqlcgen.GetUserByEmailParams{TenantID: s.cfg.Tenant, Email: email}); err == nil {
		return userFromRow(row), false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, fmt.Errorf("auth: get user: %w", err)
	}
	hash, err := HashPassword(password, s.cfg.Password)
	if err != nil {
		return User{}, false, err
	}
	var u User
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		row, err := sqlcgen.New(tx).CreateUser(ctx, sqlcgen.CreateUserParams{TenantID: s.cfg.Tenant, Email: email, PasswordHash: hash, Role: RoleAdmin})
		if err != nil {
			if pgErrorCode(err) == "23505" {
				return ErrEmailTaken
			}
			return fmt.Errorf("auth: create admin: %w", err)
		}
		if _, err := s.ledger.CreateSpotAccount(ctx, tx, &row.ID); err != nil {
			return err
		}
		u = userFromRow(row)
		err = s.audit.Record(ctx, tx, audit.Event{ActorType: audit.ActorSystem, ActorID: "bootstrap", Action: "auth.admin.bootstrap", TargetType: "user", TargetID: row.ID, After: map[string]any{"email": email}})
		return err
	})
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

// --- helpers ---

func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: commit: %w", err)
	}
	return nil
}

// newOpaqueToken returns a 32-byte random token, hex-encoded.
func newOpaqueToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// hashToken is how refresh tokens are stored: a leaked table yields nothing usable.
func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func userFromRow(r sqlcgen.AuthUser) User {
	return User{
		ID: r.ID, TenantID: r.TenantID, Email: r.Email, Role: r.Role, KYCLevel: int(r.KycLevel), Status: r.Status,
		TOTPEnabled: r.TotpEnabled, Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func apiKeyFromRow(r sqlcgen.AuthApiKey) APIKey {
	k := APIKey{ID: r.ID, KeyID: r.KeyID, Label: r.Label, IPAllowlist: slices.Clone(r.IpAllowlist), CreatedAt: r.CreatedAt}
	for _, s := range r.Scopes {
		k.Scopes = append(k.Scopes, Scope(s))
	}
	if r.LastUsedAt.Valid {
		t := r.LastUsedAt.Time
		k.LastUsedAt = &t
	}
	if r.RevokedAt.Valid {
		t := r.RevokedAt.Time
		k.RevokedAt = &t
	}
	return k
}

func pgErrorCode(err error) string {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState()
	}
	return ""
}

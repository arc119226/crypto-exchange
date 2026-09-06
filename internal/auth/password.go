// Package auth is the engine's minimal, replaceable authentication
// (docs/plan-v1.0.md §3.1 category B, ADR-0006): a users directory with
// argon2id passwords, Ed25519-signed JWT access tokens published through
// JWKS, rotating refresh tokens stored as hashes, and HMAC-signed API keys
// with scopes. Everything downstream (trading, ledger) only ever sees the
// account id carried by the Principal.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// PasswordParams are the argon2id cost parameters (RFC 9106 §4, the
// low-memory recommendation). Tests use TestPasswordParams to stay fast.
type PasswordParams struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
	KeyLen  uint32
}

// DefaultPasswordParams is what production hashes with (~40–80 ms per login).
var DefaultPasswordParams = PasswordParams{Time: 3, Memory: 64 * 1024, Threads: 1, KeyLen: 32}

// TestPasswordParams is deliberately cheap; never use it outside tests.
var TestPasswordParams = PasswordParams{Time: 1, Memory: 8 * 1024, Threads: 1, KeyLen: 32}

// Password policy.
const (
	MinPasswordLen = 8
	MaxPasswordLen = 128
	saltLen        = 16
)

// Errors.
var (
	ErrWeakPassword = errors.New("auth: password must be 8..128 characters")
	ErrBadHash      = errors.New("auth: malformed password hash")
)

// ValidatePassword enforces the length policy (complexity rules add little;
// rate limiting and argon2id do the work).
func ValidatePassword(p string) error {
	if n := len(p); n < MinPasswordLen || n > MaxPasswordLen {
		return ErrWeakPassword
	}
	return nil
}

// HashPassword returns a PHC-formatted argon2id string:
// $argon2id$v=19$m=<KiB>,t=<iters>,p=<threads>$<salt>$<hash> (base64, no padding).
// The parameters travel with the hash so they can be raised later.
func HashPassword(password string, params PasswordParams) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Threads, params.KeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, params.Memory, params.Time, params.Threads,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the PHC hash. It takes
// the same time whether or not the password matches.
func VerifyPassword(hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrBadHash
	}
	var p PasswordParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return false, ErrBadHash
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, ErrBadHash
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want))) //nolint:gosec // len(want) is small
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

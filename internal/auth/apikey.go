package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
)

// Request headers of the API key scheme (ADR-0006).
const (
	HeaderAPIKey       = "X-API-KEY"
	HeaderAPITimestamp = "X-API-TIMESTAMP" // unix milliseconds
	HeaderAPISignature = "X-API-SIGNATURE" // hex(HMAC-SHA256(secret, canonical))
)

// MaxTimestampSkew is how far a request timestamp may be from server time.
const MaxTimestampSkew = 30 * time.Second

// keyIDPrefix makes key ids recognisable in logs and configs.
const keyIDPrefix = "ak_"

// MasterKeyLen is the AES-256 key length API_KEY_MASTER_KEY must decode to.
const MasterKeyLen = 32

// ParseMasterKey decodes the hex master key used to encrypt API secrets at
// rest. The server needs the plaintext secret to verify HMAC signatures, so
// hashing (as done for passwords) is not an option.
func ParseMasterKey(hexKey string) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(k) != MasterKeyLen {
		return nil, fmt.Errorf("auth: API_KEY_MASTER_KEY must be %d bytes hex-encoded", MasterKeyLen)
	}
	return k, nil
}

// newKeyID returns a fresh public key identifier.
func newKeyID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: key id: %w", err)
	}
	return keyIDPrefix + hex.EncodeToString(b[:]), nil
}

// newSecret returns a fresh 32-byte secret, hex-encoded (shown to the user
// exactly once).
func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// sealSecret seals the secret under the current master key: nonce ||
// ciphertext.
//
// The envelope moved to internal/platform/secretbox when webhook endpoints
// needed the identical one. The format is unchanged, so rows written before
// the move still open.
func sealSecret(keys secretbox.Keyring, secret string) ([]byte, error) {
	sealed, err := keys.Seal(secret)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return sealed, nil
}

// openSecret is the inverse of sealSecret, under the current key or, during
// a rotation, the previous one.
func openSecret(keys secretbox.Keyring, sealed []byte) (string, error) {
	secret, err := keys.Open(sealed)
	if err != nil {
		return "", fmt.Errorf("auth: %w", err)
	}
	return secret, nil
}

// CanonicalRequest is the string a client signs:
//
//	<timestamp_ms> "\n" <METHOD> "\n" <request URI: path plus "?" query when present> "\n" <body>
//
// The body is the raw bytes sent (empty for GET/DELETE).
func CanonicalRequest(timestamp string, method, requestURI string, body []byte) []byte {
	var b strings.Builder
	b.Grow(len(timestamp) + len(method) + len(requestURI) + len(body) + 3)
	b.WriteString(timestamp)
	b.WriteByte('\n')
	b.WriteString(strings.ToUpper(method))
	b.WriteByte('\n')
	b.WriteString(requestURI)
	b.WriteByte('\n')
	b.Write(body)
	return []byte(b.String())
}

// SignRequest computes the X-API-SIGNATURE value; clients (and exchangectl)
// use it, the server uses it to verify.
func SignRequest(secret, timestamp, method, requestURI string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(CanonicalRequest(timestamp, method, requestURI, body))
	return hex.EncodeToString(mac.Sum(nil))
}

// verifySignature checks the timestamp window and the HMAC in constant time.
func verifySignature(secret, timestamp, signature, method, requestURI string, body []byte, now time.Time) error {
	ms, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %s must be unix milliseconds", ErrInvalidCredentials, HeaderAPITimestamp)
	}
	ts := time.UnixMilli(ms)
	if d := now.Sub(ts); d > MaxTimestampSkew || d < -MaxTimestampSkew {
		return fmt.Errorf("%w: timestamp outside the ±%s window", ErrInvalidCredentials, MaxTimestampSkew)
	}
	want := SignRequest(secret, timestamp, method, requestURI, body)
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(signature)), []byte(want)) != 1 {
		return fmt.Errorf("%w: bad signature", ErrInvalidCredentials)
	}
	return nil
}

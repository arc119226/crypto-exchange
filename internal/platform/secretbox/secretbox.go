// Package secretbox seals a short secret under a 32-byte master key.
//
// It exists because two packages need the identical envelope -- auth for API
// key secrets, webhook for endpoint signing secrets -- and a second
// implementation of AES-256-GCM is a second thing to get wrong. The format is
// nonce || ciphertext, which is what auth.api_keys.secret_enc has held since
// 0007, so this is the shape already in production data.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeySize is the master key length AES-256 requires.
const KeySize = 32

// Seal returns nonce || ciphertext. A fresh random nonce per call is what
// makes sealing the same secret twice produce different bytes, and reusing
// one under GCM would leak the plaintext difference.
func Seal(master []byte, secret string) ([]byte, error) {
	gcm, err := newGCM(master)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secretbox: nonce: %w", err)
	}
	return append(nonce, gcm.Seal(nil, nonce, []byte(secret), nil)...), nil
}

// Open is the inverse of Seal. A wrong key and a tampered ciphertext fail the
// same way, because GCM authenticates before it decrypts.
func Open(master, sealed []byte) (string, error) {
	gcm, err := newGCM(master)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("secretbox: sealed secret too short")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("secretbox: decrypt: %w", err)
	}
	return string(plain), nil
}

func newGCM(master []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, fmt.Errorf("secretbox: cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: gcm: %w", err)
	}
	return gcm, nil
}

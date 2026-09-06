package hdwallet

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// SeedFileName is the file the signer reads out of WALLET_KEYSTORE_DIR.
const SeedFileName = "hd-seed.json"

// keystoreVersion is bumped only if the on-disk shape changes incompatibly.
const keystoreVersion = 1

// go-ethereum's V3 keystore can only hold a single secp256k1 private key, not
// a BIP-39 seed, so this is a small format of our own (docs/plan-v1.0.md
// §14): scrypt over the passphrase, AES-256-GCM over the secret.
//
// The plan says to seal the BIP-39 *entropy*. We seal the mnemonic instead:
// it is the same secret in a different encoding, and cosmos/go-bip39 has no
// EntropyFromMnemonic — its MnemonicToByteArray returns entropy||checksum
// shifted by the checksum width, so recovering raw entropy would mean
// hand-rolling BIP-39 bit math in the one code path that must not have a
// subtle bug. Sealing the words also means an operator who opens the file
// during a recovery gets something they can paste into a wallet.
const (
	kdfScrypt    = "scrypt"
	cipherAESGCM = "aes-256-gcm"
)

// ScryptParams are the work factors recorded in the file, so a key encrypted
// today can still be opened after the defaults are raised.
type ScryptParams struct {
	N     int    `json:"n"`
	R     int    `json:"r"`
	P     int    `json:"p"`
	DKLen int    `json:"dklen"`
	Salt  string `json:"salt"` // hex
}

// DefaultScrypt is the production setting from docs/plan-v1.0.md §14:
// N = 2^18 costs roughly 256 MiB and about a second, which is the point.
// Tests pass a cheaper set; nothing else should.
func DefaultScrypt() ScryptParams { return ScryptParams{N: 1 << 18, R: 8, P: 1, DKLen: 32} }

// TestScrypt is a deliberately weak setting for tests. Never use it for a
// file that holds a real mnemonic.
func TestScrypt() ScryptParams { return ScryptParams{N: 1 << 12, R: 8, P: 1, DKLen: 32} }

type keystoreFile struct {
	Version    int          `json:"version"`
	KDF        string       `json:"kdf"`
	KDFParams  ScryptParams `json:"kdfparams"`
	Cipher     string       `json:"cipher"`
	Nonce      string       `json:"nonce"`      // hex
	Ciphertext string       `json:"ciphertext"` // hex, includes the GCM tag
}

// aad binds the header to the ciphertext: an attacker who can rewrite the
// file cannot lower N and have the same ciphertext still open.
func (f keystoreFile) aad() []byte {
	return fmt.Appendf(nil, "hd-seed/v%d/%s/n=%d,r=%d,p=%d,dklen=%d/%s",
		f.Version, f.KDF, f.KDFParams.N, f.KDFParams.R, f.KDFParams.P, f.KDFParams.DKLen, f.Cipher)
}

// Encrypt seals a BIP-39 mnemonic under a passphrase.
func Encrypt(mnemonic, passphrase string, params ScryptParams) ([]byte, error) {
	if mnemonic == "" {
		return nil, errors.New("hdwallet: empty mnemonic")
	}
	if strings.Join(strings.Fields(mnemonic), " ") == AnvilDefaultMnemonic {
		return nil, ErrAnvilMnemonic
	}
	if passphrase == "" {
		return nil, errors.New("hdwallet: empty passphrase (set WALLET_KEYSTORE_PASSPHRASE)")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("hdwallet: salt: %w", err)
	}
	params.Salt = hex.EncodeToString(salt)
	if err := params.validate(); err != nil {
		return nil, err
	}
	file := keystoreFile{Version: keystoreVersion, KDF: kdfScrypt, KDFParams: params, Cipher: cipherAESGCM}

	aead, err := aeadFor(passphrase, params)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("hdwallet: nonce: %w", err)
	}
	file.Nonce = hex.EncodeToString(nonce)
	file.Ciphertext = hex.EncodeToString(aead.Seal(nil, nonce, []byte(mnemonic), file.aad()))

	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("hdwallet: encode: %w", err)
	}
	return append(out, '\n'), nil
}

// Decrypt opens a keystore file. A wrong passphrase and a tampered file are
// the same error: GCM authenticates the whole thing.
func Decrypt(data []byte, passphrase string) (string, error) {
	var file keystoreFile
	if err := json.Unmarshal(data, &file); err != nil {
		return "", fmt.Errorf("hdwallet: decode keystore: %w", err)
	}
	if file.Version != keystoreVersion {
		return "", fmt.Errorf("hdwallet: unsupported keystore version %d", file.Version)
	}
	if file.KDF != kdfScrypt || file.Cipher != cipherAESGCM {
		return "", fmt.Errorf("hdwallet: unsupported kdf %q / cipher %q", file.KDF, file.Cipher)
	}
	if err := file.KDFParams.validate(); err != nil {
		return "", err
	}
	nonce, err := hex.DecodeString(file.Nonce)
	if err != nil {
		return "", fmt.Errorf("hdwallet: nonce: %w", err)
	}
	ciphertext, err := hex.DecodeString(file.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("hdwallet: ciphertext: %w", err)
	}
	aead, err := aeadFor(passphrase, file.KDFParams)
	if err != nil {
		return "", err
	}
	if len(nonce) != aead.NonceSize() {
		return "", errors.New("hdwallet: bad nonce length")
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, file.aad())
	if err != nil {
		return "", errors.New("hdwallet: cannot open keystore (wrong passphrase or corrupt file)")
	}
	return string(plaintext), nil
}

// Save writes the keystore into dir. The file is 0600: scripts/gen-dev-secrets.sh
// relaxes it for the containers that bind mount it, which is a development
// concession, not the default.
func Save(dir, mnemonic, passphrase string, params ScryptParams) (string, error) {
	blob, err := Encrypt(mnemonic, passphrase, params)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("hdwallet: keystore dir: %w", err)
	}
	path := filepath.Join(dir, SeedFileName)
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return "", fmt.Errorf("hdwallet: write keystore: %w", err)
	}
	return path, nil
}

// Load decrypts the keystore in dir and returns a ready wallet.
func Load(dir, passphrase string) (*Wallet, error) {
	path := filepath.Join(dir, SeedFileName)
	blob, err := os.ReadFile(path) //nolint:gosec // the path is configuration, not user input
	if err != nil {
		return nil, fmt.Errorf("hdwallet: read %s: %w", path, err)
	}
	mnemonic, err := Decrypt(blob, passphrase)
	if err != nil {
		return nil, err
	}
	return FromMnemonic(mnemonic)
}

func aeadFor(passphrase string, p ScryptParams) (cipher.AEAD, error) {
	salt, err := hex.DecodeString(p.Salt)
	if err != nil {
		return nil, fmt.Errorf("hdwallet: salt: %w", err)
	}
	key, err := scrypt.Key([]byte(passphrase), salt, p.N, p.R, p.P, p.DKLen)
	if err != nil {
		return nil, fmt.Errorf("hdwallet: scrypt: %w", err)
	}
	block, err := aes.NewCipher(key)
	for i := range key {
		key[i] = 0
	}
	if err != nil {
		return nil, fmt.Errorf("hdwallet: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("hdwallet: gcm: %w", err)
	}
	return aead, nil
}

// validate bounds the work factors. Without an upper bound a file claiming
// N = 2^30 would let anyone who can write it OOM the signer on startup.
func (p ScryptParams) validate() error {
	switch {
	case p.N < 1<<12 || p.N > 1<<20 || p.N&(p.N-1) != 0:
		return fmt.Errorf("hdwallet: scrypt N must be a power of two in [2^12, 2^20], got %d", p.N)
	case p.R < 1 || p.R > 16:
		return fmt.Errorf("hdwallet: scrypt r out of range: %d", p.R)
	case p.P < 1 || p.P > 4:
		return fmt.Errorf("hdwallet: scrypt p out of range: %d", p.P)
	case p.DKLen != 32:
		return fmt.Errorf("hdwallet: scrypt dklen must be 32 (AES-256), got %d", p.DKLen)
	case len(p.Salt) < 32:
		return errors.New("hdwallet: scrypt salt too short")
	}
	return nil
}

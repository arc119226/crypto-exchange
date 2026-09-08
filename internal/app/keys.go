package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// Key rotation helpers behind `exchange keys` (docs/runbooks/key-rotation.md).

// RekeyOptions is `exchange keys rekey`.
type RekeyOptions struct {
	KeystoreDir string
	// Passphrase is the one the keystore is under now (WALLET_KEYSTORE_PASSPHRASE).
	Passphrase string
	// NewPassphrase is the one it will be under (WALLET_KEYSTORE_NEW_PASSPHRASE).
	NewPassphrase string
	// Scrypt overrides the KDF cost (tests); zero means hdwallet.DefaultScrypt.
	Scrypt hdwallet.ScryptParams
}

// RekeyKeystore re-encrypts hd-seed.json under a new passphrase and returns
// the hot wallet address so the operator can check nothing else changed.
// The seed itself is untouched: a new passphrase changes which secret opens
// the file, not the addresses derived from it.
//
// The new file is written next to the old one, read back and decrypted
// under the new passphrase, and only then renamed over the old one -- so
// the keystore is never in a state where neither passphrase opens it, and
// a wrong new passphrase in the operator's environment cannot lock the
// seed away.
func RekeyKeystore(opts RekeyOptions) (hotWallet string, err error) {
	if opts.KeystoreDir == "" {
		return "", errors.New("keys: --keystore-dir is required")
	}
	if opts.Passphrase == "" || opts.NewPassphrase == "" {
		return "", errors.New("keys: WALLET_KEYSTORE_PASSPHRASE (current) and WALLET_KEYSTORE_NEW_PASSPHRASE are required")
	}
	if opts.NewPassphrase == opts.Passphrase {
		return "", errors.New("keys: the new passphrase is the current one")
	}
	path := filepath.Join(opts.KeystoreDir, hdwallet.SeedFileName)
	blob, err := os.ReadFile(path) //nolint:gosec // the path is configuration, not user input
	if err != nil {
		return "", fmt.Errorf("keys: read keystore: %w", err)
	}
	mnemonic, err := hdwallet.Decrypt(blob, opts.Passphrase)
	if err != nil {
		return "", fmt.Errorf("keys: current passphrase: %w", err)
	}
	w, err := hdwallet.FromMnemonic(mnemonic)
	if err != nil {
		return "", err
	}
	defer w.Close()
	addr, err := w.Address(hdwallet.HotWalletPath())
	if err != nil {
		return "", err
	}
	kdf := opts.Scrypt
	if kdf.N == 0 {
		kdf = hdwallet.DefaultScrypt()
	}
	out, err := hdwallet.Encrypt(mnemonic, opts.NewPassphrase, kdf)
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil { //nolint:gosec // built from the configured keystore dir
		return "", fmt.Errorf("keys: write keystore: %w", err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp)
		}
	}()
	// Read back what is on disk, not the bytes in memory: the check is that
	// the file the signer will load opens under the passphrase it will get.
	written, err := os.ReadFile(tmp) //nolint:gosec // the path was built above
	if err != nil {
		return "", fmt.Errorf("keys: read back keystore: %w", err)
	}
	back, err := hdwallet.Decrypt(written, opts.NewPassphrase)
	if err != nil {
		return "", fmt.Errorf("keys: the new keystore does not open under the new passphrase: %w", err)
	}
	if back != mnemonic {
		return "", errors.New("keys: the new keystore decrypts to a different seed")
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("keys: replace keystore: %w", err)
	}
	renamed = true
	return addr.Hex(), nil
}

// JWTPublicKey reads the key at path (the PKCS#8 file gen-jwt writes, or a
// public key already) and returns its SPKI PEM and the kid it is published
// under -- what `exchange keys jwt-public` prints for the first phase of a
// rotation and for checking which key a JWKS is serving.
func JWTPublicKey(path string) (pemBytes []byte, kid string, err error) {
	pub, err := auth.LoadPublicKey(path)
	if err != nil {
		return nil, "", err
	}
	pemBytes, err = auth.PublicKeyPEM(pub)
	if err != nil {
		return nil, "", err
	}
	kid, err = auth.KeyIDOf(pub)
	if err != nil {
		return nil, "", err
	}
	return pemBytes, kid, nil
}

// RewrapDomain names one set of rows sealed under one master key.
type RewrapDomain string

// The domains `exchange keys rewrap --domain` accepts.
const (
	RewrapAPIKeys RewrapDomain = "api-keys" // auth.api_keys.secret_enc under API_KEY_MASTER_KEY
	RewrapWebhook RewrapDomain = "webhook"  // webhook.endpoints.{secret_enc,previous_secret_enc} under WEBHOOK_SIGNING_KEY
	RewrapTOTP    RewrapDomain = "totp"     // auth.users.totp_secret_enc under ADMIN_TOTP_KEY
)

// RewrapDomains lists them in the order the runbook rotates them.
var RewrapDomains = []RewrapDomain{RewrapAPIKeys, RewrapWebhook, RewrapTOTP}

// ParseRewrapDomain accepts one of RewrapDomains.
func ParseRewrapDomain(s string) (RewrapDomain, error) {
	for _, d := range RewrapDomains {
		if string(d) == strings.TrimSpace(s) {
			return d, nil
		}
	}
	return "", fmt.Errorf("keys: unknown domain %q (one of %s)", s, rewrapDomainList())
}

func rewrapDomainList() string {
	names := make([]string, 0, len(RewrapDomains))
	for _, d := range RewrapDomains {
		names = append(names, string(d))
	}
	return strings.Join(names, ", ")
}

// RewrapOptions is `exchange keys rewrap`.
type RewrapOptions struct {
	DSN    string
	Tenant string
	Domain RewrapDomain
	// Keys is the domain's keyring: Current to seal under, Previous to open
	// with. Without a previous key the pass verifies that every row opens
	// under the current key and writes nothing.
	Keys secretbox.Keyring
}

// RewrapOptionsFor picks the keyring for the domain out of the
// configuration: the domain's current and previous master keys, and the
// process's DATABASE_URL, which the runbook points at ex_migrate.
func RewrapOptionsFor(cfg Config, domain RewrapDomain) (RewrapOptions, error) {
	var (
		decode func() (secretbox.Keyring, error)
		name   string
	)
	switch domain {
	case RewrapAPIKeys:
		decode, name = cfg.APIKeyKeys, "API_KEY_MASTER_KEY"
	case RewrapWebhook:
		decode, name = cfg.Webhook.Keys, "WEBHOOK_SIGNING_KEY"
	case RewrapTOTP:
		decode, name = cfg.Admin.TOTPKeys, "ADMIN_TOTP_KEY"
	default:
		return RewrapOptions{}, fmt.Errorf("keys: unknown domain %q", domain)
	}
	keys, err := decode()
	if err != nil {
		return RewrapOptions{}, err
	}
	if keys.Empty() {
		return RewrapOptions{}, fmt.Errorf("keys: rewrap %s: %s is not set", domain, name)
	}
	return RewrapOptions{DSN: cfg.DB.URL.Reveal(), Tenant: cfg.TenantID, Domain: domain, Keys: keys}, nil
}

// Rewrap re-seals every row of the domain under the current key in one
// transaction and records what it did in the audit trail. It is idempotent:
// a second run scans the same rows and changes none. A row that neither key
// opens aborts the transaction with nothing written, because a secret nobody
// can open is a fault to investigate, not a row to skip.
func Rewrap(ctx context.Context, opts RewrapOptions, out io.Writer) (secretbox.Rewrapped, error) {
	var n secretbox.Rewrapped
	if opts.DSN == "" {
		return n, errors.New("keys: rewrap: DATABASE_URL is required")
	}
	if err := opts.Keys.Validate(); err != nil {
		return n, fmt.Errorf("keys: rewrap %s: %w", opts.Domain, err)
	}
	if opts.Keys.Empty() {
		return n, fmt.Errorf("keys: rewrap %s: a current key is required", opts.Domain)
	}
	if opts.Tenant == "" {
		opts.Tenant = "default"
	}
	pool, err := pg.Open(ctx, pg.PoolConfig{DSN: opts.DSN, MaxConns: 2, ApplicationName: "exchange-rewrap"})
	if err != nil {
		return n, fmt.Errorf("keys: rewrap: %w", err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return n, fmt.Errorf("keys: rewrap: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	switch opts.Domain {
	case RewrapAPIKeys:
		n, err = auth.RewrapAPIKeys(ctx, tx, opts.Tenant, opts.Keys)
	case RewrapWebhook:
		n, err = webhook.RewrapSecrets(ctx, tx, opts.Tenant, opts.Keys)
	case RewrapTOTP:
		n, err = auth.RewrapTOTPSecrets(ctx, tx, opts.Tenant, opts.Keys)
	default:
		err = fmt.Errorf("keys: unknown domain %q", opts.Domain)
	}
	if err != nil {
		return n, err
	}
	err = audit.NewRecorder(opts.Tenant).Record(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, ActorID: "exchange keys rewrap",
		Action: "secrets.rewrap", TargetType: "secret_domain", TargetID: string(opts.Domain),
		After: map[string]any{"scanned": n.Scanned, "rewrapped": n.Changed, "previous_key": len(opts.Keys.Previous) > 0},
	})
	if err != nil {
		return n, fmt.Errorf("keys: rewrap: audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return n, fmt.Errorf("keys: rewrap: commit: %w", err)
	}
	_, _ = fmt.Fprintf(out, "rewrap %s: %d rows scanned, %d re-sealed under the current key\n", opts.Domain, n.Scanned, n.Changed)
	return n, nil
}

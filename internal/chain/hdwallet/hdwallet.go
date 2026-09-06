// Package hdwallet derives every EVM key the exchange owns from one BIP-39
// seed (docs/plan-v1.0.md §6.4.1, §14; ADR-0007).
//
// It is imported only by the signer role. No other role holds key material:
// the chain role asks the signer to sign, and the api role only hands out
// addresses the signer has already derived and stored (§8, the api row).
//
// Two derivation accounts, fixed by the plan:
//
//	m/44'/60'/0'/0/{i}   deposit addresses, one per user account
//	m/44'/60'/1'/0/0     the hot wallet that pays withdrawals and gas
package hdwallet

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	bip39 "github.com/cosmos/go-bip39"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// AnvilDefaultMnemonic is anvil's well-known development mnemonic. Its keys
// are public, so persisting it as our seed would put every deposit address
// one `anvil --help` away. Encrypt refuses it, and so does
// scripts/gen-dev-secrets.sh; both checks exist because either path can
// create the keystore.
//
// Deriving from it is deliberately allowed: anvil prints the keys of
// m/44'/60'/0'/0/{i} on startup, so its own output is an independent oracle
// for this package and the tests use it as one.
const AnvilDefaultMnemonic = "test test test test test test test test test test test junk"

// ErrAnvilMnemonic is returned when the anvil default mnemonic is persisted.
var ErrAnvilMnemonic = errors.New("hdwallet: refusing anvil's default mnemonic")

// hardened is the offset BIP-32 uses to mark a hardened child index.
const hardened uint32 = 0x80000000

// Path is a BIP-32 derivation path, hardened components already offset.
type Path []uint32

// MaxDepositIndex is the largest BIP-44 address_index. Anything above it
// collides with the hardened range: index 2^31 and hardened index 0 are the
// same 32-bit value, so two pool rows would derive one address.
const MaxDepositIndex = hardened - 1

// DepositPath returns m/44'/60'/0'/0/index, the address a user deposits to.
func DepositPath(index uint32) (Path, error) {
	if index > MaxDepositIndex {
		return nil, fmt.Errorf("hdwallet: deposit index %d exceeds the non-hardened range", index)
	}
	return Path{44 | hardened, 60 | hardened, 0 | hardened, 0, index}, nil
}

// HotWalletPath returns m/44'/60'/1'/0/0. scripts/gen-dev-secrets.sh derives
// the same path with `cast wallet address` and writes the result to
// HOT_WALLET_ADDRESS, which is what TestHotWalletMatchesCast checks against.
func HotWalletPath() Path {
	return Path{44 | hardened, 60 | hardened, 1 | hardened, 0, 0}
}

// String renders the path in the usual m/44'/60'/... notation.
func (p Path) String() string {
	parts := make([]string, 0, len(p)+1)
	parts = append(parts, "m")
	for _, c := range p {
		if c >= hardened {
			parts = append(parts, strconv.FormatUint(uint64(c-hardened), 10)+"'")
			continue
		}
		parts = append(parts, strconv.FormatUint(uint64(c), 10))
	}
	return strings.Join(parts, "/")
}

// Wallet is a decrypted seed. Hold exactly one per process and Close it on
// shutdown; it is safe for concurrent use because Derive never mutates it.
type Wallet struct {
	master *hdkeychain.ExtendedKey
}

// LogValue implements slog.LogValuer so a Wallet can never be logged.
func (*Wallet) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// String implements fmt.Stringer so accidental formatting is safe too.
func (*Wallet) String() string { return "[redacted]" }

// FromMnemonic builds a wallet from a BIP-39 mnemonic with an empty BIP-39
// passphrase — the same convention `cast wallet address --mnemonic` uses, so
// the two agree on every address.
func FromMnemonic(mnemonic string) (*Wallet, error) {
	// NewSeedWithErrorChecking, not IsMnemonicValid: the latter only checks
	// the word count and that every word is in the list, so it accepts a
	// mnemonic with one word swapped for another valid one. That would derive
	// a different wallet without a word of complaint — every deposit address
	// wrong, and the funds sent to them unreachable.
	seed, err := bip39.NewSeedWithErrorChecking(mnemonic, "")
	if err != nil {
		return nil, fmt.Errorf("hdwallet: invalid mnemonic: %w", err)
	}
	master, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		return nil, fmt.Errorf("hdwallet: master key: %w", err)
	}
	return &Wallet{master: master}, nil
}

// Derive returns the private key at path. The caller owns the key and should
// not keep it longer than the signature it is producing.
func (w *Wallet) Derive(path Path) (*ecdsa.PrivateKey, error) {
	key := w.master
	for i, child := range path {
		next, err := key.Derive(child)
		if err != nil {
			return nil, fmt.Errorf("hdwallet: derive %s at depth %d: %w", path, i, err)
		}
		if i > 0 { // never zero the master
			key.Zero()
		}
		key = next
	}
	defer key.Zero()
	priv, err := key.ECPrivKey()
	if err != nil {
		return nil, fmt.Errorf("hdwallet: private key at %s: %w", path, err)
	}
	out, err := crypto.ToECDSA(priv.Serialize())
	if err != nil {
		return nil, fmt.Errorf("hdwallet: to ecdsa at %s: %w", path, err)
	}
	return out, nil
}

// Address returns the EIP-55 address at path without exposing the key.
//
// It does not try to wipe the derived key afterwards. Go gives no way to do
// that reliably — the scalar lives in a big.Int the runtime is free to copy —
// and since Go 1.25 mutating ecdsa.PrivateKey.D is deprecated precisely
// because it can produce an invalid key. What we can control is that the key
// never leaves this package and never reaches a log line.
func (w *Wallet) Address(path Path) (common.Address, error) {
	priv, err := w.Derive(path)
	if err != nil {
		return common.Address{}, err
	}
	return crypto.PubkeyToAddress(priv.PublicKey), nil
}

// Close wipes the master key.
func (w *Wallet) Close() {
	if w.master != nil {
		w.master.Zero()
		w.master = nil
	}
}

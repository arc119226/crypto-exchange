package secretbox

import (
	"crypto/subtle"
	"errors"
	"fmt"
)

// Keyring is the master key in use and, while a rotation is under way, the
// one it replaced. Seal always uses Current; Open tries Current and then
// Previous. That order is what lets a role keep reading rows sealed before
// the rotation until `exchange keys rewrap` has rewritten them, and what
// makes the rewrap idempotent: a row that already opens under Current is
// left alone (docs/runbooks/key-rotation.md).
//
// The envelope carries no key id on purpose. Adding one would change the
// bytes every existing row holds; trying two keys costs one failed GCM open
// on a 32-byte secret, which is nothing.
type Keyring struct {
	Current  []byte
	Previous []byte
}

// Validate checks the shape: keys are KeySize bytes, a previous key needs a
// current one, and the two differ -- a "rotation" to the same key is a
// configuration mistake, not a rotation.
func (k Keyring) Validate() error {
	if len(k.Current) == 0 && len(k.Previous) == 0 {
		return nil
	}
	if len(k.Current) != KeySize {
		if len(k.Current) == 0 {
			return errors.New("secretbox: a previous key without a current one")
		}
		return fmt.Errorf("secretbox: current key must be %d bytes", KeySize)
	}
	if len(k.Previous) == 0 {
		return nil
	}
	if len(k.Previous) != KeySize {
		return fmt.Errorf("secretbox: previous key must be %d bytes", KeySize)
	}
	if subtle.ConstantTimeCompare(k.Current, k.Previous) == 1 {
		return errors.New("secretbox: the previous key is the current key")
	}
	return nil
}

// Empty reports whether there is no key at all.
func (k Keyring) Empty() bool { return len(k.Current) == 0 }

// Seal seals under the current key.
func (k Keyring) Seal(secret string) ([]byte, error) { return Seal(k.Current, secret) }

// Open opens under the current key, then the previous one. The error is the
// current key's: a blob neither key opens is reported as a wrong key, which
// is what it is.
func (k Keyring) Open(sealed []byte) (string, error) {
	secret, _, err := k.open(sealed)
	return secret, err
}

// Rewrap returns sealed re-sealed under the current key and whether that
// changed anything. A blob already under the current key comes back as it
// is, so a rewrap that runs twice writes nothing the second time.
func (k Keyring) Rewrap(sealed []byte) ([]byte, bool, error) {
	secret, previous, err := k.open(sealed)
	if err != nil {
		return nil, false, err
	}
	if !previous {
		return sealed, false, nil
	}
	out, err := Seal(k.Current, secret)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func (k Keyring) open(sealed []byte) (secret string, previous bool, err error) {
	secret, err = Open(k.Current, sealed)
	if err == nil {
		return secret, false, nil
	}
	if len(k.Previous) == 0 {
		return "", false, err
	}
	if s, perr := Open(k.Previous, sealed); perr == nil {
		return s, true, nil
	}
	return "", false, err
}

// Rewrapped counts what one rewrap pass saw and rewrote.
type Rewrapped struct {
	Scanned int
	Changed int
}

// Add folds another pass's counts in.
func (r *Rewrapped) Add(o Rewrapped) {
	r.Scanned += o.Scanned
	r.Changed += o.Changed
}

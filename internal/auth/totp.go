package auth

import (
	"bytes"
	"fmt"
	"image/png"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTP parameters (RFC 6238, the defaults every authenticator app assumes).
const (
	totpPeriod = 30 * time.Second
	totpDigits = otp.DigitsSix
	totpAlg    = otp.AlgorithmSHA1
	// totpSecretBytes is the raw secret length; 160 bits is what RFC 4226
	// recommends and what apps display as 32 base32 characters.
	totpSecretBytes = 20
)

// TOTPEnrolment is what `exchange admin totp enroll` hands the operator: the
// otpauth URL and secret their authenticator needs, and a QR of the same,
// shown once and never stored in the clear.
type TOTPEnrolment struct {
	Secret string // base32, what the app asks for on manual entry
	URL    string // otpauth://totp/...
	PNG    []byte // the URL as a QR code
}

// newTOTPKey generates a fresh secret for one administrator.
func newTOTPKey(issuer, email string) (*otp.Key, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer: issuer, AccountName: email, Period: uint(totpPeriod.Seconds()),
		Digits: totpDigits, Algorithm: totpAlg, SecretSize: totpSecretBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: totp: %w", err)
	}
	return key, nil
}

// qrPNG renders the key's otpauth URL as a QR code.
func qrPNG(key *otp.Key) ([]byte, error) {
	img, err := key.Image(256, 256)
	if err != nil {
		return nil, fmt.Errorf("auth: totp qr: %w", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("auth: totp qr: %w", err)
	}
	return buf.Bytes(), nil
}

// totpStep is the 30-second window a time falls in.
func totpStep(t time.Time) int64 { return t.Unix() / int64(totpPeriod.Seconds()) }

// totpMatch reports which step a code is valid for, checking the current step
// and one either side (clocks drift). It is written with Skew 0 per step
// instead of one call with Skew 1 because the library's answer is only
// yes/no, and the replay rule needs the number: the matched step is what gets
// recorded as spent, and a code for the step ahead (a client clock running
// fast) must be spent at *that* step, or it would be accepted again when the
// server's clock gets there.
func totpMatch(secret, code string, now time.Time) (step int64, ok bool) {
	for _, delta := range []time.Duration{0, -totpPeriod, totpPeriod} {
		at := now.Add(delta)
		valid, err := totp.ValidateCustom(code, secret, at, totp.ValidateOpts{
			Period: uint(totpPeriod.Seconds()), Skew: 0, Digits: totpDigits, Algorithm: totpAlg,
		})
		if err == nil && valid {
			return totpStep(at), true
		}
	}
	return 0, false
}

// TOTPCode computes the code for a secret at a time. Exported for the
// screenshot script and tests; nothing in the server calls it.
func TOTPCode(secret string, at time.Time) (string, error) {
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period: uint(totpPeriod.Seconds()), Skew: 0, Digits: totpDigits, Algorithm: totpAlg,
	})
	if err != nil {
		return "", fmt.Errorf("auth: totp: %w", err)
	}
	return code, nil
}

package telemetry

import (
	"log/slog"
	"net/url"
)

// Redacted is the placeholder written to logs in place of a secret.
const Redacted = "[REDACTED]"

// Secret is a string that never reaches a log line or a %v/%s format verb.
// Config fields holding passwords, passphrases and signing keys use it; the
// only way to read the value is Reveal().
type Secret string

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// String implements fmt.Stringer so accidental formatting is safe.
func (Secret) String() string { return Redacted }

// GoString makes %#v safe as well.
func (Secret) GoString() string { return Redacted }

// Reveal returns the underlying value. Call sites should be few and obvious.
func (s Secret) Reveal() string { return string(s) }

// IsSet reports whether the secret is non-empty.
func (s Secret) IsSet() bool { return s != "" }

// RedactURL strips the password from a connection string such as a Postgres
// DSN so it can be logged. Unparseable input is replaced entirely.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return Redacted
	}
	return u.Redacted()
}

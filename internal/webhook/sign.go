// Package webhook delivers events to customer HTTP endpoints
// (docs/plan-v1.0.md §2, §5.2).
//
// Events reach JetStream through the outbox and the relay already; this is
// the last hop, and the only one that leaves the system's own network. That
// is what shapes the package: everything here assumes the far end is slow,
// unreachable, or lying about having received something.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Headers every delivery carries (docs/plan-v1.0.md §7.6).
const (
	SignatureHeader = "X-Exchange-Signature" // v1=<hex>
	TimestampHeader = "X-Exchange-Timestamp" // unix milliseconds
	EventIDHeader   = "X-Exchange-Event-Id"
	EventTypeHeader = "X-Exchange-Event-Type"
)

// ErrBadSignature is every way a signature can fail to check out. The reason
// is deliberately not distinguished: telling a caller whether the secret was
// wrong or the body was tampered with helps an attacker more than a
// developer, and the developer has the request in front of them.
var ErrBadSignature = errors.New("webhook: signature does not verify")

// Sign returns the SignatureHeader value and the TimestampHeader value:
//
//	v1=<hex HMAC-SHA256(secret, "<timestamp>.<body>")>, <unix milliseconds>
//
// The timestamp travels in its own header rather than inside the signature
// value, because docs/plan-v1.0.md §7.6 wrote that shape down before this
// code existed and it is a contract with customers. It is still covered by
// the HMAC -- as a header field alone it could be swapped for a fresh one and
// an old body replayed under a signature that still checked out.
//
// This is deliberately NOT auth.SignRequest, which signs
// "<ts>\n<METHOD>\n<requestURI>\n<body>" for inbound API-key requests. Two
// reasons, both of which would be discovered late:
//
//   - There is no request URI worth signing on the way out. Binding one in
//     would tie the secret to the endpoint's URL, so a customer moving from
//     /hook to /hooks/v2 would silently stop verifying.
//   - The inbound scheme's timestamp is checked against a window by our
//     middleware. Here the *customer* checks it, from a header, using
//     whatever library they have -- so the format has to be the one their
//     ecosystem already knows. This is Stripe's and GitHub's shape.
func Sign(secret string, body []byte, at time.Time) (signature, timestamp string) {
	return SignAll([]string{secret}, body, at)
}

// SignAll signs with every secret, newest first, as "v1=<a>,v1=<b>". Two
// secrets exist only during the grace period after a rotation; a receiver
// verifies whichever it holds and ignores the rest (Verify accepts any).
func SignAll(secrets []string, body []byte, at time.Time) (signature, timestamp string) {
	ts := strconv.FormatInt(at.UnixMilli(), 10)
	parts := make([]string, 0, len(secrets))
	for _, s := range secrets {
		parts = append(parts, "v1="+mac(s, ts, body))
	}
	return strings.Join(parts, ","), ts
}

// Verify checks a signature against the body and the clock. tolerance bounds
// how old -- and how far in the future -- a delivery may be; a signature with
// no age limit is one that can be replayed forever, and a clock ahead of ours
// is as suspect as one behind.
func Verify(secret, signature, timestamp string, body []byte, now time.Time, tolerance time.Duration) error {
	// Only v1 exists. Parsing rather than comparing the whole string leaves
	// room for a v2 alongside it, which is how a scheme gets replaced without
	// a flag day for every customer -- and for the second v1 a delivery
	// carries during the grace period after a rotation. Any one that matches
	// is enough: the receiver holds one secret and does not know which
	// position it is in.
	var v1s []string
	for _, part := range strings.Split(signature, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k == "v1" && v != "" {
			v1s = append(v1s, v)
		}
	}
	if len(v1s) == 0 || timestamp == "" {
		return fmt.Errorf("%w: need %s: v1=<hex> and %s", ErrBadSignature, SignatureHeader, TimestampHeader)
	}
	ms, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: %s must be unix milliseconds", ErrBadSignature, TimestampHeader)
	}
	if skew := now.Sub(time.UnixMilli(ms)); skew > tolerance || skew < -tolerance {
		return fmt.Errorf("%w: timestamp is %s away", ErrBadSignature, skew.Round(time.Second))
	}
	// Constant time, and on the hex text rather than the bytes: a decode step
	// would need its own error path for input an attacker controls. Every
	// candidate is checked rather than stopping at the first, so the time
	// taken says nothing about which position matched.
	want := []byte(mac(secret, timestamp, body))
	ok := false
	for _, v := range v1s {
		if hmac.Equal([]byte(v), want) {
			ok = true
		}
	}
	if !ok {
		return ErrBadSignature
	}
	return nil
}

func mac(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

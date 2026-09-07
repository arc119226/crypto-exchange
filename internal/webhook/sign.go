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

// SignatureHeader carries the timestamp and the HMAC.
const SignatureHeader = "X-Exchange-Signature"

// ErrBadSignature is every way a signature can fail to check out. The reason
// is deliberately not distinguished: telling a caller whether the secret was
// wrong or the body was tampered with helps an attacker more than a
// developer, and the developer has the request in front of them.
var ErrBadSignature = errors.New("webhook: signature does not verify")

// Sign returns the value for SignatureHeader:
//
//	t=<unix milliseconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>
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
//
// The timestamp is inside the signed payload, not merely alongside it: as a
// header field alone it could be swapped for a fresh one and an old body
// replayed under a signature that still checked out.
func Sign(secret string, body []byte, at time.Time) string {
	ts := strconv.FormatInt(at.UnixMilli(), 10)
	return "t=" + ts + ",v1=" + mac(secret, ts, body)
}

// Verify checks a signature against the body and the clock. tolerance bounds
// how old -- and how far in the future -- a delivery may be; a signature with
// no age limit is one that can be replayed forever, and a clock ahead of ours
// is as suspect as one behind.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	if ts == "" || v1 == "" {
		return fmt.Errorf("%w: header must be t=<unix ms>,v1=<hex>", ErrBadSignature)
	}
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: t must be unix milliseconds", ErrBadSignature)
	}
	if skew := now.Sub(time.UnixMilli(ms)); skew > tolerance || skew < -tolerance {
		return fmt.Errorf("%w: timestamp is %s away", ErrBadSignature, skew.Round(time.Second))
	}
	// Constant time, and on the hex text rather than the bytes: a decode step
	// would need its own error path for input an attacker controls.
	if !hmac.Equal([]byte(v1), []byte(mac(secret, ts, body))) {
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

package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verifyIndependently is deliberately a second implementation of the scheme,
// written from the wire format rather than from Sign. If the test called
// Sign to check Sign, a change to the canonical string would pass the test
// and break every customer -- which is the one failure this test exists to
// prevent.
func verifyIndependently(t *testing.T, signature, timestamp, secret string, body []byte) bool {
	t.Helper()
	var v1 string
	for _, part := range strings.Split(signature, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k == "v1" {
			v1 = v
		}
	}
	if v1 == "" || timestamp == "" {
		return false
	}
	ts := timestamp
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return hmac.Equal([]byte(v1), []byte(hex.EncodeToString(mac.Sum(nil))))
}

func TestSignatureIsVerifiableFromTheWireFormatAlone(t *testing.T) {
	body := []byte(`{"event_type":"trade.executed","event_id":"ev-1"}`)
	at := time.UnixMilli(1757280000000)

	sig, ts := Sign("shh", body, at)

	assert.True(t, strings.HasPrefix(sig, "v1="), sig)
	assert.Equal(t, "1757280000000", ts, "the timestamp travels in its own header (§7.6)")
	assert.True(t, verifyIndependently(t, sig, ts, "shh", body),
		"a customer with only docs/webhooks.md must be able to verify this")

	assert.False(t, verifyIndependently(t, sig, ts, "wrong", body), "a different secret must not verify")
	assert.False(t, verifyIndependently(t, sig, ts, "shh", append(body, ' ')), "a tampered body must not verify")
}

// The timestamp is inside the signed payload, not merely beside it. If it
// were only a header field an attacker could replay yesterday's body with
// today's timestamp and the signature would still check out.
func TestTimestampIsCoveredBySignature(t *testing.T) {
	body := []byte(`{"event_id":"ev-1"}`)
	early, earlyTS := Sign("shh", body, time.UnixMilli(1757280000000))
	later, laterTS := Sign("shh", body, time.UnixMilli(1757280001000))
	require.NotEqual(t, early, later, "the same body at two times must sign differently")

	// The timestamp is in its own header, so an attacker can freely replace
	// it. Present the later one with the earlier signature: it must not
	// verify, which is what proves the timestamp is inside the HMAC and not
	// merely beside it.
	assert.False(t, verifyIndependently(t, early, laterTS, "shh", body))
	assert.True(t, verifyIndependently(t, early, earlyTS, "shh", body), "its own timestamp still does")
}

// Verify is what exchangectl webhook-sink and customers' own code use. It has
// to accept what Sign produced and reject a stale one, because a signature
// with no age limit is a signature that can be replayed forever.
func TestVerifyAcceptsFreshRejectsStale(t *testing.T) {
	body := []byte(`{"event_id":"ev-1"}`)
	now := time.Now()

	verify := func(at time.Time, secret string) error {
		sig, ts := Sign(secret, body, at)
		return Verify("shh", sig, ts, body, now, 5*time.Minute)
	}
	require.NoError(t, verify(now, "shh"))
	assert.Error(t, verify(now.Add(-10*time.Minute), "shh"), "ten minutes old, five minute tolerance")
	assert.Error(t, verify(now, "nope"), "signed with a different secret")
	// A timestamp far in the future is as suspect as one far in the past.
	assert.Error(t, verify(now.Add(10*time.Minute), "shh"))

	sig, ts := Sign("shh", body, now)
	assert.Error(t, Verify("shh", "garbage", ts, body, now, 5*time.Minute))
	assert.Error(t, Verify("shh", sig, "notanumber", body, now, 5*time.Minute))
	assert.Error(t, Verify("shh", sig, "", body, now, 5*time.Minute), "a missing timestamp header")
}

func TestSignIsStableAcrossCalls(t *testing.T) {
	body := []byte("x")
	at := time.UnixMilli(1757280000000)
	a, aTS := Sign("s", body, at)
	b, bTS := Sign("s", body, at)
	assert.Equal(t, a, b, "no randomness in the scheme")
	assert.Equal(t, aTS, bTS)
	_, err := strconv.ParseInt(aTS, 10, 64)
	assert.NoError(t, err, "the timestamp header is unix milliseconds")
}

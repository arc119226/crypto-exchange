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
func verifyIndependently(t *testing.T, header, secret string, body []byte) bool {
	t.Helper()
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
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return hmac.Equal([]byte(v1), []byte(hex.EncodeToString(mac.Sum(nil))))
}

func TestSignatureIsVerifiableFromTheWireFormatAlone(t *testing.T) {
	body := []byte(`{"event_type":"trade.executed","event_id":"ev-1"}`)
	at := time.UnixMilli(1757280000000)

	header := Sign("shh", body, at)

	assert.True(t, strings.HasPrefix(header, "t=1757280000000,v1="), header)
	assert.True(t, verifyIndependently(t, header, "shh", body),
		"a customer with only docs/webhooks.md must be able to verify this")

	assert.False(t, verifyIndependently(t, header, "wrong", body), "a different secret must not verify")
	assert.False(t, verifyIndependently(t, header, "shh", append(body, ' ')), "a tampered body must not verify")
}

// The timestamp is inside the signed payload, not merely beside it. If it
// were only a header field an attacker could replay yesterday's body with
// today's timestamp and the signature would still check out.
func TestTimestampIsCoveredBySignature(t *testing.T) {
	body := []byte(`{"event_id":"ev-1"}`)
	early := Sign("shh", body, time.UnixMilli(1757280000000))
	later := Sign("shh", body, time.UnixMilli(1757280001000))
	require.NotEqual(t, early, later, "the same body at two times must sign differently")

	// Splice the later timestamp onto the earlier signature: it must not verify.
	spliced := "t=1757280001000," + early[strings.Index(early, "v1="):]
	assert.False(t, verifyIndependently(t, spliced, "shh", body))
}

// Verify is what exchangectl webhook-sink and customers' own code use. It has
// to accept what Sign produced and reject a stale one, because a signature
// with no age limit is a signature that can be replayed forever.
func TestVerifyAcceptsFreshRejectsStale(t *testing.T) {
	body := []byte(`{"event_id":"ev-1"}`)
	now := time.Now()

	require.NoError(t, Verify("shh", Sign("shh", body, now), body, now, 5*time.Minute))
	assert.Error(t, Verify("shh", Sign("shh", body, now.Add(-10*time.Minute)), body, now, 5*time.Minute),
		"ten minutes old, five minute tolerance")
	assert.Error(t, Verify("nope", Sign("shh", body, now), body, now, 5*time.Minute))
	assert.Error(t, Verify("shh", "garbage", body, now, 5*time.Minute))
	assert.Error(t, Verify("shh", "t=notanumber,v1=aa", body, now, 5*time.Minute))

	// A timestamp far in the future is as suspect as one far in the past.
	assert.Error(t, Verify("shh", Sign("shh", body, now.Add(10*time.Minute)), body, now, 5*time.Minute))
}

func TestSignIsStableAcrossCalls(t *testing.T) {
	body := []byte("x")
	at := time.UnixMilli(1757280000000)
	assert.Equal(t, Sign("s", body, at), Sign("s", body, at), "no randomness in the scheme")
	_, err := strconv.ParseInt(strings.TrimPrefix(strings.Split(Sign("s", body, at), ",")[0], "t="), 10, 64)
	assert.NoError(t, err, "t is unix milliseconds")
}

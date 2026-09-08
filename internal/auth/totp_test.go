package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The replay rule needs to know which step a code matched, which the
// library's yes/no Validate cannot say. totpMatch is the piece that makes
// that number available; these are its edges.
func TestTOTPMatchReportsTheStepTheCodeBelongsTo(t *testing.T) {
	key, err := newTOTPKey("exchange", "admin@example.com")
	require.NoError(t, err)
	secret := key.Secret()
	// Well inside a step, so ±30s stays within the neighbouring steps.
	now := time.Unix(1_757_000_015, 0).UTC()

	for _, tc := range []struct {
		name   string
		codeAt time.Duration
		want   bool
	}{
		{"current step", 0, true},
		{"one step behind (client clock slow)", -totpPeriod, true},
		{"one step ahead (client clock fast)", totpPeriod, true},
		{"two steps behind", -2 * totpPeriod, false},
		{"two steps ahead", 2 * totpPeriod, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := TOTPCode(secret, now.Add(tc.codeAt))
			require.NoError(t, err)
			step, ok := totpMatch(secret, code, now)
			assert.Equal(t, tc.want, ok)
			if tc.want {
				assert.Equal(t, totpStep(now.Add(tc.codeAt)), step,
					"the step recorded as spent must be the one the code was generated for, not the clock's")
			}
		})
	}
}

func TestTOTPMatchRejectsWhatIsNotACode(t *testing.T) {
	key, err := newTOTPKey("exchange", "admin@example.com")
	require.NoError(t, err)
	now := time.Now()
	for _, code := range []string{"", "12345", "1234567", "abcdef", "000000"} {
		_, ok := totpMatch(key.Secret(), code, now)
		// "000000" is a real code one time in a million; the others never.
		if code != "000000" {
			assert.False(t, ok, "%q must not verify", code)
		}
	}
	other, err := newTOTPKey("exchange", "someone@example.com")
	require.NoError(t, err)
	code, err := TOTPCode(other.Secret(), now)
	require.NoError(t, err)
	_, ok := totpMatch(key.Secret(), code, now)
	assert.False(t, ok, "a code for a different secret must not verify")
}

// The replay rule itself, as the service applies it: a code is spent at the
// step it matched, so the same code presented again -- or a code for any
// earlier step -- is refused even though the arithmetic still checks out.
func TestTOTPReplayRuleSpendsTheMatchedStep(t *testing.T) {
	key, err := newTOTPKey("exchange", "admin@example.com")
	require.NoError(t, err)
	now := time.Unix(1_757_000_015, 0).UTC()

	// A fast client produces the code for the step ahead; it verifies now.
	ahead, err := TOTPCode(key.Secret(), now.Add(totpPeriod))
	require.NoError(t, err)
	step, ok := totpMatch(key.Secret(), ahead, now)
	require.True(t, ok)
	last := step

	// Thirty seconds later the server's clock reaches that step. Recording
	// the *clock's* step at acceptance time would have left this code live;
	// recording the matched step makes it spent.
	later := now.Add(totpPeriod)
	step2, ok := totpMatch(key.Secret(), ahead, later)
	require.True(t, ok, "arithmetically the code is still valid")
	assert.False(t, step2 > last, "but it must be refused as a replay: step %d was already spent", step2)

	// The next step's code is fine.
	next, err := TOTPCode(key.Secret(), later.Add(totpPeriod))
	require.NoError(t, err)
	step3, ok := totpMatch(key.Secret(), next, later)
	require.True(t, ok)
	assert.True(t, step3 > last)
}

func TestTOTPEnrolmentArtifacts(t *testing.T) {
	key, err := newTOTPKey("exchange", "admin@example.com")
	require.NoError(t, err)
	assert.Len(t, key.Secret(), 32, "160 bits of secret is 32 base32 characters")
	assert.Contains(t, key.URL(), "otpauth://totp/exchange:admin@example.com")
	assert.Contains(t, key.URL(), "period=30")
	png, err := qrPNG(key)
	require.NoError(t, err)
	assert.Equal(t, []byte("\x89PNG"), png[:4])
}

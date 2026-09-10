package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --to names a day the operator wants included, so it has to widen to the
// midnight after it. Getting this backwards drops the last day of every
// report, silently and plausibly.
func TestDayOrInstantIncludesTheDayNamedAsTheEnd(t *testing.T) {
	to, err := dayOrInstant("to", "2026-09-08", true)
	require.NoError(t, err)
	require.NotNil(t, to)
	assert.Equal(t, "2026-09-09T00:00:00Z", to.UTC().Format(time.RFC3339))

	from, err := dayOrInstant("from", "2026-09-01", false)
	require.NoError(t, err)
	require.NotNil(t, from)
	assert.Equal(t, "2026-09-01T00:00:00Z", from.UTC().Format(time.RFC3339))
}

// An RFC 3339 instant is passed through exactly, end or not: a caller who
// gives an instant means that instant.
func TestDayOrInstantPassesAnInstantThrough(t *testing.T) {
	to, err := dayOrInstant("to", "2026-09-08T12:30:00Z", true)
	require.NoError(t, err)
	require.NotNil(t, to)
	assert.Equal(t, "2026-09-08T12:30:00Z", to.UTC().Format(time.RFC3339))
}

func TestDayOrInstantAbsentAndInvalid(t *testing.T) {
	v, err := dayOrInstant("from", "", false)
	require.NoError(t, err)
	assert.Nil(t, v, "absent means the server picks the default")

	_, err = dayOrInstant("from", "last tuesday", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--from")
}

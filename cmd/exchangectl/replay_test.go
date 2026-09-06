package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixture = "../../test/fixtures/matching/partial_fill_price_improvement.jsonl"

func TestReplayTable(t *testing.T) {
	out, err := run(t, "replay", "--file", fixture)
	require.NoError(t, err)
	assert.Contains(t, out, "accepted")
	assert.Contains(t, out, "trade")
	assert.Contains(t, out, "0.4 x 1990 = 796")
	assert.Contains(t, out, "cancelled")
	assert.Contains(t, out, "reason=user")
	assert.Contains(t, out, "(empty book)")
}

func TestReplayJSONAndEventsFile(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	out, err := run(t, "--output", "json", "replay", "--file", fixture, "--events", events, "--snapshot")
	require.NoError(t, err)
	assert.Contains(t, out, `"kind":"trade"`)
	assert.Contains(t, out, `"price":"1990"`)
	assert.Contains(t, out, `"last_seq": 3`)
	written, err := os.ReadFile(events)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(written)), "\n")
	assert.Len(t, lines, 6, "accepted, accepted, trade, filled, updated, cancelled")
	assert.True(t, strings.HasPrefix(lines[2], `{"kind":"trade"`), lines[2])
}

func TestReplayDepthAndErrors(t *testing.T) {
	out, err := run(t, "replay", "--file", "../../test/fixtures/matching/limit_rest_and_cancel.jsonl", "--depth", "1")
	require.NoError(t, err)
	_, depthSection, found := strings.Cut(out, "depth after seq")
	require.True(t, found, out)
	assert.Contains(t, depthSection, "SIDE")
	assert.Contains(t, depthSection, "2001")
	assert.Contains(t, depthSection, "1999")
	assert.NotContains(t, depthSection, "1998.5", "--depth 1 shows one level per side")

	_, err = run(t, "replay", "--file", filepath.Join(t.TempDir(), "missing.jsonl"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open script")

	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	require.NoError(t, os.WriteFile(bad, []byte("{\"seq\":1,\"cancel\":{\"order_id\":\"x\"}}\n"), 0o644))
	_, err = run(t, "replay", "--file", bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing market header")

	_, err = run(t, "replay")
	require.Error(t, err, "--file is required")
}

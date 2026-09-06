package matching_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/matching"
)

var update = flag.Bool("update", false, "rewrite the matching golden files from the current implementation")

const fixtureDir = "../../test/fixtures/matching"

// TestGolden replays every test/fixtures/matching/*.jsonl script and compares
// the events (one JSON line per event) and the final snapshot with the
// committed golden files. `go test ./internal/matching -run TestGolden -update`
// regenerates them; review the diff like any other code change.
func TestGolden(t *testing.T) {
	scripts, err := filepath.Glob(filepath.Join(fixtureDir, "*.jsonl"))
	require.NoError(t, err)
	var found int
	for _, path := range scripts {
		if strings.HasSuffix(path, ".events.jsonl") {
			continue
		}
		found++
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			script, err := matching.ReadScript(bytes.NewReader(raw))
			require.NoError(t, err)
			perCommand, book, err := matching.Run(script)
			require.NoError(t, err)

			var events bytes.Buffer
			for _, evs := range perCommand {
				for _, e := range evs {
					b, err := matching.EncodeEvent(e)
					require.NoError(t, err)
					events.Write(b)
					events.WriteByte('\n')
				}
			}
			snapshot, err := json.MarshalIndent(book.Snapshot(), "", "  ")
			require.NoError(t, err)
			snapshot = append(snapshot, '\n')

			eventsPath := filepath.Join(fixtureDir, name+".events.jsonl")
			snapshotPath := filepath.Join(fixtureDir, name+".snapshot.json")
			if *update {
				require.NoError(t, os.WriteFile(eventsPath, events.Bytes(), 0o644))
				require.NoError(t, os.WriteFile(snapshotPath, snapshot, 0o644))
				return
			}
			wantEvents, err := os.ReadFile(eventsPath)
			require.NoError(t, err, "missing golden; run with -update")
			assert.Equal(t, string(wantEvents), events.String(), "events differ from golden %s", eventsPath)
			wantSnap, err := os.ReadFile(snapshotPath)
			require.NoError(t, err, "missing golden; run with -update")
			assert.Equal(t, string(wantSnap), string(snapshot), "snapshot differs from golden %s", snapshotPath)

			// every golden event must decode back (format stability)
			for _, line := range bytes.Split(bytes.TrimSpace(wantEvents), []byte{'\n'}) {
				_, err := matching.DecodeEvent(line)
				require.NoError(t, err, string(line))
			}
		})
	}
	require.GreaterOrEqual(t, found, 5, "Phase 1 DoD: at least five golden fixtures")
}

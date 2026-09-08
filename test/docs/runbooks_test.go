// Package docs tests the operational documents the way the code is tested:
// every runbook under docs/runbooks/ has the four sections docs/plan-v1.0.md
// §12 Phase 7 asks for, in order and non-empty; every path a runbook or the
// beta checklist names exists; every exchange / exchangectl subcommand a
// runbook tells an operator to type is one the binaries have; every metric
// a runbook cites is one the Go code registers.
//
// CI's paths-ignore lets a documentation-only pull request skip this, so a
// broken runbook shows up on the merge to main rather than on the pull
// request. That is accepted: the alternative is a full CI run for a typo.
package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var repoRoot = filepath.Join("..", "..")

// The four sections, in this order, before anything else.
var requiredSections = []string{"症狀", "檢查指令", "處置", "驗證"}

var (
	h1Re       = regexp.MustCompile(`(?m)^# (.+)$`)
	h2Re       = regexp.MustCompile(`(?m)^## (.+)$`)
	linkRe     = regexp.MustCompile(`\]\(([^)\s#]+)(?:#[^)]*)?\)`)
	backtickRe = regexp.MustCompile("`([^`\n]+)`")
	// json is left out: the Sepolia fixtures a runbook names are generated
	// files that live outside the repository
	repoPathRe = regexp.MustCompile(`^(docs|scripts|deploy|infra|migrations|cmd|internal|build|test|web|api)/[A-Za-z0-9_./-]+\.(md|sh|sql|go|yaml|yml|conf|toml)$`)
	fenceRe    = regexp.MustCompile("(?s)```.*?```")
	// exchange <sub> [<sub>]: the word before must not be part of another
	// name (crypto-exchange, exchange-api), the words after are lowercase
	// subcommands; a flag or an argument ends the match.
	cliRe      = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])(exchangectl|exchange)( [a-z][a-z-]*)( [a-z][a-z-]*)?`)
	useRe      = regexp.MustCompile(`Use: *"([a-z][a-z-]*)`)
	metricRe   = regexp.MustCompile(`^([a-z][a-z0-9]*(?:_[a-z0-9]+)+)(\{.*\})?$`)
	literalRe  = regexp.MustCompile(`"([a-z][a-z0-9_]*_[a-z0-9_]*)"`)
	metricPref = []string{"trading_", "engine_", "chain_", "withdrawals_", "sweeps_", "deposits_", "outbox_", "ledger_", "reconciliation_",
		"hot_wallet_", "webhook_", "ws_", "stream_", "backup_", "marketdata_", "event_consumer", "http_", "exchange_", "retention_", "admin_login", "db_pool"}
	metricSuffix = []string{"_total", "_seconds", "_bytes", "_blocks", "_diff", "_lag", "_depth", "_seq", "_ready", "_balance", "_review"}
	builtinPref  = []string{"node_", "process_", "go_", "up"}
	// English that follows the word "exchange" in prose or in a quoted error
	// message ("this exchange did not send"), as opposed to a subcommand.
	prose = map[string]bool{"did": true, "does": true, "is": true, "was": true, "has": true, "and": true, "or": true, "the": true,
		"a": true, "an": true, "to": true, "in": true, "on": true, "of": true, "for": true, "with": true, "that": true, "this": true,
		"as": true, "at": true, "role": true, "roles": true, "service": true, "stack": true, "beta": true, "binary": true}
)

func runbooks(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot, "docs", "runbooks", "*.md"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	sort.Strings(files)
	return files
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // repository files
	require.NoError(t, err)
	return string(b)
}

// TestRunbooksHaveTheFourSections: a runbook is read at 3am by someone who
// did not write it, so every one has the same shape.
func TestRunbooksHaveTheFourSections(t *testing.T) {
	for _, f := range runbooks(t) {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			// fenced code blocks hold shell comments that look like headings;
			// masking their "#" keeps every offset and the section bodies intact
			body := fenceRe.ReplaceAllStringFunc(read(t, f), func(block string) string {
				return strings.ReplaceAll(block, "#", "_")
			})
			require.Regexp(t, `\A# \S`, body, "starts with a title")
			assert.Len(t, h1Re.FindAllString(body, -1), 1, "one title")
			heads := h2Re.FindAllStringSubmatchIndex(body, -1)
			require.GreaterOrEqual(t, len(heads), len(requiredSections), "needs %v", requiredSections)
			for i, want := range requiredSections {
				got := strings.TrimSpace(body[heads[i][2]:heads[i][3]])
				assert.Equal(t, want, got, "section %d", i+1)
				end := len(body)
				if i+1 < len(heads) {
					end = heads[i+1][0]
				}
				section := strings.TrimSpace(body[heads[i][1]:end])
				assert.NotEmpty(t, section, "section %q is empty", want)
			}
		})
	}
}

// TestRunbooksPointAtFilesThatExist: markdown links and backticked
// repository paths in the runbooks and the beta checklist.
func TestRunbooksPointAtFilesThatExist(t *testing.T) {
	files := append(runbooks(t), filepath.Join(repoRoot, "docs", "beta-checklist.md"))
	for _, f := range files {
		body := read(t, f)
		for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			p := filepath.Join(filepath.Dir(f), target)
			_, err := os.Stat(p)
			assert.NoError(t, err, "%s links to %s", filepath.Base(f), target)
		}
		for _, m := range backtickRe.FindAllStringSubmatch(body, -1) {
			ref := m[1]
			if !repoPathRe.MatchString(ref) {
				continue
			}
			_, err := os.Stat(filepath.Join(repoRoot, ref))
			assert.NoError(t, err, "%s names %s", filepath.Base(f), ref)
		}
	}
}

// TestRunbooksUseCommandsTheBinariesHave: `exchange keys rewrap` in a
// runbook must be a subcommand cmd/exchange declares, so a renamed command
// cannot leave an operator typing something that no longer exists.
func TestRunbooksUseCommandsTheBinariesHave(t *testing.T) {
	known := map[string]bool{}
	for _, dir := range []string{"cmd/exchange", "cmd/exchangectl"} {
		files, err := filepath.Glob(filepath.Join(repoRoot, dir, "*.go"))
		require.NoError(t, err)
		for _, f := range files {
			for _, m := range useRe.FindAllStringSubmatch(read(t, f), -1) {
				known[m[1]] = true
			}
		}
	}
	require.True(t, known["serve"] && known["migrate"] && known["orders"], "sanity: the binaries' commands were found")
	for _, f := range runbooks(t) {
		for _, m := range cliRe.FindAllStringSubmatch(read(t, f), -1) {
			if prose[strings.TrimSpace(m[2])] {
				continue
			}
			for _, sub := range []string{m[2], m[3]} {
				sub = strings.TrimSpace(sub)
				if sub == "" {
					continue
				}
				assert.True(t, known[sub], "%s: %q is not a subcommand of %s", filepath.Base(f), strings.TrimSpace(m[0]), m[1])
			}
		}
	}
}

// TestRunbooksCiteRegisteredMetrics: a metric a runbook tells the reader to
// grep for must be one some Go file under internal/ registers, by the same
// rule infra/observability/observability_test.go applies to dashboards.
func TestRunbooksCiteRegisteredMetrics(t *testing.T) {
	registered := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, m := range literalRe.FindAllStringSubmatch(read(t, path), -1) {
			registered[m[1]] = true
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, registered["exchange_ready"], "sanity")
	looksLikeMetric := func(name string) bool {
		var prefixed bool
		for _, p := range metricPref {
			if strings.HasPrefix(name, p) {
				prefixed = true
			}
		}
		if !prefixed {
			return false
		}
		if strings.Count(name, "_") >= 2 {
			return true
		}
		for _, s := range metricSuffix {
			if strings.HasSuffix(name, s) {
				return true
			}
		}
		return false
	}
	for _, f := range runbooks(t) {
		for _, m := range backtickRe.FindAllStringSubmatch(read(t, f), -1) {
			mm := metricRe.FindStringSubmatch(m[1])
			if mm == nil || !looksLikeMetric(mm[1]) || registered[mm[1]] {
				continue
			}
			builtin := false
			for _, p := range builtinPref {
				if strings.HasPrefix(mm[1], p) {
					builtin = true
				}
			}
			assert.True(t, builtin, "%s cites %q, which no Go file under internal/ registers", filepath.Base(f), mm[1])
		}
	}
}

// TestBetaChecklistCoversEveryRunbook: the checklist is the index an
// operator works from, so a runbook nobody lists is a runbook nobody reads.
func TestBetaChecklistCoversEveryRunbook(t *testing.T) {
	checklist := read(t, filepath.Join(repoRoot, "docs", "beta-checklist.md"))
	for _, f := range runbooks(t) {
		assert.Contains(t, checklist, "docs/runbooks/"+filepath.Base(f), "the checklist does not mention %s", filepath.Base(f))
	}
	for _, must := range []string{"RPO", "RTO", "escrow", "Sepolia"} {
		assert.Contains(t, checklist, must)
	}
}

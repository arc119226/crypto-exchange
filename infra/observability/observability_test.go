// Package observability holds no Go code; this test is the stand-in for
// promtool and Grafana's own validation, which the CI image does not carry.
// It runs under `make test` like every other package and checks the files
// compose mounts into Prometheus and Grafana:
//
//   - every dashboard parses, carries a unique uid, targets the provisioned
//     datasource (uid "prometheus"), has unique panel ids and no empty
//     queries;
//   - alerts.yml parses as a Prometheus rule file and every rule has an
//     expression, a severity and a summary;
//   - every metric name a dashboard or a rule reads is one the Go code
//     registers (it appears as a string literal under internal/), so a
//     renamed metric breaks the build here instead of leaving an empty
//     panel or an alert that can never fire;
//   - prometheus.yml loads alerts.yml through rule_files and compose mounts
//     the same file at that path.
package observability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

const datasourceUID = "prometheus"

// Metrics the Go runtime, the Prometheus client and Prometheus itself
// expose without any code in this repository naming them.
// node_ is prom/node-exporter, which the production overlay runs for the
// disk alert.
var builtinPrefixes = []string{"up", "process_", "go_", "scrape_", "promhttp_", "node_"}

// PromQL words that look like metric names once selectors and ranges are
// stripped. Functions are recognised by the "(" that follows them, so
// only aggregation modifiers and operators need listing.
var promqlWords = map[string]bool{
	"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
	"and": true, "or": true, "unless": true, "bool": true, "offset": true, "inf": true, "nan": true,
}

var (
	selectorRe  = regexp.MustCompile(`\{[^}]*\}`)
	rangeRe     = regexp.MustCompile(`\[[^\]]*\]`)
	groupingRe  = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	tokenRe     = regexp.MustCompile(`\b[a-z][a-z0-9_]*\b`)
	callRe      = regexp.MustCompile(`\b[a-z][a-z0-9_]*\s*\(`)
	metricNamRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// metricNames extracts the metric names an expression reads, with the
// histogram suffixes folded back onto the base name.
func metricNames(expr string) []string {
	s := selectorRe.ReplaceAllString(expr, "")
	s = rangeRe.ReplaceAllString(s, "")
	s = groupingRe.ReplaceAllString(s, "")
	calls := map[string]bool{}
	for _, c := range callRe.FindAllString(s, -1) {
		calls[strings.TrimSpace(strings.TrimSuffix(c, "("))] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, tok := range tokenRe.FindAllString(s, -1) {
		if promqlWords[tok] || calls[tok] {
			continue
		}
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			tok = strings.TrimSuffix(tok, suffix)
		}
		if !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	sort.Strings(out)
	return out
}

func isBuiltin(name string) bool {
	for _, p := range builtinPrefixes {
		if name == p || (strings.HasSuffix(p, "_") && strings.HasPrefix(name, p)) {
			return true
		}
	}
	return false
}

// registeredMetrics returns every metric name that appears as a string
// literal in non-test Go files under internal/. Name: "x" in a *Opts
// literal and NewDesc("x", ...) both match; a name that only exists as a
// concatenation would not, which is a reason not to build names that way.
func registeredMetrics(t *testing.T) map[string]bool {
	t.Helper()
	root := filepath.Join("..", "..", "internal")
	literal := regexp.MustCompile(`"([a-z][a-z0-9_]*_[a-z0-9_]*)"`)
	names := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range literal.FindAllStringSubmatch(string(b), -1) {
			names[m[1]] = true
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, names["http_request_duration_seconds"], "sanity: telemetry's own metric must be found")
	return names
}

func requireKnownMetrics(t *testing.T, known map[string]bool, where, expr string) {
	t.Helper()
	names := metricNames(expr)
	require.NotEmpty(t, names, "%s: no metric in %q", where, expr)
	for _, n := range names {
		assert.True(t, metricNamRe.MatchString(n), "%s: odd token %q in %q", where, n, expr)
		if isBuiltin(n) {
			continue
		}
		assert.True(t, known[n], "%s reads %q, which no Go file under internal/ registers (expr %q)", where, n, expr)
	}
}

type panel struct {
	ID         int             `json:"id"`
	Type       string          `json:"type"`
	Title      string          `json:"title"`
	Datasource json.RawMessage `json:"datasource"`
	Targets    []target        `json:"targets"`
	Panels     []panel         `json:"panels"`
}

type target struct {
	RefID      string          `json:"refId"`
	Expr       string          `json:"expr"`
	Datasource json.RawMessage `json:"datasource"`
}

type dashboard struct {
	UID           string  `json:"uid"`
	Title         string  `json:"title"`
	SchemaVersion int     `json:"schemaVersion"`
	Panels        []panel `json:"panels"`
	Templating    struct {
		List []struct {
			Name       string          `json:"name"`
			Datasource json.RawMessage `json:"datasource"`
		} `json:"list"`
	} `json:"templating"`
}

func requireDatasource(t *testing.T, where string, raw json.RawMessage) {
	t.Helper()
	var ds struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	}
	require.NoError(t, json.Unmarshal(raw, &ds), "%s: datasource", where)
	assert.Equal(t, "prometheus", ds.Type, "%s: datasource type", where)
	assert.Equal(t, datasourceUID, ds.UID, "%s: datasource uid must be the provisioned one", where)
}

func walkPanels(t *testing.T, known map[string]bool, board string, panels []panel, ids map[int]string) {
	t.Helper()
	for _, p := range panels {
		where := board + " / " + p.Title
		if prev, dup := ids[p.ID]; dup {
			t.Errorf("%s: panel id %d already used by %q", where, p.ID, prev)
		}
		ids[p.ID] = p.Title
		require.NotEmpty(t, p.Title, "%s: panel %d has no title", board, p.ID)
		switch p.Type {
		case "row":
			walkPanels(t, known, board, p.Panels, ids)
			continue
		case "text":
			continue
		}
		requireDatasource(t, where, p.Datasource)
		require.NotEmpty(t, p.Targets, "%s: no targets", where)
		refs := map[string]bool{}
		for _, tg := range p.Targets {
			require.NotEmpty(t, tg.Expr, "%s: target %s has an empty expr", where, tg.RefID)
			require.False(t, refs[tg.RefID], "%s: duplicate refId %s", where, tg.RefID)
			refs[tg.RefID] = true
			requireDatasource(t, where+" target "+tg.RefID, tg.Datasource)
			requireKnownMetrics(t, known, where, tg.Expr)
		}
	}
}

func TestDashboards(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("dashboards", "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 5, "docs/plan-v1.0.md §15 names five dashboards")
	known := registeredMetrics(t)
	uids := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		var d dashboard
		require.NoError(t, json.Unmarshal(b, &d), "%s: not a dashboard", f)
		require.NotEmpty(t, d.UID, "%s: uid", f)
		require.NotEmpty(t, d.Title, "%s: title", f)
		assert.Equal(t, 39, d.SchemaVersion, "%s: Grafana 11.2 dashboards use schemaVersion 39", f)
		if prev, dup := uids[d.UID]; dup {
			t.Errorf("%s: uid %q already used by %s", f, d.UID, prev)
		}
		uids[d.UID] = f
		vars := map[string]bool{}
		for _, v := range d.Templating.List {
			vars[v.Name] = true
			requireDatasource(t, f+" variable "+v.Name, v.Datasource)
		}
		assert.True(t, vars["deployment"] && vars["instance"], "%s: every board carries the deployment and instance variables", f)
		require.NotEmpty(t, d.Panels, "%s: no panels", f)
		walkPanels(t, known, filepath.Base(f), d.Panels, map[int]string{})
	}
}

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			For         string            `yaml:"for"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

func TestAlertRules(t *testing.T) {
	b, err := os.ReadFile("alerts.yml")
	require.NoError(t, err)
	var rf ruleFile
	require.NoError(t, yaml.Unmarshal(b, &rf))
	require.NotEmpty(t, rf.Groups)
	known := registeredMetrics(t)
	seen := map[string]bool{}
	n := 0
	for _, g := range rf.Groups {
		require.NotEmpty(t, g.Name)
		for _, r := range g.Rules {
			n++
			where := g.Name + "/" + r.Alert
			require.NotEmpty(t, r.Alert, "%s: alert name", g.Name)
			require.False(t, seen[r.Alert], "%s: duplicate alert", where)
			seen[r.Alert] = true
			require.NotEmpty(t, r.Expr, "%s: expr", where)
			if r.For != "" {
				_, err := time.ParseDuration(r.For)
				require.NoError(t, err, "%s: for", where)
			}
			assert.Contains(t, []string{"critical", "warning"}, r.Labels["severity"], "%s: severity label", where)
			assert.NotEmpty(t, r.Annotations["summary"], "%s: summary annotation", where)
			requireKnownMetrics(t, known, where, r.Expr)
		}
	}
	// the seven of docs/plan-v1.0.md §15, plus what Phase 7 added for the
	// backups (docs/runbooks/backup-restore.md)
	want := []string{"ExchangeNotReady", "LedgerTrialBalanceBroken", "ChainScannerLagging", "OutboxBacklog", "HotWalletLow", "WithdrawalsPendingReview", "ReconciliationBreak",
		"BackupStale", "WalArchiveStale", "BackupNeverTaken", "DiskAlmostFull",
		// Phase 8 (docs/plan-v1.0.md §23.3): the withdrawal fee is priced to
		// cover gas, and this is what says it stopped doing so.
		"WithdrawalGasExceedsFee",
		// §6.4.1's reversed path: a credited deposit the chain took back.
		"DepositAwaitingReversal"}
	assert.Equal(t, len(want), n, "every alert is listed here so a new one is a deliberate addition")
	for _, w := range want {
		assert.True(t, seen[w], "missing alert %s", w)
	}
}

func TestPrometheusLoadsTheRuleFileComposeMounts(t *testing.T) {
	b, err := os.ReadFile("prometheus.yml")
	require.NoError(t, err)
	var cfg struct {
		RuleFiles []string `yaml:"rule_files"`
	}
	require.NoError(t, yaml.Unmarshal(b, &cfg))
	require.Len(t, cfg.RuleFiles, 1)
	inContainer := cfg.RuleFiles[0]

	compose, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", "compose.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(compose), "infra/observability/alerts.yml:"+inContainer+":ro",
		"compose must mount alerts.yml where prometheus.yml's rule_files expects it")

	ds, err := os.ReadFile(filepath.Join("grafana", "provisioning", "datasources", "prometheus.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(ds), "uid: "+datasourceUID, "dashboards pin the datasource by this uid")
}

func TestMetricNameExtraction(t *testing.T) {
	cases := map[string][]string{
		`histogram_quantile(0.99, sum by (le, route) (rate(http_request_duration_seconds_bucket{instance=~"$instance"}[$__rate_interval])))`: {"http_request_duration_seconds"},
		`exchange_ready == 0 or up{job="exchange"} == 0`:                                     {"exchange_ready", "up"},
		`max by (market) (engine_seq{a="b"}) - max by (market) (marketdata_book_seq{a="b"})`: {"engine_seq", "marketdata_book_seq"},
		`db_pool_connections{state="acquired"} / on (instance) db_pool_max`:                  {"db_pool_connections", "db_pool_max"},
		`hot_wallet_balance{asset="ETH"} < 1`:                                                {"hot_wallet_balance"},
	}
	for expr, want := range cases {
		assert.Equal(t, want, metricNames(expr), expr)
	}
}

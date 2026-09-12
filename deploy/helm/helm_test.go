// Package helm tests the exchange chart the way a CI job that cannot start
// a cluster still can: `helm lint`, `helm template` under the default and
// the kind values, `kubeconform` when present, and a set of invariants over
// the rendered manifests that a reviewer would otherwise have to check by
// hand (docs/plan-v1.0.md §12 Phase 7). It runs under `make test` with the
// rest of ./...; without a helm binary it skips, and in CI (CI=true) a
// missing helm fails instead, the same rule the integration tests apply to
// Docker.
package helm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

const chartDir = "exchange"

// secretNames are variables that must only ever reach a pod as a mounted
// file (their *_FILE variant); none may appear as a plain env value or in
// the ConfigMap.
var secretNames = []string{
	"DATABASE_URL", "NATS_URL", "REDIS_PASSWORD", "JWT_PRIVATE_KEY", "API_KEY_MASTER_KEY",
	"WEBHOOK_SIGNING_KEY", "ADMIN_TOTP_KEY", "ADMIN_API_KEY", "ADMIN_BOOTSTRAP_PASSWORD",
	"WALLET_KEYSTORE_PASSPHRASE", "POSTGRES_PASSWORD",
}

func helmBinary(t *testing.T) string {
	t.Helper()
	if h := os.Getenv("HELM"); h != "" {
		return h
	}
	h, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is not installed and CI is set: the chart cannot be checked")
		}
		t.Skip("helm not found; set HELM or add it to PATH")
	}
	return h
}

// render templates the chart with the given extra arguments and returns the
// parsed documents.
func render(t *testing.T, helm string, args ...string) []map[string]any {
	t.Helper()
	full := append([]string{"template", "exchange", chartDir}, args...)
	cmd := exec.Command(helm, full...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	require.NoError(t, cmd.Run(), "helm %s: %s", strings.Join(full, " "), stderr.String())
	return parseDocs(t, out.String())
}

func parseDocs(t *testing.T, rendered string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err != nil {
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			require.NoError(t, err)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}

// path walks nested maps and lists: path(doc, "spec", "template", "spec", "containers", 0, "env").
func path(v any, keys ...any) any {
	for _, k := range keys {
		switch node := v.(type) {
		case map[string]any:
			v = node[k.(string)]
		case []any:
			i := k.(int)
			if i >= len(node) {
				return nil
			}
			v = node[i]
		default:
			return nil
		}
	}
	return v
}

func name(doc map[string]any) string { return path(doc, "metadata", "name").(string) }
func kind(doc map[string]any) string { return doc["kind"].(string) }

func byKind(docs []map[string]any, k string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, d := range docs {
		if kind(d) == k {
			out[name(d)] = d
		}
	}
	return out
}

func TestChartLints(t *testing.T) {
	helm := helmBinary(t)
	for _, args := range [][]string{{"lint", chartDir}, {"lint", chartDir, "-f", filepath.Join(chartDir, "values-kind.yaml")}} {
		cmd := exec.Command(helm, args...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "helm %s:\n%s", strings.Join(args, " "), out)
	}
}

func TestChartInvariants(t *testing.T) {
	helm := helmBinary(t)
	docs := render(t, helm)
	deployments := byKind(docs, "Deployment")
	for _, role := range []string{"api", "engine", "chain", "signer", "stream", "admin", "worker"} {
		d, ok := deployments["exchange-"+role]
		require.True(t, ok, "a Deployment for the %s role", role)
		probes := path(d, "spec", "template", "spec", "containers", 0).(map[string]any)
		assert.NotNil(t, probes["readinessProbe"], "%s: readiness probe", role)
		assert.NotNil(t, probes["livenessProbe"], "%s: liveness probe", role)
		if role == "api" || role == "admin" {
			assert.Nil(t, probes["startupProbe"], "%s answers at once", role)
		} else {
			assert.NotNil(t, probes["startupProbe"], "%s waits on other services at start", role)
		}
		assert.EqualValues(t, 37, path(d, "spec", "template", "spec", "terminationGracePeriodSeconds"), "%s: drain 2 + engine 10 + timeout 20 + 5", role)
		strategy := path(d, "spec", "strategy", "type")
		switch role {
		case "engine", "chain", "signer", "admin":
			assert.EqualValues(t, 1, path(d, "spec", "replicas"), "%s is a singleton", role)
			assert.Equal(t, "Recreate", strategy, "%s never runs twice at once", role)
		default:
			assert.Equal(t, "RollingUpdate", strategy, role)
		}
		mounts := map[string]bool{}
		for _, m := range path(d, "spec", "template", "spec", "containers", 0, "volumeMounts").([]any) {
			mounts[path(m, "name").(string)] = true
		}
		assert.Equal(t, role == "signer", mounts["keystore"], "%s: only the signer mounts the keystore", role)
		assert.Equal(t, role == "api", mounts["jwt"], "%s: only the api mounts the JWT signing key", role)
		for _, e := range path(d, "spec", "template", "spec", "containers", 0, "env").([]any) {
			n := path(e, "name").(string)
			for _, secret := range secretNames {
				assert.NotEqual(t, secret, n, "%s: %s must arrive as a file, not a value", role, secret)
			}
			if strings.HasSuffix(n, "_FILE") {
				assert.True(t, strings.HasPrefix(path(e, "value").(string), "/var/run/exchange/"), "%s: %s points into the secrets directory", role, n)
			}
		}
	}
	assert.Len(t, deployments, 7, "no dev dependencies without dev.enabled")
	assert.Empty(t, byKind(docs, "Secret"), "the chart renders no Secret")

	cm := byKind(docs, "ConfigMap")["exchange"]
	require.NotNil(t, cm)
	for k := range path(cm, "data").(map[string]any) {
		for _, secret := range secretNames {
			assert.NotEqual(t, secret, k, "ConfigMap must not carry %s", secret)
		}
	}
	assert.Equal(t, "http://exchange-api:8080/.well-known/jwks.json", path(cm, "data", "JWT_JWKS_URL"))

	job := byKind(docs, "Job")["exchange-migrate"]
	require.NotNil(t, job)
	assert.Equal(t, "pre-install,pre-upgrade", path(job, "metadata", "annotations", "helm.sh/hook"))
	assert.Contains(t, path(job, "metadata", "annotations", "helm.sh/hook-delete-policy"), "before-hook-creation")

	pdbs := byKind(docs, "PodDisruptionBudget")
	assert.Contains(t, pdbs, "exchange-api", "api has two replicas by default")
	assert.Len(t, pdbs, 1, "a budget only where there is more than one pod")

	services := byKind(docs, "Service")
	for _, role := range []string{"api", "stream", "admin"} {
		assert.Contains(t, services, "exchange-"+role)
	}
	assert.Empty(t, byKind(docs, "Ingress"), "ingress is off by default")
}

// The singleton roles are refused a second replica by the templates rather
// than by a note in values.yaml. admin is in the list for a reason unlike the
// other three: it loses a webhook signing secret, permanently and silently,
// when a second process answers the redirect that reveals it
// (internal/admin/ui_webhooks.go, exchange.isSingleton in _helpers.tpl).
func TestChartRefusesASecondSingleton(t *testing.T) {
	helm := helmBinary(t)
	for _, role := range []string{"engine", "chain", "signer", "admin"} {
		cmd := exec.Command(helm, "template", "exchange", chartDir, "--set", "roles."+role+".replicas=2")
		out, err := cmd.CombinedOutput()
		require.Error(t, err, role)
		assert.Contains(t, string(out), "replicas must be 1", role)
	}
}

func TestChartKindValues(t *testing.T) {
	helm := helmBinary(t)
	docs := render(t, helm, "-f", filepath.Join(chartDir, "values-kind.yaml"))
	deployments := byKind(docs, "Deployment")
	for _, dep := range []string{"postgres", "nats", "redis", "anvil"} {
		d, ok := deployments["exchange-"+dep]
		require.True(t, ok, "dev %s", dep)
		assert.Equal(t, "pre-install,pre-upgrade", path(d, "metadata", "annotations", "helm.sh/hook"), "%s is a hook so the migrate hook finds it", dep)
		assert.Equal(t, "-10", path(d, "metadata", "annotations", "helm.sh/hook-weight"), dep)
	}
	jobs := byKind(docs, "Job")
	assert.Equal(t, "0", path(jobs["exchange-migrate"], "metadata", "annotations", "helm.sh/hook-weight"))
	assert.Equal(t, "5", path(jobs["exchange-bootstrap"], "metadata", "annotations", "helm.sh/hook-weight"), "seed and bootstrap after migrate")
	assert.Empty(t, byKind(docs, "Secret"))
	assert.Empty(t, byKind(docs, "PodDisruptionBudget"), "one api replica in kind")
	cm := byKind(docs, "ConfigMap")["exchange"]
	assert.Equal(t, "http://exchange-anvil:8545", path(cm, "data", "ETH_RPC_URL"), "the chart points at its own anvil")
	assert.Equal(t, "dev", path(cm, "data", "EXCHANGE_ENV"))
}

// TestChartKubeconform validates the rendered manifests against the
// Kubernetes API schemas when kubeconform is available (CI installs it).
func TestChartKubeconform(t *testing.T) {
	helm := helmBinary(t)
	kc := os.Getenv("KUBECONFORM")
	if kc == "" {
		var err error
		if kc, err = exec.LookPath("kubeconform"); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatal("kubeconform is not installed and CI is set")
			}
			t.Skip("kubeconform not found; set KUBECONFORM or add it to PATH")
		}
	}
	for _, values := range []string{"", filepath.Join(chartDir, "values-kind.yaml")} {
		args := []string{"template", "exchange", chartDir}
		if values != "" {
			args = append(args, "-f", values)
		}
		rendered, err := exec.Command(helm, args...).Output()
		require.NoError(t, err)
		cmd := exec.Command(kc, "-strict", "-summary", "-ignore-missing-schemas", "-kubernetes-version", "1.31.0")
		cmd.Stdin = bytes.NewReader(rendered)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "kubeconform (%q):\n%s", values, out)
	}
}

// TestChartPinsTheSameImagesAsCompose: the throwaway dependencies are the
// compose file's, so a version bump happens in both places or the test
// says so.
func TestChartPinsTheSameImagesAsCompose(t *testing.T) {
	values := map[string]any{}
	b, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(b, &values))
	compose := map[string]any{}
	b, err = os.ReadFile(filepath.Join("..", "compose", "compose.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(b, &compose))
	for _, dep := range []string{"postgres", "nats", "redis"} {
		assert.Equal(t, path(compose, "services", dep, "image"), path(values, "dev", dep, "image"), dep)
	}
	env, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	require.NoError(t, err)
	tag := regexp.MustCompile(`(?m)^FOUNDRY_TAG=(\S+)$`).FindStringSubmatch(string(env))
	require.Len(t, tag, 2)
	assert.Equal(t, "ghcr.io/foundry-rs/foundry:"+tag[1], path(values, "dev", "anvil", "image"), "FOUNDRY_TAG in .env.example")
}

// TestChartHooksReferenceOnlyHooks: Helm creates the release's regular
// resources only after every pre-install hook has succeeded, so a hook that
// mounts the release's own ConfigMap or runs under its ServiceAccount waits
// forever (the first kind install timed out on exactly that). A hook may
// reference other hooks, or objects that exist before the release (the
// existing Secrets, the ConfigMaps kind-secrets.sh applies).
func TestChartHooksReferenceOnlyHooks(t *testing.T) {
	helm := helmBinary(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"defaults", nil},
		{"kind", []string{"-f", filepath.Join(chartDir, "values-kind.yaml")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs := render(t, helm, tc.args...)
			regular := map[string]bool{} // "Kind/name" of every non-hook document
			for _, d := range docs {
				if path(d, "metadata", "annotations", "helm.sh/hook") == nil {
					regular[kind(d)+"/"+name(d)] = true
				}
			}
			checked := 0
			for _, d := range docs {
				if path(d, "metadata", "annotations", "helm.sh/hook") == nil {
					continue
				}
				spec, ok := path(d, "spec", "template", "spec").(map[string]any)
				if !ok {
					continue // a Service or similar: no pod
				}
				checked++
				hook := kind(d) + "/" + name(d)
				refuse := func(k, n string) {
					if n != "" && regular[k+"/"+n] {
						t.Errorf("hook %s references %s/%s, which Helm creates only after the hooks", hook, k, n)
					}
				}
				if sa, _ := spec["serviceAccountName"].(string); sa != "" {
					refuse("ServiceAccount", sa)
				}
				for _, v := range list(spec["volumes"]) {
					refuse("ConfigMap", str(path(v, "configMap", "name")))
					refuse("Secret", str(path(v, "secret", "secretName")))
				}
				for _, field := range []string{"containers", "initContainers"} {
					for _, c := range list(spec[field]) {
						for _, e := range list(path(c, "envFrom")) {
							refuse("ConfigMap", str(path(e, "configMapRef", "name")))
							refuse("Secret", str(path(e, "secretRef", "name")))
						}
						for _, e := range list(path(c, "env")) {
							refuse("ConfigMap", str(path(e, "valueFrom", "configMapKeyRef", "name")))
							refuse("Secret", str(path(e, "valueFrom", "secretKeyRef", "name")))
						}
					}
				}
			}
			assert.Positive(t, checked, "at least the migrate hook has a pod template")
		})
	}
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

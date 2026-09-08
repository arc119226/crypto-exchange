package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/api"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

func get(t *testing.T, h http.Handler, path string) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, rec.Body.String()
}

func TestReadyz(t *testing.T) {
	ok := func(context.Context) error { return nil }
	fail := func(context.Context) error { return errors.New("down") }

	c := NewChecker()
	c.Register("postgres", true, ok)
	c.Register("redis", false, ok)
	mux := newOpsMux(c, prometheus.NewRegistry(), false)

	code, body, _ := get(t, mux, "/healthz")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ok", body["status"])

	code, body, _ = get(t, mux, "/readyz")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "ok", body["status"])

	// optional failure → 200 degraded
	c2 := NewChecker()
	c2.Register("postgres", true, ok)
	c2.Register("redis", false, fail)
	code, body, _ = get(t, newOpsMux(c2, prometheus.NewRegistry(), false), "/readyz")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "degraded", body["status"])
	checks := body["checks"].(map[string]any)
	assert.Equal(t, "down", checks["redis"].(map[string]any)["err"])

	// required failure → 503 unavailable
	c3 := NewChecker()
	c3.Register("postgres", true, fail)
	code, body, _ = get(t, newOpsMux(c3, prometheus.NewRegistry(), false), "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "unavailable", body["status"])

	// draining → 503 even when everything is fine
	c.SetDraining()
	code, body, _ = get(t, mux, "/readyz")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "draining", body["status"])
}

func TestReadyzTimeout(t *testing.T) {
	c := NewChecker()
	c.timeout = 10_000_000 // 10 ms
	c.Register("slow", true, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	body, code := c.Evaluate(context.Background())
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body.Checks["slow"].Err, "deadline")
}

func TestMetricsAndPprof(t *testing.T) {
	reg := prometheus.NewRegistry()
	telemetry.BuildInfo(reg, "v1", "c", "api")
	c := NewChecker()
	code, _, raw := get(t, newOpsMux(c, reg, true), "/metrics")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, raw, `exchange_build_info{`)
	code, _, _ = get(t, newOpsMux(c, reg, true), "/debug/pprof/")
	assert.Equal(t, http.StatusOK, code)
	code, _, _ = get(t, newOpsMux(c, reg, false), "/debug/pprof/")
	assert.Equal(t, http.StatusNotFound, code, "pprof disabled outside dev")
}

// emptyRegistry is a registry.Reader with no rows: enough to prove the
// OpenAPI routes are mounted behind the middleware chain.
type emptyRegistry struct{}

func (emptyRegistry) ListAssets(context.Context, string) ([]registry.Asset, error) { return nil, nil }
func (emptyRegistry) GetAsset(context.Context, string, string) (registry.Asset, error) {
	return registry.Asset{}, registry.ErrNotFound
}
func (emptyRegistry) ListMarkets(context.Context, string) ([]registry.Market, error) { return nil, nil }
func (emptyRegistry) GetMarket(context.Context, string, string) (registry.Market, error) {
	return registry.Market{}, registry.ErrNotFound
}

func TestAPIServerFallbacks(t *testing.T) {
	cfg := Config{HTTPAddr: ":0", TenantID: "default"}
	log := telemetry.NewLogger(nopWriter{}, 0, "api", "dev")

	call := func(h http.Handler, method, path string) (int, http.Header, map[string]any) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(telemetry.RequestIDHeader, "corr-1")
		h.ServeHTTP(rec, req)
		var body map[string]any
		if rec.Body.Len() > 0 {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
		}
		return rec.Code, rec.Header(), body
	}

	t.Run("without routes the catch-all still produces problem+json with correlation id", func(t *testing.T) {
		h := newAPIRouter(log, telemetry.NewHTTPMetrics(prometheus.NewRegistry()), api.Deps{Tenant: cfg.TenantID})
		code, hdr, p := call(h, http.MethodGet, "/v1/nothing")
		assert.Equal(t, http.StatusNotFound, code)
		assert.Equal(t, "application/problem+json", hdr.Get("Content-Type"))
		assert.Equal(t, "corr-1", p["correlation_id"])
		assert.Equal(t, float64(404), p["status"])
	})

	t.Run("with routes: 200, 404 and 405 all carry the correlation id", func(t *testing.T) {
		h := newAPIRouter(log, telemetry.NewHTTPMetrics(prometheus.NewRegistry()), api.Deps{Tenant: cfg.TenantID, Registry: emptyRegistry{}})

		code, hdr, body := call(h, http.MethodGet, "/v1/markets")
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, "application/json", hdr.Get("Content-Type"))
		assert.Equal(t, "corr-1", hdr.Get(telemetry.RequestIDHeader))
		assert.Equal(t, []any{}, body["markets"], "empty list, not null")

		code, hdr, p := call(h, http.MethodGet, "/v1/nothing")
		assert.Equal(t, http.StatusNotFound, code)
		assert.Equal(t, "application/problem+json", hdr.Get("Content-Type"))
		assert.Equal(t, "corr-1", p["correlation_id"])

		code, hdr, p = call(h, http.MethodPost, "/v1/markets")
		assert.Equal(t, http.StatusMethodNotAllowed, code, "a registered path with the wrong method is 405, not 404")
		assert.Equal(t, "application/problem+json", hdr.Get("Content-Type"))
		assert.Equal(t, "corr-1", p["correlation_id"])
		assert.Equal(t, float64(405), p["status"])
	})
}

type nopWriter struct{}

func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }

func TestReadinessGaugeFollowsChecker(t *testing.T) {
	var fail atomic.Bool
	c := NewChecker()
	c.Register("dep", true, func(context.Context) error {
		if fail.Load() {
			return errors.New("down")
		}
		return nil
	})
	reg := prometheus.NewPedanticRegistry()
	g := newReadinessGauge(reg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.run(ctx, c, 5*time.Millisecond) }()

	require.Eventually(t, func() bool { return testutil.ToFloat64(g.ready) == 1 }, time.Second, time.Millisecond)
	fail.Store(true)
	require.Eventually(t, func() bool { return testutil.ToFloat64(g.ready) == 0 }, time.Second, time.Millisecond)
	fail.Store(false)
	require.Eventually(t, func() bool { return testutil.ToFloat64(g.ready) == 1 }, time.Second, time.Millisecond)
	c.SetDraining()
	require.Eventually(t, func() bool { return testutil.ToFloat64(g.ready) == 0 }, time.Second, time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	families, err := reg.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	assert.Equal(t, "exchange_ready", families[0].GetName())
}

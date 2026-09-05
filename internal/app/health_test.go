package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestAPIServerFallbacks(t *testing.T) {
	cfg := Config{HTTPAddr: ":0"}
	log := telemetry.NewLogger(nopWriter{}, 0, "api", "dev")
	srv := newAPIServer(cfg, log, telemetry.NewHTTPMetrics(prometheus.NewRegistry()), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/nothing", nil)
	req.Header.Set(telemetry.RequestIDHeader, "corr-1")
	srv.Handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	var p map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	assert.Equal(t, "corr-1", p["correlation_id"])
	assert.Equal(t, float64(404), p["status"])
}

type nopWriter struct{}

func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }

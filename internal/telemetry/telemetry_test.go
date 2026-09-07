package telemetry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoggerFixedFields(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, slog.LevelInfo, "api", "v1.2.3")
	l.Debug("hidden")
	l.Info("hello", slog.String("order_id", "o1"))
	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "hello", rec["msg"])
	assert.Equal(t, "api", rec["role"])
	assert.Equal(t, "v1.2.3", rec["version"])
	assert.Equal(t, "o1", rec["order_id"])
	assert.Contains(t, rec, "ts")
	assert.NotContains(t, rec, "time")
	assert.Equal(t, 1, strings.Count(buf.String(), "\n"), "debug must be filtered at info level")
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "": slog.LevelInfo, "warn": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError} {
		got, err := ParseLevel(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := ParseLevel("verbose")
	assert.Error(t, err)
}

func TestSecretNeverLogs(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, slog.LevelInfo, "api", "dev")
	s := Secret("hunter2-super-secret")
	l.Info("cfg", slog.Any("password", s), slog.String("dsn", RedactURL("postgres://ex_api:hunter2-super-secret@db:5432/exchange?sslmode=disable")))
	out := buf.String()
	assert.NotContains(t, out, "hunter2")
	assert.Contains(t, out, Redacted)
	assert.Contains(t, out, "postgres://ex_api:xxxxx@db:5432/exchange")
	assert.NotContains(t, fmt.Sprintf("%s %v %#v %+v", s, s, s, s), "hunter2")
	assert.Equal(t, "hunter2-super-secret", s.Reveal())
	assert.True(t, s.IsSet())
	assert.False(t, Secret("").IsSet())
	assert.Equal(t, Redacted, RedactURL("not a url at all"))
}

// RedactURL only removes userinfo, which is where a Postgres DSN keeps its
// password. A hosted RPC endpoint keeps its API key in the path instead, so
// it needs the other function -- this is not hypothetical, the startup config
// line printed a live Alchemy key in full until it used this.
func TestRedactEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"alchemy key in the path", "https://eth-sepolia.g.alchemy.com/v2/abc123", "https://eth-sepolia.g.alchemy.com/[redacted]"},
		{"infura project id", "https://sepolia.infura.io/v3/deadbeef", "https://sepolia.infura.io/[redacted]"},
		{"a trailing slash is still a path", "https://rpc.example.com/", "https://rpc.example.com/[redacted]"},
		{"websocket", "wss://eth-sepolia.g.alchemy.com/v2/abc123", "wss://eth-sepolia.g.alchemy.com/[redacted]"},
		{"nothing to hide", "http://anvil:8545", "http://anvil:8545"},
		// Not a URL: returned as-is. When the endpoint is malformed, the
		// malformed value is exactly what the operator needs to see.
		{"not a url", "eth-sepolia.g.alchemy.com", "eth-sepolia.g.alchemy.com"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactEndpoint(tc.in))
		})
	}
}

func TestCorrelationMiddleware(t *testing.T) {
	var buf bytes.Buffer
	base := NewLogger(&buf, slog.LevelInfo, "api", "dev")
	var seenID string
	h := CorrelationMiddleware(base)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = CorrelationID(r.Context())
		Logger(r.Context()).Info("in handler")
		w.WriteHeader(http.StatusNoContent)
	}))

	// caller-provided id is kept and echoed
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, "req-abc-123")
	h.ServeHTTP(rec, req)
	assert.Equal(t, "req-abc-123", seenID)
	assert.Equal(t, "req-abc-123", rec.Header().Get(RequestIDHeader))
	assert.Contains(t, buf.String(), `"correlation_id":"req-abc-123"`)

	// invalid ids (spaces, too long, non-ascii) are replaced
	for _, bad := range []string{"has space", strings.Repeat("a", 129), "ünïcode"} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set(RequestIDHeader, bad)
		h.ServeHTTP(rec, req)
		got := rec.Header().Get(RequestIDHeader)
		assert.NotEqual(t, bad, got)
		assert.Len(t, got, 32, "generated id is 16 random bytes hex")
	}

	// missing id is generated and unique
	ids := map[string]bool{}
	for i := 0; i < 5; i++ {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		ids[rec.Header().Get(RequestIDHeader)] = true
	}
	assert.Len(t, ids, 5)
}

func TestLoggerFallback(t *testing.T) {
	assert.Same(t, slog.Default(), Logger(t.Context()))
	assert.Equal(t, "", CorrelationID(t.Context()))
}

func TestHTTPMetricsMiddleware(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewHTTPMetrics(reg)
	h := m.Middleware(func(r *http.Request) string {
		if r.URL.Path == "/v1/markets/ETH-USDC" {
			return "/v1/markets/{symbol}"
		}
		return ""
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok")) // implicit 200
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/markets/ETH-USDC", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))

	families, err := reg.Gather()
	require.NoError(t, err)
	var got []string
	for _, f := range families {
		if f.GetName() != "http_request_duration_seconds" {
			continue
		}
		for _, mt := range f.GetMetric() {
			labels := map[string]string{}
			for _, lp := range mt.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			got = append(got, labels["route"]+" "+labels["method"]+" "+labels["status"])
		}
	}
	assert.ElementsMatch(t, []string{"/v1/markets/{symbol} GET 200", "unmatched GET 500"}, got)
	assert.Equal(t, 0.0, testutil.ToFloat64(m.inFlight))
}

func TestBuildInfo(t *testing.T) {
	reg := prometheus.NewRegistry()
	BuildInfo(reg, "v9", "abc123", "engine")
	families, err := reg.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	assert.Equal(t, "exchange_build_info", families[0].GetName())
	assert.Equal(t, 1.0, families[0].GetMetric()[0].GetGauge().GetValue())
	reg2 := NewRegistry()
	fams, err := reg2.Gather()
	require.NoError(t, err)
	assert.NotEmpty(t, fams, "go/process collectors registered")
}

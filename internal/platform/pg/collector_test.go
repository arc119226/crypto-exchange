package pg

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// pgxpool opens connections lazily, so a pool pointed at a closed port is
// enough to exercise the collector without a database.
func TestPoolCollectorReportsPoolStat(t *testing.T) {
	pc, err := pgxpool.ParseConfig("postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	require.NoError(t, err)
	pc.MaxConns = 7
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	require.NoError(t, err)
	defer pool.Close()

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(NewPoolCollector(pool)))

	problems, err := testutil.GatherAndLint(reg)
	require.NoError(t, err)
	require.Empty(t, problems)

	require.Equal(t, 7.0, testutil.ToFloat64(gaugeOf(t, reg, "db_pool_max", nil)))
	for _, state := range []string{"acquired", "idle", "constructing"} {
		require.Equal(t, 0.0, testutil.ToFloat64(gaugeOf(t, reg, "db_pool_connections", map[string]string{"state": state})), state)
	}
}

// gaugeOf pulls one sample out of the registry as a collector so
// testutil.ToFloat64 can read it.
func gaugeOf(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) prometheus.Collector {
	t.Helper()
	families, err := g.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
	metrics:
		for _, m := range f.GetMetric() {
			for k, v := range labels {
				found := false
				for _, l := range m.GetLabel() {
					if l.GetName() == k && l.GetValue() == v {
						found = true
					}
				}
				if !found {
					continue metrics
				}
			}
			gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: name})
			gauge.Set(m.GetGauge().GetValue())
			return gauge
		}
	}
	t.Fatalf("metric %s%v not gathered", name, labels)
	return nil
}

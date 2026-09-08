package pg

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// poolCollector exposes pgxpool.Stat as Prometheus gauges. The pool keeps
// its own counters; collecting on scrape means no goroutine and no sampling
// interval to get wrong (docs/plan-v1.0.md §15, the System dashboard's
// "PG connections").
type poolCollector struct {
	pool        *pgxpool.Pool
	connections *prometheus.Desc
	maxConns    *prometheus.Desc
}

// NewPoolCollector returns a collector for one pool. Register it once per
// process; a role that opens several pools (none do today) would need
// distinct const labels.
func NewPoolCollector(pool *pgxpool.Pool) prometheus.Collector {
	return &poolCollector{
		pool: pool,
		connections: prometheus.NewDesc("db_pool_connections",
			"Connections in the pgx pool by state: acquired (in use), idle, constructing (being opened).",
			[]string{"state"}, nil),
		maxConns: prometheus.NewDesc("db_pool_max",
			"Configured pool ceiling (DATABASE_MAX_CONNS).", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.connections
	ch <- c.maxConns
}

// Collect implements prometheus.Collector.
func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(s.AcquiredConns()), "acquired")
	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(s.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(s.ConstructingConns()), "constructing")
	ch <- prometheus.MustNewConstMetric(c.maxConns, prometheus.GaugeValue, float64(s.MaxConns()))
}

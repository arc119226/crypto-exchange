package eventbus

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the outbox instruments of docs/plan-v1.0.md §15.
type Metrics struct {
	backlog   prometheus.Gauge
	lag       prometheus.Gauge
	published prometheus.Counter
}

// NewMetrics registers outbox_backlog, outbox_relay_lag_seconds and
// outbox_published_total.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		backlog: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_backlog",
			Help: "Outbox rows not yet published to the event bus.",
		}),
		lag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_relay_lag_seconds",
			Help: "Age of the oldest unpublished outbox row.",
		}),
		published: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_published_total",
			Help: "Outbox rows published by the relay.",
		}),
	}
	reg.MustRegister(m.backlog, m.lag, m.published)
	return m
}

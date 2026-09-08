package eventbus

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the outbox instruments of docs/plan-v1.0.md §15.
type Metrics struct {
	backlog     prometheus.Gauge
	lag         prometheus.Gauge
	published   prometheus.Counter
	consumerLag *prometheus.GaugeVec
}

// NewMetrics registers outbox_backlog, outbox_relay_lag_seconds,
// outbox_published_total and event_consumer_lag. One instance serves every
// relay and consumer of a process: the names are global to the registry,
// so a second registration would panic (role=all runs the engine, the
// worker and the stream in one process).
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
		consumerLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "event_consumer_lag",
			Help: "Events the stream holds that a consumer has not received yet (JetStream pending), sampled every few seconds.",
		}, []string{"consumer"}),
	}
	reg.MustRegister(m.backlog, m.lag, m.published, m.consumerLag)
	return m
}

func (m *Metrics) observeLag(consumer string, pending uint64) {
	if m == nil {
		return
	}
	m.consumerLag.WithLabelValues(consumer).Set(float64(pending))
}

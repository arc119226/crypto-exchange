package stream

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the stream instruments of docs/plan-v1.0.md §15.
type Metrics struct {
	connections  *prometheus.GaugeVec
	sent         *prometheus.CounterVec
	slow         prometheus.Counter
	authFailures prometheus.Counter
	pushDelay    *prometheus.HistogramVec
}

// NewMetrics registers ws_connections, ws_messages_sent_total,
// ws_slow_client_disconnects_total, ws_auth_failures_total and
// stream_push_delay_seconds.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		connections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ws_connections",
			Help: "Open WebSocket connections by endpoint (public, private).",
		}, []string{"kind"}),
		sent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ws_messages_sent_total",
			Help: "Messages queued to clients by channel.",
		}, []string{"channel"}),
		slow: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ws_slow_client_disconnects_total",
			Help: "Connections closed because their send buffer was full.",
		}),
		authFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ws_auth_failures_total",
			Help: "Private connections refused for a missing or invalid token.",
		}),
		pushDelay: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "stream_push_delay_seconds",
			Help:    "Event occurred_at to the message being queued for a client, by channel (docs/plan-v1.0.md §3.3).",
			Buckets: []float64{.005, .01, .025, .05, .1, .2, .3, .5, 1, 2.5},
		}, []string{"channel"}),
	}
	if reg != nil {
		reg.MustRegister(m.connections, m.sent, m.slow, m.authFailures, m.pushDelay)
	}
	return m
}

func (m *Metrics) connOpened(kind string) {
	if m != nil {
		m.connections.WithLabelValues(kind).Inc()
	}
}

func (m *Metrics) connClosed(kind string) {
	if m != nil {
		m.connections.WithLabelValues(kind).Dec()
	}
}

func (m *Metrics) queued(channel string, n int) {
	if m != nil && n > 0 {
		m.sent.WithLabelValues(channel).Add(float64(n))
	}
}

func (m *Metrics) slowClient() {
	if m != nil {
		m.slow.Inc()
	}
}

func (m *Metrics) authFailed() {
	if m != nil {
		m.authFailures.Inc()
	}
}

func (m *Metrics) delay(channel string, occurredAt time.Time) {
	if m != nil && !occurredAt.IsZero() {
		m.pushDelay.WithLabelValues(channel).Observe(time.Since(occurredAt).Seconds())
	}
}

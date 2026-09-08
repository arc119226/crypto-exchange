package marketdata

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the market-data instruments.
type Metrics struct {
	rebuilds       *prometheus.CounterVec
	bookSeq        *prometheus.GaugeVec
	klineLag       *prometheus.GaugeVec
	snapshotWrites prometheus.Counter
}

// NewMetrics registers marketdata_book_rebuilds_total, marketdata_book_seq,
// marketdata_kline_writer_lag and marketdata_snapshot_writes_total.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		rebuilds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "marketdata_book_rebuilds_total",
			Help: "Shadow book rebuilds from trading.orders by market and reason (start, gap, inconsistent, overflow).",
		}, []string{"market", "reason"}),
		bookSeq: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "marketdata_book_seq",
			Help: "Engine seq the shadow book is current at; compare with engine_seq.",
		}, []string{"market"}),
		klineLag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "marketdata_kline_writer_lag",
			Help: "Engine commands the candle writer has not folded yet (market seq minus cursor).",
		}, []string{"market"}),
		snapshotWrites: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "marketdata_snapshot_writes_total",
			Help: "Depth snapshots written to the cache for the api role.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.rebuilds, m.bookSeq, m.klineLag, m.snapshotWrites)
	}
	return m
}

// ObserveRebuild counts one rebuild.
func (m *Metrics) ObserveRebuild(market, reason string) {
	if m != nil {
		m.rebuilds.WithLabelValues(market, reason).Inc()
	}
}

// ObserveBookSeq records the shadow book's seq.
func (m *Metrics) ObserveBookSeq(market string, seq uint64) {
	if m != nil {
		m.bookSeq.WithLabelValues(market).Set(float64(seq)) //nolint:forbidigo // Prometheus gauge, not money
	}
}

// ObserveSnapshotWrite counts one cache write.
func (m *Metrics) ObserveSnapshotWrite() {
	if m != nil {
		m.snapshotWrites.Inc()
	}
}

func (m *Metrics) observeKlineLag(market string, lag int64) {
	if m != nil {
		m.klineLag.WithLabelValues(market).Set(float64(lag)) //nolint:forbidigo // Prometheus gauge, not money
	}
}

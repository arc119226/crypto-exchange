package trading

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the engine instruments of docs/plan-v1.0.md §15.
type Metrics struct {
	queueDepth      *prometheus.GaugeVec
	applyDuration   *prometheus.HistogramVec
	orders          *prometheus.CounterVec
	trades          *prometheus.CounterVec
	seq             *prometheus.GaugeVec
	openOrders      *prometheus.GaugeVec
	rebuildDuration prometheus.Histogram
	rebuilds        prometheus.Counter
	batchSize       *prometheus.HistogramVec
	batchDuration   *prometheus.HistogramVec
	batchFallbacks  *prometheus.CounterVec
}

// NewMetrics registers the trading metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "trading_command_queue_depth", Help: "Commands waiting for the market's runner.",
		}, []string{"market"}),
		applyDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "trading_apply_duration_seconds", Help: "Time to apply one new-order command including its database transaction.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5}, //nolint:forbidigo // Prometheus API, not money
		}, []string{"market"}),
		orders: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "trading_orders_total", Help: "Orders by market, type and resulting status.",
		}, []string{"market", "type", "status"}),
		trades: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "trading_trades_total", Help: "Trades executed by market.",
		}, []string{"market"}),
		seq: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "engine_seq", Help: "Last committed command sequence per market.",
		}, []string{"market"}),
		openOrders: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "engine_open_orders", Help: "Resting orders in the book per market.",
		}, []string{"market"}),
		rebuildDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "engine_rebuild_duration_seconds", Help: "Time to rebuild one order book from the database.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}, //nolint:forbidigo // Prometheus API, not money
		}),
		rebuilds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "engine_rebuilds_total", Help: "Order book rebuilds after a failed transaction.",
		}),
		batchSize: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "trading_batch_size", Help: "Commands committed per transaction (group commit).",
			Buckets: []float64{1, 2, 4, 8, 16, 32, 50}, //nolint:forbidigo // Prometheus API, not money
		}, []string{"market"}),
		batchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "trading_batch_duration_seconds", Help: "Time from taking a group of commands off the queue to its COMMIT.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5}, //nolint:forbidigo // Prometheus API, not money
		}, []string{"market"}),
		batchFallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "trading_batch_fallbacks_total", Help: "Groups that failed as a whole and were re-run one command per transaction, by reason.",
		}, []string{"market", "reason"}),
	}
	reg.MustRegister(m.queueDepth, m.applyDuration, m.orders, m.trades, m.seq, m.openOrders, m.rebuildDuration, m.rebuilds,
		m.batchSize, m.batchDuration, m.batchFallbacks)
	return m
}

// observeBatch records the size of one committed group.
func (m *Metrics) observeBatch(market string, n int) {
	m.batchSize.WithLabelValues(market).Observe(float64(n)) //nolint:forbidigo // Prometheus histogram, not money
}

// observeQueue records the runner's queue length.
func (m *Metrics) observeQueue(market string, n int) {
	m.queueDepth.WithLabelValues(market).Set(float64(n)) //nolint:forbidigo // Prometheus gauge, not money
}

// observeBook records the last seq and the number of resting orders.
func (m *Metrics) observeBook(market string, seq uint64, open int) {
	m.seq.WithLabelValues(market).Set(float64(seq))         //nolint:forbidigo // Prometheus gauge, not money
	m.openOrders.WithLabelValues(market).Set(float64(open)) //nolint:forbidigo // Prometheus gauge, not money
}

// addTrades counts executed trades.
func (m *Metrics) addTrades(market string, n int) {
	m.trades.WithLabelValues(market).Add(float64(n)) //nolint:forbidigo // Prometheus counter, not money
}

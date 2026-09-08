package app

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// CheckFunc probes one dependency.
type CheckFunc func(context.Context) error

type check struct {
	name     string
	required bool
	fn       CheckFunc
}

// Checker aggregates readiness checks for /readyz.
type Checker struct {
	mu       sync.RWMutex
	checks   []check
	draining atomic.Bool
	timeout  time.Duration
}

// readinessSampleInterval is how often exchange_ready is refreshed; six
// samples fit in the alert's one-minute window.
const readinessSampleInterval = 10 * time.Second

// NewChecker returns a Checker with a 2 s per-check timeout.
func NewChecker() *Checker { return &Checker{timeout: 2 * time.Second} }

// Register adds a check. Required checks make /readyz return 503 when they
// fail; optional ones only mark the status "degraded".
func (c *Checker) Register(name string, required bool, fn CheckFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks = append(c.checks, check{name: name, required: required, fn: fn})
}

// SetDraining makes /readyz return 503 so load balancers stop routing to
// this instance while in-flight work completes.
func (c *Checker) SetDraining() { c.draining.Store(true) }

// CheckResult is the per-dependency readiness outcome.
type CheckResult struct {
	OK        bool   `json:"ok"`
	Required  bool   `json:"required"`
	LatencyMs int64  `json:"latency_ms"`
	Err       string `json:"err,omitempty"`
}

// Readiness is the /readyz body.
type Readiness struct {
	Status string                 `json:"status"` // ok | degraded | draining | unavailable
	Checks map[string]CheckResult `json:"checks"`
}

// Evaluate runs all checks concurrently and returns the body and HTTP status.
func (c *Checker) Evaluate(ctx context.Context) (Readiness, int) {
	c.mu.RLock()
	checks := make([]check, len(c.checks))
	copy(checks, c.checks)
	c.mu.RUnlock()

	results := make([]CheckResult, len(checks))
	var wg sync.WaitGroup
	for i, ch := range checks {
		wg.Add(1)
		go func(i int, ch check) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()
			start := time.Now()
			err := ch.fn(cctx)
			r := CheckResult{OK: err == nil, Required: ch.required, LatencyMs: time.Since(start).Milliseconds()}
			if err != nil {
				r.Err = err.Error()
			}
			results[i] = r
		}(i, ch)
	}
	wg.Wait()

	body := Readiness{Status: "ok", Checks: make(map[string]CheckResult, len(checks))}
	code := http.StatusOK
	for i, ch := range checks {
		body.Checks[ch.name] = results[i]
		if results[i].OK {
			continue
		}
		if ch.required {
			body.Status = "unavailable"
			code = http.StatusServiceUnavailable
		} else if body.Status == "ok" {
			body.Status = "degraded"
		}
	}
	if c.draining.Load() {
		body.Status = "draining"
		code = http.StatusServiceUnavailable
	}
	return body, code
}

// HealthzHandler answers liveness: the process is up.
func (c *Checker) HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
}

// ReadyzHandler answers readiness.
func (c *Checker) ReadyzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, code := c.Evaluate(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
}

// readinessGauge publishes exchange_ready (docs/plan-v1.0.md §15). The
// alert "readyz failed for more than a minute" has to come from the
// process's own view sampled on a clock: a load balancer's probes are not
// scraped, and a dead process has no /readyz at all (that case is `up == 0`,
// which the same rule covers). 1 means /readyz would answer 200 right now.
type readinessGauge struct {
	ready prometheus.Gauge
}

func newReadinessGauge(reg prometheus.Registerer) *readinessGauge {
	g := &readinessGauge{ready: prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "exchange_ready",
		Help: "1 when every required readiness check passes and the process is not draining; 0 otherwise.",
	})}
	reg.MustRegister(g.ready)
	return g
}

// run samples the checker immediately and then every interval until ctx
// ends. It returns nil on cancellation so it can sit in the role errgroup.
func (g *readinessGauge) run(ctx context.Context, c *Checker, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		_, code := c.Evaluate(ctx)
		if code == http.StatusOK {
			g.ready.Set(1)
		} else {
			g.ready.Set(0)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

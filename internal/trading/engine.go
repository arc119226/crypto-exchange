package trading

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/policy"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// lockRetry is how often Start retries the advisory lock while another
// engine instance holds it.
const lockRetry = time.Second

// defaultQueueSize bounds the commands waiting per market.
const defaultQueueSize = 1024

// DefaultBatchSize is how many queued commands a runner commits in one
// transaction at most (docs/plan-v1.0.md §5.2 group commit). MaxBatchSize
// is the ceiling: a failed group costs one book rebuild, so it stays small.
const (
	DefaultBatchSize = 50
	MaxBatchSize     = 50
)

// Engine runs one runner per market of a tenant. Exactly one engine per
// database may run: Start takes a Postgres advisory lock and blocks until
// it gets it (docs/plan-v1.0.md §5.1). Engine implements CommandBus for the
// in-process case (role=all, engine role itself).
type Engine struct {
	pool      *pgxpool.Pool
	ledger    *ledger.Service
	reg       *registry.Cache
	store     registry.Reader
	outbox    eventbus.Outbox
	policy    policy.OrderPolicy
	tenant    string
	log       *slog.Logger
	metrics   *Metrics
	queueSize int
	batchSize int

	beforeCommit func(market string, commands int) error // test hook, see WithFaultInjection

	enqueue sync.Mutex // PlaceMany's requests enter a queue without interleaving
	mu      sync.RWMutex
	runners map[string]*runner
	ready   atomic.Bool

	lockConn *pgxpool.Conn
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewEngine wires an engine. reg is loaded (and reloaded) from store by the
// engine; callers share it read-only.
func NewEngine(pool *pgxpool.Pool, l *ledger.Service, reg *registry.Cache, store registry.Reader, tenant string, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		pool: pool, ledger: l, reg: reg, store: store, policy: policy.Basic{}, tenant: tenant, log: log,
		queueSize: defaultQueueSize, batchSize: DefaultBatchSize, runners: map[string]*runner{},
	}
}

// WithMetrics attaches Prometheus instruments.
func (e *Engine) WithMetrics(m *Metrics) *Engine {
	e.metrics = m
	return e
}

// WithPolicy swaps the OrderPolicy NewEngine installs. It has no callers, in
// this repository or its tests: internal/policy ships one implementation,
// policy.Basic{}, and NewEngine takes it directly. The comment here used to
// claim tests used it, which one grep disproves -- so it said the opposite of
// what it should, which is that this is a seam kept open on purpose for a
// deployment that needs different order admission rules.
func (e *Engine) WithPolicy(p policy.OrderPolicy) *Engine {
	e.policy = p
	return e
}

// WithQueueSize sets the per-market command queue length.
func (e *Engine) WithQueueSize(n int) *Engine {
	if n > 0 {
		e.queueSize = n
	}
	return e
}

// WithBatchSize sets how many queued commands a runner commits per
// transaction (1..MaxBatchSize; 1 turns group commit off).
func (e *Engine) WithBatchSize(n int) *Engine {
	if n > 0 {
		e.batchSize = min(n, MaxBatchSize)
	}
	return e
}

// Registry returns the engine's live registry cache.
func (e *Engine) Registry() *registry.Cache { return e.reg }

// Ready reports whether the lock is held and every book is restored.
func (e *Engine) Ready() bool { return e.ready.Load() }

// ReadyCheck is a readiness probe for internal/app.
func (e *Engine) ReadyCheck(context.Context) error {
	if !e.Ready() {
		return errors.New("engine: order books not ready")
	}
	return nil
}

// lockKey is the advisory lock id of a tenant's engine.
func lockKey(tenant string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("exchange-engine:" + tenant))
	return int64(h.Sum64()) //nolint:gosec // wrap-around is fine for a lock id
}

// Start acquires the single-instance lock, loads the registry and restores
// one book per tradable market, then starts the runner goroutines. It
// returns once the engine is ready or ctx ends.
func (e *Engine) Start(ctx context.Context) error {
	if err := e.acquireLock(ctx); err != nil {
		return err
	}
	if err := e.reg.Load(ctx, e.store); err != nil {
		e.releaseLock()
		return fmt.Errorf("trading: load registry: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.cancel = cancel
	if err := e.startRunners(ctx, runCtx); err != nil {
		e.Stop()
		return err
	}
	e.ready.Store(true)
	e.log.Info("engine ready", slog.Int("markets", len(e.runners)))
	return nil
}

// acquireLock takes pg_advisory_lock on a dedicated connection, retrying
// while another instance holds it.
func (e *Engine) acquireLock(ctx context.Context) error {
	for attempt := 1; ; attempt++ {
		conn, err := e.pool.Acquire(ctx)
		if err != nil {
			return fmt.Errorf("trading: acquire connection: %w", err)
		}
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey(e.tenant)).Scan(&got); err != nil {
			conn.Release()
			return fmt.Errorf("trading: advisory lock: %w", err)
		}
		if got {
			e.lockConn = conn
			return nil
		}
		conn.Release()
		e.log.Warn("another engine instance holds the lock; waiting", slog.Int("attempt", attempt))
		select {
		case <-ctx.Done():
			return fmt.Errorf("trading: waiting for engine lock: %w", ctx.Err())
		case <-time.After(lockRetry):
		}
	}
}

func (e *Engine) releaseLock() {
	if e.lockConn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = e.lockConn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey(e.tenant))
	cancel()
	e.lockConn.Release()
	e.lockConn = nil
}

// tradable reports whether the engine runs a book for the market's status.
func tradable(m registry.Market) bool {
	switch m.Status {
	case registry.MarketActive, registry.MarketHalted, registry.MarketCancelOnly:
		return true
	}
	return false
}

// startRunners restores and starts a runner for every tradable market that
// has none yet. A market whose configuration the engine cannot build is
// logged and skipped; only every market failing is an error.
func (e *Engine) startRunners(ctx, runCtx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var wanted, failed int
	for _, m := range e.reg.Markets() {
		if !tradable(m) {
			continue
		}
		if _, exists := e.runners[m.Symbol]; exists {
			continue
		}
		wanted++
		r := newRunner(e, m)
		if err := r.restore(ctx); err != nil {
			// One market the engine cannot build must not stop the others.
			// Aborting here meant a single unrunnable configuration -- a
			// self-trade policy the matcher does not implement, a tick and
			// scale combination that cannot be exact -- left every market
			// without a runner and /readyz red. One market unavailable is a
			// far smaller failure than all of them.
			//
			// Not silent, though: this is the one path where a market
			// disappears without anyone asking it to, and with no
			// Alertmanager in the beta (docs/beta-checklist.md) the log is
			// where it will be found. If every market fails it is not a bad
			// row, it is a bad deployment, and the caller still gets an error.
			failed++
			e.log.Error("market has no runner: its configuration is one the engine cannot build",
				slog.String("market", m.Symbol), slog.String("error", err.Error()))
			continue
		}
		e.runners[m.Symbol] = r
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			r.run(runCtx)
		}()
	}
	if wanted > 0 && failed == wanted {
		return fmt.Errorf("trading: none of the %d tradable markets could be started", wanted)
	}
	return nil
}

// Reload re-reads the registry and starts runners for new markets. Status
// and fee changes of existing markets take effect immediately because
// runners read the cache per command (docs/plan-v1.0.md §6.6).
func (e *Engine) Reload(ctx context.Context) error {
	if err := e.reg.Load(ctx, e.store); err != nil {
		return fmt.Errorf("trading: reload registry: %w", err)
	}
	if e.cancel == nil {
		return errors.New("trading: engine not started")
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	prev := e.cancel
	e.cancel = func() { prev(); cancel() }
	return e.startRunners(ctx, runCtx)
}

// Stop ends every runner (in-flight commands finish), waits for them and
// releases the lock.
func (e *Engine) Stop() {
	e.ready.Store(false)
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()
	e.releaseLock()
}

// Markets lists the symbols with a running book.
func (e *Engine) Markets() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.runners))
	for s := range e.runners {
		out = append(out, s)
	}
	return out
}

func (e *Engine) runner(symbol string) (*runner, error) {
	e.mu.RLock()
	r, ok := e.runners[symbol]
	e.mu.RUnlock()
	if !ok {
		if _, known := e.reg.Market(symbol); known {
			return nil, fmt.Errorf("%w: market %s is not trading", ErrEngineUnavailable, symbol)
		}
		return nil, fmt.Errorf("%w: %s", ErrMarketNotFound, symbol)
	}
	return r, nil
}

// submit hands a request to the market's runner and waits for the answer.
func (e *Engine) submit(ctx context.Context, symbol string, req request) (response, error) {
	if !e.Ready() {
		return response{}, ErrEngineUnavailable
	}
	r, err := e.runner(symbol)
	if err != nil {
		return response{}, err
	}
	req.ctx = ctx
	req.reply = make(chan response, 1)
	e.enqueue.Lock()
	select {
	case r.cmds <- req:
		e.enqueue.Unlock()
	case <-ctx.Done():
		e.enqueue.Unlock()
		return response{}, fmt.Errorf("%w: queue full: %v", ErrEngineUnavailable, ctx.Err())
	}
	select {
	case res := <-req.reply:
		return res, res.err
	case <-ctx.Done():
		// the runner still executes the command; the caller only stops waiting
		return response{}, fmt.Errorf("%w: %v", ErrEngineUnavailable, ctx.Err())
	}
}

// Command is one of the two things ExecuteMany can hand a runner.
type Command struct {
	Place  *PlaceOrderRequest
	Cancel *CancelRequest
}

// CommandResult is ExecuteMany's answer for one Command: for a place the
// result, for a cancel the order in Result.Order.
type CommandResult struct {
	Result PlaceOrderResult
	Err    error
}

// ExecuteMany enqueues commands for one market back to back, so they
// reach the runner as one contiguous stretch of its queue (one group when
// they fit, docs/plan-v1.0.md §5.2). Results are positional. Tests use it
// to decide what a group contains; a bulk endpoint could too.
func (e *Engine) ExecuteMany(ctx context.Context, market string, cmds []Command) []CommandResult {
	results := make([]CommandResult, len(cmds))
	fail := func(from int, err error) {
		for i := from; i < len(results); i++ {
			results[i].Err = err
		}
	}
	if !e.Ready() {
		fail(0, ErrEngineUnavailable)
		return results
	}
	r, err := e.runner(market)
	if err != nil {
		fail(0, err)
		return results
	}
	queued := make([]request, 0, len(cmds))
	e.enqueue.Lock()
	for i := range cmds {
		req := request{ctx: ctx, place: cmds[i].Place, cancel: cmds[i].Cancel, reply: make(chan response, 1)}
		if req.place == nil && req.cancel == nil {
			results[i].Err = fmt.Errorf("%w: empty command", ErrInvalidRequest)
			continue
		}
		select {
		case r.cmds <- req:
			queued = append(queued, req)
			results[i].Err = nil
		case <-ctx.Done():
			e.enqueue.Unlock()
			fail(i, fmt.Errorf("%w: queue full: %v", ErrEngineUnavailable, ctx.Err()))
			return e.collect(ctx, queued, results)
		}
	}
	e.enqueue.Unlock()
	return e.collect(ctx, queued, results)
}

// collect waits for the queued requests' replies, in order; results
// already carrying an error were never queued.
func (e *Engine) collect(ctx context.Context, queued []request, results []CommandResult) []CommandResult {
	next := 0
	for i := range results {
		if results[i].Err != nil {
			continue
		}
		req := queued[next]
		next++
		select {
		case res := <-req.reply:
			results[i] = CommandResult{Result: res.result, Err: res.err}
		case <-ctx.Done():
			results[i].Err = fmt.Errorf("%w: %v", ErrEngineUnavailable, ctx.Err())
		}
	}
	return results
}

// WithFaultInjection installs a hook the runner calls right before every
// group's COMMIT; a non-nil error fails the group. For tests and chaos
// drills only: it is how "a group fails as a whole" is exercised without
// pulling the database's plug.
func (e *Engine) WithFaultInjection(f func(market string, commands int) error) *Engine {
	e.beforeCommit = f
	return e
}

// PlaceOrder implements CommandBus.
func (e *Engine) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (PlaceOrderResult, error) {
	res, err := e.submit(ctx, req.MarketSymbol, request{place: &req})
	return res.result, err
}

// CancelOrder implements CommandBus.
func (e *Engine) CancelOrder(ctx context.Context, market string, req CancelRequest) (Order, error) {
	res, err := e.submit(ctx, market, request{cancel: &req})
	return res.result.Order, err
}

// Depth implements CommandBus: the aggregated book straight from memory.
func (e *Engine) Depth(ctx context.Context, market string, n int) (matching.Depth, error) {
	if n <= 0 {
		n = 0
	}
	res, err := e.submit(ctx, market, request{query: true, depth: n})
	return res.depth, err
}

// Snapshot returns every resting order of a market (tests, replay checks).
func (e *Engine) Snapshot(ctx context.Context, market string) (matching.Snapshot, error) {
	res, err := e.submit(ctx, market, request{query: true, depth: -1})
	return res.snapshot, err
}

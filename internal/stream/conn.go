package stream

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// transport is the socket as the connection logic sees it, so tests can
// stand in a writer that blocks or a reader that feeds scripted frames.
type transport interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, b []byte) error
	Ping(ctx context.Context) error
	Close(code int, reason string) error
}

// wsTransport is the real socket.
type wsTransport struct{ c *websocket.Conn }

// Read implements transport.
func (t wsTransport) Read(ctx context.Context) ([]byte, error) {
	_, b, err := t.c.Read(ctx)
	return b, err
}

// Write implements transport.
func (t wsTransport) Write(ctx context.Context, b []byte) error {
	return t.c.Write(ctx, websocket.MessageText, b)
}

// Ping implements transport.
func (t wsTransport) Ping(ctx context.Context) error { return t.c.Ping(ctx) }

// Close implements transport.
func (t wsTransport) Close(code int, reason string) error {
	return t.c.Close(websocket.StatusCode(code), reason) //nolint:gosec // 1000..4999
}

// Close codes used here.
const (
	closeNormal    = int(websocket.StatusNormalClosure)
	closeGoingAway = int(websocket.StatusGoingAway)
	closePolicy    = int(websocket.StatusPolicyViolation)
)

// conn is one client connection: a reader (the caller's goroutine), a
// writer goroutine draining the bounded buffer, and a pinger.
//
// Closing goes through the writer: closeWith records the reason and asks
// the writer to close, so whatever was queued before the decision (an
// error message, say) still goes out ahead of the close frame. The socket
// library ends the connection itself when a write or ping context expires,
// which is what finally happens to a client that stopped reading.
type conn struct {
	id      uint64
	kind    string // public | private
	t       transport
	cfg     Config
	log     *slog.Logger
	metrics *Metrics

	out      chan []byte
	closeReq chan struct{}
	// base outlives the close decision: reads and writes run on it so the
	// library does not tear the socket down before the close frame is sent.
	base   context.Context
	ctx    context.Context
	cancel context.CancelFunc

	// closeMu guards the close decision; the first reason wins.
	closeMu     sync.Mutex
	closeCode   int
	closeReason string
	closing     bool

	// hub state, guarded by the hub's mutex
	subs    map[string]struct{}
	account string

	// private state, guarded by mu (step f)
	mu sync.Mutex
	pv privateState
}

func newConn(ctx context.Context, id uint64, kind string, t transport, cfg Config, m *Metrics, log *slog.Logger) *conn {
	cctx, cancel := context.WithCancel(ctx)
	return &conn{
		id: id, kind: kind, t: t, cfg: cfg, log: log.With(slog.Uint64("conn", id), slog.String("kind", kind)), metrics: m,
		out: make(chan []byte, cfg.WriteBuffer), closeReq: make(chan struct{}, 1), base: ctx, ctx: cctx, cancel: cancel,
		subs: map[string]struct{}{},
	}
}

// send queues b for the writer. A full buffer is a client that cannot keep
// up: it is closed rather than waited for (docs/plan-v1.0.md §7.5).
func (c *conn) send(b []byte) bool {
	c.closeMu.Lock()
	closing := c.closing
	c.closeMu.Unlock()
	if closing {
		return false
	}
	select {
	case <-c.ctx.Done():
		return false
	default:
	}
	select {
	case c.out <- b:
		return true
	default:
		if c.closeWith(closePolicy, ReasonSlowConsumer) {
			c.metrics.slowClient()
			c.log.Warn("slow client disconnected", slog.Int("buffer", c.cfg.WriteBuffer))
		}
		return false
	}
}

// closeWith records why the connection is closing and asks the writer to
// close it. It reports whether this call was the one that decided.
func (c *conn) closeWith(code int, reason string) bool {
	c.closeMu.Lock()
	if c.closing {
		c.closeMu.Unlock()
		return false
	}
	c.closing = true
	c.closeCode, c.closeReason = code, reason
	c.closeMu.Unlock()
	select {
	case c.closeReq <- struct{}{}:
	default:
	}
	return true
}

func (c *conn) closeStatus() (int, string) {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if !c.closing {
		return closeNormal, ""
	}
	return c.closeCode, c.closeReason
}

// writeLoop drains the buffer and, when asked, closes the socket after
// what was already queued. It ends the connection context last.
func (c *conn) writeLoop() {
	defer c.cancel()
	write := func(b []byte) bool {
		wctx, cancel := context.WithTimeout(c.base, c.cfg.WriteTimeout)
		err := c.t.Write(wctx, b)
		cancel()
		if err != nil {
			c.closeWith(closeGoingAway, "write failed")
			return false
		}
		return true
	}
	closeSocket := func() {
		code, reason := c.closeStatus()
		if err := c.t.Close(code, reason); err != nil && !errors.Is(err, context.Canceled) {
			c.log.Debug("close", slog.String("err", err.Error()))
		}
	}
	for {
		// A close request outranks queued frames. select picks among ready
		// cases at random, and against a peer that stopped reading every
		// queued frame costs a full write timeout, so without this the
		// close could trail the decision by WriteBuffer timeouts.
		select {
		case <-c.closeReq:
			c.flush(write)
			closeSocket()
			return
		default:
		}
		select {
		case <-c.base.Done():
			return
		case <-c.closeReq:
			c.flush(write)
			closeSocket()
			return
		case b := <-c.out:
			if !write(b) {
				// the socket is gone or the peer is not reading: nothing
				// queued behind this frame would get through either
				closeSocket()
				return
			}
		}
	}
}

// flush writes what was queued before the close decision, so an error
// message reaches the client ahead of the close frame. A slow consumer is
// the exception: the queue is full because the peer is not reading, and
// waiting on it once more per frame would only delay the close.
func (c *conn) flush(write func([]byte) bool) {
	if _, reason := c.closeStatus(); reason == ReasonSlowConsumer {
		return
	}
	for {
		select {
		case b := <-c.out:
			if !write(b) {
				return
			}
		default:
			return
		}
	}
}

// pingLoop keeps the connection honest: a ping every PingInterval, and a
// pong that does not arrive within PongTimeout closes it. The reader must
// be running for pongs to be seen, which it always is.
func (c *conn) pingLoop() {
	tick := time.NewTicker(c.cfg.PingInterval)
	defer tick.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tick.C:
			pctx, cancel := context.WithTimeout(c.ctx, c.cfg.PongTimeout)
			err := c.t.Ping(pctx)
			cancel()
			if err != nil && c.ctx.Err() == nil {
				c.closeWith(closePolicy, ReasonPongTimeout)
				return
			}
		}
	}
}

// readLoop decodes client messages until the connection ends. Malformed
// frames get an error message; a client that keeps sending them is closed.
func (c *conn) readLoop(handle func(m clientMessage)) {
	bad := 0
	for {
		b, err := c.t.Read(c.base)
		if err != nil {
			if c.base.Err() == nil {
				code := websocket.CloseStatus(err)
				if code == -1 || (code != websocket.StatusNormalClosure && code != websocket.StatusGoingAway) {
					c.log.Debug("read ended", slog.String("err", err.Error()))
				}
				c.closeWith(closeNormal, "")
			}
			return
		}
		var m clientMessage
		if err := json.Unmarshal(b, &m); err != nil || m.Op == "" {
			bad++
			c.sendError(CodeBadMessage, "expected {\"op\": ...}")
			if bad >= 5 {
				c.closeWith(closePolicy, ReasonBadMessage)
				return
			}
			continue
		}
		handle(m)
	}
}

func (c *conn) sendJSON(v any) bool { return c.send(mustJSON(v)) }

func (c *conn) sendError(code, msg string) bool {
	return c.sendJSON(errorMessage{Type: "error", Code: code, Message: msg})
}

// serve runs the connection to completion: writer and pinger in the
// background, the reader here. When the reader ends -- the peer closed, or
// the writer closed the socket -- the writer is asked to close (a no-op if
// it already did) and everything is waited for.
func (c *conn) serve(handle func(m clientMessage), onClose func()) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.writeLoop() }()
	go func() { defer wg.Done(); c.pingLoop() }()
	c.readLoop(handle)
	c.closeWith(closeNormal, "")
	wg.Wait()
	onClose()
}

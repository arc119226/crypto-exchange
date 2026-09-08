package stream

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
)

// TokenVerifier checks a private connection's JWT: *auth.Verifier in one
// process, *auth.RemoteVerifier against the api role's JWKS otherwise.
type TokenVerifier interface {
	Verify(token string, audience string, now time.Time) (auth.Claims, error)
}

// OutboxReader replays an account's events for resume: *eventbus.OutboxReader.
type OutboxReader interface {
	ByAccountSince(ctx context.Context, tenant, accountID string, sinceSeq int64, limit int32) ([]eventbus.Envelope, error)
}

// AccountSeqReader reads where an account's private sequence stands:
// *marketdata.Store.
type AccountSeqReader interface {
	AccountSeq(ctx context.Context, accountID string) (int64, error)
}

// Deps are the server's collaborators. A nil Verifier refuses every
// private connection with auth_failed (a dev role without a JWKS).
type Deps struct {
	Tenant   string
	Feed     *Feed
	Hub      *Hub
	Verifier TokenVerifier
	Outbox   OutboxReader
	Accounts AccountSeqReader
	Metrics  *Metrics
	Log      *slog.Logger
}

// Server serves /ws/v1/public and /ws/v1/private.
type Server struct {
	cfg    Config
	d      Deps
	nextID atomic.Uint64
	conns  sync.WaitGroup
	base   context.Context
	cancel context.CancelFunc
}

// New builds a server. ctx bounds every connection: cancelling it (or
// CloseAll) ends them all.
func New(ctx context.Context, cfg Config, d Deps) *Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	cfg = cfg.withDefaults()
	base, cancel := context.WithCancel(ctx)
	s := &Server{cfg: cfg, d: d, base: base, cancel: cancel}
	d.Feed.private = s.deliverEvent
	return s
}

// Handler routes the two endpoints.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Get("/ws/v1/public", s.servePublic)
	r.Get("/ws/v1/private", s.servePrivate)
	return r
}

// CloseAll closes every connection (1001 going away) and waits for the
// close handshakes, or for ctx; whatever is still open then is torn down.
func (s *Server) CloseAll(ctx context.Context) {
	s.d.Hub.closeAll(closeGoingAway, "shutdown")
	done := make(chan struct{})
	go func() { s.conns.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	s.cancel()
}

// Connections counts open connections.
func (s *Server) Connections() int { return s.d.Hub.Connections() }

func (s *Server) accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, bool) {
	opts := &websocket.AcceptOptions{OriginPatterns: s.cfg.AllowedOrigins, InsecureSkipVerify: s.cfg.allowsAnyOrigin()}
	ws, err := websocket.Accept(w, r, opts)
	if err != nil {
		// Accept has already answered the HTTP request (403 on a bad origin,
		// 400 on a non-upgrade)
		s.d.Log.Debug("websocket accept refused", slog.String("err", err.Error()), slog.String("origin", r.Header.Get("Origin")))
		return nil, false
	}
	ws.SetReadLimit(s.cfg.MaxMessageBytes)
	return ws, true
}

func (s *Server) open(kind string, t transport) *conn {
	c := newConn(s.base, s.nextID.Add(1), kind, t, s.cfg, s.d.Metrics, s.d.Log)
	s.d.Hub.add(c)
	s.d.Metrics.connOpened(kind)
	s.conns.Add(1)
	return c
}

func (s *Server) run(c *conn, handle func(clientMessage)) {
	c.serve(handle, func() {
		s.d.Hub.remove(c)
		s.d.Metrics.connClosed(c.kind)
		s.conns.Done()
	})
}

func (s *Server) servePublic(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.accept(w, r)
	if !ok {
		return
	}
	c := s.open("public", wsTransport{c: ws})
	s.run(c, func(m clientMessage) { s.handlePublicOp(c, m) })
}

// handlePublicOp is the operations every connection understands.
func (s *Server) handlePublicOp(c *conn, m clientMessage) {
	switch m.Op {
	case OpSubscribe:
		s.subscribe(c, m)
	case OpUnsubscribe:
		s.unsubscribe(c, m)
	case OpPing:
		c.sendJSON(ackMessage{Type: "pong"})
	case OpAuth, OpResume:
		c.sendError(CodeNotAvailable, m.Op+" is for /ws/v1/private")
	default:
		c.sendError(CodeBadMessage, "unknown op "+m.Op)
	}
}

func (s *Server) subscribe(c *conn, m clientMessage) {
	ch, ok := parseChannel(m.Channel)
	if !ok {
		c.sendError(CodeUnknownChannel, "no channel "+m.Channel)
		return
	}
	if m.Market == "" {
		c.sendError(CodeBadMessage, "subscribe needs a market")
		return
	}
	if len(c.subs) >= s.cfg.MaxSubscriptions {
		c.sendError(CodeTooManySubscriptions, "at most "+itoa(s.cfg.MaxSubscriptions)+" subscriptions per connection")
		return
	}
	if err := s.d.Feed.Subscribe(c, ch, m.Market); err != nil {
		c.sendError(CodeUnknownMarket, "no market "+m.Market)
		return
	}
	c.sendJSON(ackMessage{Type: "subscribed", Channel: m.Channel, Market: m.Market})
}

func (s *Server) unsubscribe(c *conn, m clientMessage) {
	ch, ok := parseChannel(m.Channel)
	if !ok {
		c.sendError(CodeUnknownChannel, "no channel "+m.Channel)
		return
	}
	s.d.Feed.Unsubscribe(c, ch, m.Market)
	c.sendJSON(ackMessage{Type: "unsubscribed", Channel: m.Channel, Market: m.Market})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

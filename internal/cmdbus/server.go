package cmdbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// Verifier checks the internal token. *auth.Verifier and
// *auth.RemoteVerifier both satisfy it.
type Verifier interface {
	Verify(token string, audience string, now time.Time) (auth.Claims, error)
}

// ServerConfig configures the engine side of the bus.
type ServerConfig struct {
	Tenant        string
	SubjectPrefix string
	Timeout       time.Duration
	// Verifier checks the api role's internal token. Nil accepts unsigned
	// commands, which is only safe on a trusted local bus (dev); Serve logs
	// a warning so it is never silent.
	Verifier Verifier
	Metrics  *Metrics
	Logger   *slog.Logger
	Now      func() time.Time
}

func (c ServerConfig) withDefaults() ServerConfig {
	if c.Tenant == "" {
		c.Tenant = "default"
	}
	if c.SubjectPrefix == "" {
		c.SubjectPrefix = DefaultSubjectPrefix
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	return c
}

// Server answers trading commands for every market of one tenant.
type Server struct {
	cfg ServerConfig
	eng trading.CommandBus
	sub *nats.Subscription
}

// Serve subscribes to the tenant's command subjects and answers them from
// eng until Close.
//
// One wildcard subscription covers every market rather than one per market:
// a market added by a registry reload is then served without re-subscribing,
// and the per-market subject is still what clients publish to, so a future
// sharded engine can split the wildcard without a client change.
func Serve(nc *nats.Conn, eng trading.CommandBus, cfg ServerConfig) (*Server, error) {
	if nc == nil {
		return nil, errors.New("cmdbus: nats connection required")
	}
	if eng == nil {
		return nil, errors.New("cmdbus: engine required")
	}
	cfg = cfg.withDefaults()
	s := &Server{cfg: cfg, eng: eng}
	subject := cfg.SubjectPrefix + "." + cfg.Tenant + ".*"
	sub, err := nc.QueueSubscribe(subject, queueGroup, s.handle)
	if err != nil {
		return nil, fmt.Errorf("cmdbus: subscribe %s: %w", subject, err)
	}
	s.sub = sub
	if cfg.Verifier == nil {
		cfg.Logger.Warn("command bus accepts unsigned commands: no JWKS configured (dev only)")
	}
	cfg.Logger.Info("command bus listening", slog.String("subject", subject), slog.String("queue", queueGroup))
	return s, nil
}

// Close drains the subscription; in-flight commands still answer.
func (s *Server) Close() error {
	if s.sub == nil {
		return nil
	}
	if err := s.sub.Drain(); err != nil {
		return fmt.Errorf("cmdbus: drain: %w", err)
	}
	return nil
}

// handle answers one command. It always replies — a caller waiting on a
// request must never be left to time out because of a decode error or a
// panic in the engine.
func (s *Server) handle(msg *nats.Msg) {
	start := s.cfg.Now()
	var req Request
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		s.reply(msg, Response{Error: &Error{Kind: KindInvalidRequest, Message: "cmdbus: decode request: " + err.Error()}}, "", start)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	log := s.cfg.Logger.With(slog.String("op", string(req.Op)), slog.String("market", req.Market))
	if cid := msg.Header.Get(HeaderCorrelationID); cid != "" {
		ctx = telemetry.WithCorrelationID(ctx, cid)
		log = log.With(slog.String(telemetry.CorrelationIDKey, cid))
	}
	ctx = telemetry.WithLogger(ctx, log)

	defer func() {
		if r := recover(); r != nil {
			log.Error("command bus handler panicked", slog.Any("panic", r))
			s.reply(msg, Response{Error: &Error{Kind: KindInternal, Message: "cmdbus: handler panicked"}}, string(req.Op), start)
		}
	}()

	resp := s.dispatch(ctx, req)
	if resp.Error != nil && resp.Error.Kind == KindInternal {
		log.Error("command failed", slog.String("err", resp.Error.Message))
	}
	s.reply(msg, resp, string(req.Op), start)
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	if err := s.authorize(req); err != nil {
		return Response{Error: errorOf(err)}
	}
	switch req.Op {
	case OpPlace:
		if req.Place == nil {
			return Response{Error: errorOf(fmt.Errorf("%w: place command without a body", trading.ErrInvalidRequest))}
		}
		res, err := s.eng.PlaceOrder(ctx, *req.Place)
		if err != nil {
			return Response{Error: errorOf(err)}
		}
		return Response{Result: &res}
	case OpCancel:
		if req.Cancel == nil {
			return Response{Error: errorOf(fmt.Errorf("%w: cancel command without a body", trading.ErrInvalidRequest))}
		}
		order, err := s.eng.CancelOrder(ctx, req.Market, *req.Cancel)
		if err != nil {
			return Response{Error: errorOf(err)}
		}
		return Response{Order: &order}
	case OpDepth:
		book, err := s.eng.Depth(ctx, req.Market, req.Depth)
		if err != nil {
			return Response{Error: errorOf(err)}
		}
		return Response{Book: &book}
	default:
		return Response{Error: errorOf(fmt.Errorf("%w: unknown op %q", trading.ErrInvalidRequest, req.Op))}
	}
}

// authorize verifies the internal token and binds it to the command: the
// engine trusts the token, never the account id in the body
// (docs/plan-v1.0.md §14).
func (s *Server) authorize(req Request) error {
	if s.cfg.Verifier == nil || req.Op == OpDepth {
		return nil // depth is public data; a nil verifier is the dev bus
	}
	claims, err := s.cfg.Verifier.Verify(req.Token, auth.AudienceInternal, s.cfg.Now())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRejected, err)
	}
	if claims.TenantID != s.cfg.Tenant {
		return fmt.Errorf("%w: token tenant %q is not %q", ErrRejected, claims.TenantID, s.cfg.Tenant)
	}
	if !slices.Contains(claims.Scopes, auth.ScopeTrade) {
		return fmt.Errorf("%w: token lacks the %s scope", ErrRejected, auth.ScopeTrade)
	}
	var account string
	switch {
	case req.Place != nil:
		account = req.Place.AccountID
	case req.Cancel != nil:
		account = req.Cancel.AccountID
	}
	if account != claims.AccountID {
		return fmt.Errorf("%w: command account does not match the token", ErrRejected)
	}
	return nil
}

func (s *Server) reply(msg *nats.Msg, resp Response, op string, start time.Time) {
	body, err := json.Marshal(resp)
	if err != nil {
		body = []byte(`{"error":{"kind":"internal","message":"cmdbus: encode response failed"}}`)
	}
	if err := msg.Respond(body); err != nil {
		s.cfg.Logger.Error("command bus reply failed", slog.String("err", err.Error()))
	}
	label := "ok"
	if resp.Error != nil {
		label = string(resp.Error.Kind)
	}
	s.cfg.Metrics.observeServer(op, label, s.cfg.Now().Sub(start))
}

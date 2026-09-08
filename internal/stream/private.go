package stream

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
)

// privateState is the authentication and resume state of a private
// connection (docs/ws-api.md "Private channels").
//
// After the auth acknowledgement, live frames are held for one resume
// window: a client that sends resume gets the outbox replayed first and the
// held frames merged after it, deduplicated on account_seq; a client that
// sends anything else, or nothing, goes live from now. This is what makes
// a reconnect neither lose nor repeat an event -- a frame delivered
// between the acknowledgement and the resume would otherwise be either a
// duplicate of the replay or a gap in it.
type privateState struct {
	authed    bool
	account   string
	holding   bool
	live      bool
	held      []privateFrame
	holdTimer *time.Timer
	authTimer *time.Timer
}

func (s *Server) servePrivate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.accept(w, r)
	if !ok {
		return
	}
	c := s.open("private", wsTransport{c: ws})
	c.pv.authTimer = time.AfterFunc(s.cfg.AuthTimeout, func() {
		c.mu.Lock()
		authed := c.pv.authed
		c.mu.Unlock()
		if !authed {
			c.closeWith(closePolicy, ReasonAuthRequired)
		}
	})
	s.run(c, func(m clientMessage) { s.handlePrivateOp(c, m) })
	c.pv.authTimer.Stop()
	c.mu.Lock()
	if c.pv.holdTimer != nil {
		c.pv.holdTimer.Stop()
	}
	c.mu.Unlock()
}

func (s *Server) handlePrivateOp(c *conn, m clientMessage) {
	c.mu.Lock()
	authed, holding := c.pv.authed, c.pv.holding
	c.mu.Unlock()
	switch {
	case m.Op == OpAuth:
		s.authenticate(c, m)
		return
	case m.Op == OpPing:
		c.sendJSON(ackMessage{Type: "pong"})
		return
	case !authed:
		c.sendError(CodeNotAuthenticated, "send {\"op\":\"auth\",\"token\":...} first")
		return
	case m.Op == OpResume:
		s.resume(c, m)
		return
	}
	if holding {
		// the first frame after the acknowledgement decides: this one is not a
		// resume, so live starts now
		s.goLive(c, nil)
	}
	s.handlePublicOp(c, m)
}

// authenticate binds the connection to the token's account and answers
// with the account's current sequence, read after the binding so that no
// event can fall between the two.
func (s *Server) authenticate(c *conn, m clientMessage) {
	c.mu.Lock()
	already := c.pv.authed
	c.mu.Unlock()
	if already {
		c.sendError(CodeBadMessage, "already authenticated")
		return
	}
	if s.d.Verifier == nil {
		s.d.Metrics.authFailed()
		c.sendError(CodeAuthFailed, "private channels are not available: no token verifier configured")
		c.closeWith(closePolicy, ReasonAuthFailed)
		return
	}
	claims, err := s.d.Verifier.Verify(m.Token, auth.AudiencePublic, time.Now())
	if err == nil && claims.TenantID != s.d.Tenant {
		err = errors.New("tenant mismatch")
	}
	if err == nil && claims.AccountID == "" {
		err = errors.New("token carries no account")
	}
	if err != nil {
		s.d.Metrics.authFailed()
		c.sendError(CodeAuthFailed, "invalid token")
		c.closeWith(closePolicy, ReasonAuthFailed)
		return
	}
	s.d.Hub.bind(c, claims.AccountID)
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	seq, err := s.d.Accounts.AccountSeq(ctx, claims.AccountID)
	cancel()
	if err != nil {
		s.d.Log.Error("account seq read failed", slog.String("account", claims.AccountID), slog.String("err", err.Error()))
		c.sendError(CodeAuthFailed, "account unavailable")
		c.closeWith(closePolicy, ReasonAuthFailed)
		return
	}
	c.mu.Lock()
	c.pv.authed, c.pv.account, c.pv.holding = true, claims.AccountID, true
	c.pv.holdTimer = time.AfterFunc(s.cfg.ResumeWindow, func() { s.goLive(c, nil) })
	c.mu.Unlock()
	c.sendJSON(authAck{Type: "auth", AccountID: claims.AccountID, AccountSeq: seq})
	c.log.Info("private connection authenticated", slog.String("account", claims.AccountID), slog.Int64("account_seq", seq))
}

// goLive ends the hold: held frames after the last replayed sequence go
// out in order, later ones go straight through. lastReplayed nil means no
// replay happened and everything held is sent.
func (s *Server) goLive(c *conn, lastReplayed *int64) {
	c.mu.Lock()
	if !c.pv.holding {
		c.mu.Unlock()
		return
	}
	c.pv.holding, c.pv.live = false, true
	if c.pv.holdTimer != nil {
		c.pv.holdTimer.Stop()
	}
	held := c.pv.held
	c.pv.held = nil
	for _, f := range held {
		if lastReplayed != nil && f.accountSeq != nil && *f.accountSeq <= *lastReplayed {
			continue
		}
		c.send(f.b)
		s.d.Metrics.queued(f.channel, 1)
	}
	c.mu.Unlock()
}

// resume replays the account's events after since_seq from the outbox,
// then merges what was held meanwhile.
func (s *Server) resume(c *conn, m clientMessage) {
	if m.SinceSeq == nil || *m.SinceSeq < 0 {
		c.sendError(CodeBadMessage, "resume needs since_seq >= 0")
		return
	}
	c.mu.Lock()
	holding := c.pv.holding
	account := c.pv.account
	if holding && c.pv.holdTimer != nil {
		c.pv.holdTimer.Stop() // the replay may take longer than the window
	}
	c.mu.Unlock()
	if !holding {
		c.sendError(CodeResumeTooLate, "resume must be the first message after the auth acknowledgement")
		return
	}
	if s.d.Outbox == nil {
		c.sendError(CodeResumeFailed, "resume is not available")
		s.goLive(c, nil)
		return
	}
	since := *m.SinceSeq
	last := since
	replayed := 0
	for {
		ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
		page, err := s.d.Outbox.ByAccountSince(ctx, s.d.Tenant, account, last, s.cfg.ResumePageSize)
		cancel()
		if err != nil {
			s.d.Log.Error("resume replay failed", slog.String("account", account), slog.String("err", err.Error()))
			c.sendError(CodeResumeFailed, "replay failed; reconnect")
			c.closeWith(closeGoingAway, "resume failed")
			return
		}
		for _, e := range page {
			for _, f := range privateFrames(e) {
				if !c.send(f.b) {
					return
				}
				s.d.Metrics.queued(f.channel, 1)
				replayed++
			}
			if e.AccountSeq != nil {
				last = *e.AccountSeq
			}
		}
		if int32(len(page)) < s.cfg.ResumePageSize { //nolint:gosec // page size is small
			break
		}
	}
	s.goLive(c, &last)
	c.sendJSON(resumedAck{Type: "resumed", SinceSeq: since, Replayed: replayed})
}

// deliverEvent is the feed's hook for account-scoped events: every
// connection of the account gets the frame, held or live.
func (s *Server) deliverEvent(e eventbus.Envelope) {
	for _, f := range privateFrames(e) {
		account := f.account
		s.d.Hub.publishAccount(account, func(c *conn) {
			c.mu.Lock()
			if c.pv.holding {
				// a replay is a few outbox pages; the hold outlasts one send
				// buffer so an active account is not cut off mid-resume
				if len(c.pv.held) >= 4*s.cfg.WriteBuffer {
					c.mu.Unlock()
					if c.closeWith(closePolicy, ReasonSlowConsumer) {
						s.d.Metrics.slowClient()
					}
					return
				}
				c.pv.held = append(c.pv.held, f)
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()
			if c.send(f.b) {
				s.d.Metrics.queued(f.channel, 1)
				s.d.Metrics.delay(f.channel, f.at)
			}
		})
	}
}

// privateFrame is one message to one account.
type privateFrame struct {
	account    string
	accountSeq *int64
	channel    string
	at         time.Time
	b          []byte
}

// privateFrames maps an envelope to the private messages it produces:
// none for market-only events, one for account events, two for a trade
// (maker and taker, on the fills channel, not resumable).
func privateFrames(e eventbus.Envelope) []privateFrame {
	if e.EventType == marketdata.EventTradeExecuted {
		var t struct {
			Maker string `json:"maker_account_id"`
			Taker string `json:"taker_account_id"`
		}
		if err := json.Unmarshal(e.Payload, &t); err != nil {
			return nil
		}
		out := make([]privateFrame, 0, 2)
		for _, party := range []struct{ account, role string }{{t.Maker, "maker"}, {t.Taker, "taker"}} {
			if party.account == "" {
				continue
			}
			msg := privateMessage{Channel: ChannelFills, Type: e.EventType, Role: party.role, Seq: e.Seq, EventID: e.EventID, OccurredAt: e.OccurredAt, Data: e.Payload}
			out = append(out, privateFrame{account: party.account, channel: ChannelFills, at: e.OccurredAt, b: mustJSON(msg)})
		}
		return out
	}
	if e.AccountID == nil || *e.AccountID == "" {
		return nil
	}
	ch := privateChannel(e.EventType)
	if ch == "" {
		return nil
	}
	msg := privateMessage{Channel: ch, Type: e.EventType, Seq: e.Seq, AccountSeq: e.AccountSeq, EventID: e.EventID, OccurredAt: e.OccurredAt, Data: e.Payload}
	return []privateFrame{{account: *e.AccountID, accountSeq: e.AccountSeq, channel: ch, at: e.OccurredAt, b: mustJSON(msg)}}
}

// privateChannel maps an event type to its channel; "" for events the
// private stream does not carry.
func privateChannel(eventType string) string {
	domain, _, _ := strings.Cut(eventType, ".")
	switch domain {
	case "order":
		return ChannelOrders
	case "balance":
		return ChannelBalances
	case "deposit":
		return ChannelDeposits
	case "withdrawal":
		return ChannelWithdrawals
	}
	return ""
}

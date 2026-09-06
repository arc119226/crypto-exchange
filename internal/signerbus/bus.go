// Package signerbus carries signing requests from the chain role to the signer
// role over NATS request-reply (docs/plan-v1.0.md §5.1, §6.6).
//
// It lives outside internal/chain/signer for the same reason internal/cmdbus
// lives outside internal/trading: the signer owns the Signer interface and
// must not depend on NATS. Here the two meet -- the wire format, the subject
// cmd.signer.<tenant>, and the error classification that lets the caller tell
// a refusal (never retry) from an unreachable signer (safe to retry).
package signerbus

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/nats-io/nats.go"

	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// DefaultSubjectPrefix is the first tokens of the signer's subject; the full
// subject is <prefix>.<tenant>.
const DefaultSubjectPrefix = "cmd.signer"

// queueGroup keeps a second signer instance from answering the same request
// twice. Two signers holding the same seed would both produce a valid
// signature, and the signing log would refuse the second — a queue group turns
// that race into an ordinary one-of-N delivery.
const queueGroup = "signer"

// wireRequest is Request on the wire. Amounts travel as decimal strings and
// fees as decimal strings too, because a JSON number cannot hold either
// without loss (docs/plan-v1.0.md §6.5).
type wireRequest struct {
	// Op is "sign" (or empty, the default) or "hot_wallet". The address query
	// exists so the chain role learns the hot wallet from the only process
	// that can derive it, rather than from a config value that could disagree
	// with the key actually signing.
	Op      string      `json:"op,omitempty"`
	Kind    signer.Kind `json:"kind"`
	RefID   string      `json:"ref_id"`
	Attempt int32       `json:"attempt"`
	ChainID int64       `json:"chain_id"`
	To      string      `json:"to"`
	Asset   string      `json:"asset"`
	Value   string      `json:"value"`
	Nonce   uint64      `json:"nonce"`
	Gas     uint64      `json:"gas"`
	TipCap  string      `json:"tip_cap"`
	FeeCap  string      `json:"fee_cap"`
}

// wireResponse is Result or a failure. RawTx is base64 because JSON has no
// bytes; it is the signed transaction and never leaves this pair of roles.
type wireResponse struct {
	Address string     `json:"address,omitempty"`
	RawTx   string     `json:"raw_tx,omitempty"`
	TxHash  string     `json:"tx_hash,omitempty"`
	From    string     `json:"from,omitempty"`
	Nonce   uint64     `json:"nonce,omitempty"`
	Error   *wireError `json:"error,omitempty"`
}

// wireError classifies a failure so the caller can rebuild the sentinel it
// would have seen in-process. Without it a refusal and a transient RPC error
// would be indistinguishable across the container boundary, and the caller
// would retry the one thing it must never retry.
type wireError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Operations.
const (
	opSign      = "sign"
	opHotWallet = "hot_wallet"
)

const (
	errRefused       = "refused"
	errAlreadySigned = "already_signed"
	errUnsupported   = "unsupported"
	errInternal      = "internal"
)

// Client asks a signer running elsewhere to sign (docs/plan-v1.0.md §5.1).
type Client struct {
	nc      *nats.Conn
	subject string
	timeout time.Duration

	mu  sync.Mutex
	hot common.Address
}

var _ signer.Signer = (*Client)(nil)

// ClientConfig configures the remote signer.
type ClientConfig struct {
	Tenant        string
	SubjectPrefix string
	Timeout       time.Duration
}

// NewClient builds a remote signer client.
func NewClient(nc *nats.Conn, cfg ClientConfig) (*Client, error) {
	if nc == nil {
		return nil, errors.New("signer: a NATS connection is required")
	}
	if cfg.Tenant == "" {
		cfg.Tenant = "default"
	}
	if cfg.SubjectPrefix == "" {
		cfg.SubjectPrefix = DefaultSubjectPrefix
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	return &Client{nc: nc, subject: cfg.SubjectPrefix + "." + cfg.Tenant, timeout: cfg.Timeout}, nil
}

// Subject is the subject requests go to.
func (c *Client) Subject() string { return c.subject }

// HotWallet implements Signer by asking the signer role, once.
//
// The address is not configured on this side on purpose. A config value that
// disagreed with the key actually signing would track nonces for one address
// while another sent the transactions, and nothing would notice until the
// chain rejected everything.
func (c *Client) HotWallet(ctx context.Context) (common.Address, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hot != (common.Address{}) {
		return c.hot, nil
	}
	resp, err := c.roundTrip(ctx, wireRequest{Op: opHotWallet})
	if err != nil {
		return common.Address{}, err
	}
	if !common.IsHexAddress(resp.Address) {
		return common.Address{}, fmt.Errorf("signer: %q is not an address", resp.Address)
	}
	c.hot = common.HexToAddress(resp.Address)
	return c.hot, nil
}

// Sign implements Signer by asking the signer role.
func (c *Client) Sign(ctx context.Context, req signer.Request) (signer.Result, error) {
	if err := req.Validate(); err != nil {
		return signer.Result{}, err
	}
	resp, err := c.roundTrip(ctx, wireRequest{
		Op: opSign, Kind: req.Kind, RefID: req.RefID, Attempt: req.Attempt, ChainID: req.ChainID,
		To: strings.ToLower(req.To.Hex()), Asset: req.Asset, Value: req.Value.String(),
		Nonce: req.Nonce, Gas: req.Gas, TipCap: req.TipCap.String(), FeeCap: req.FeeCap.String(),
	})
	if err != nil {
		return signer.Result{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(resp.RawTx)
	if err != nil {
		return signer.Result{}, fmt.Errorf("signer: decode signed tx: %w", err)
	}
	return signer.Result{RawTx: raw, TxHash: resp.TxHash, From: resp.From, Nonce: resp.Nonce}, nil
}

// roundTrip sends one request and rebuilds the failure the caller would have
// seen in-process.
func (c *Client) roundTrip(ctx context.Context, req wireRequest) (wireResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return wireResponse{}, fmt.Errorf("signer: marshal request: %w", err)
	}
	msg := nats.NewMsg(c.subject)
	msg.Data = body
	if id := telemetry.CorrelationID(ctx); id != "" {
		msg.Header.Set("Correlation-Id", id)
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	reply, err := c.nc.RequestMsgWithContext(rctx, msg)
	if err != nil {
		// A signer that cannot be reached has not signed anything, so the
		// caller may safely try again later with the same attempt number.
		return wireResponse{}, fmt.Errorf("signer: request: %w", err)
	}
	var resp wireResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return wireResponse{}, fmt.Errorf("signer: decode reply: %w", err)
	}
	if resp.Error != nil {
		return wireResponse{}, rebuild(resp.Error)
	}
	return resp, nil
}

func rebuild(e *wireError) error {
	switch e.Kind {
	case errRefused:
		return fmt.Errorf("%w: %s", signer.ErrRefused, e.Message)
	case errAlreadySigned:
		return fmt.Errorf("%w: %s", signer.ErrAlreadySigned, e.Message)
	case errUnsupported:
		return fmt.Errorf("%w: %s", signer.ErrUnsupported, e.Message)
	default:
		return fmt.Errorf("signer: %s", e.Message)
	}
}

// Server answers signing requests in the signer role.
type Server struct {
	sub *nats.Subscription
	s   signer.Signer
	log *slog.Logger
}

// Serve subscribes the signer to its subject.
func Serve(nc *nats.Conn, s signer.Signer, tenant, prefix string, log *slog.Logger) (*Server, error) {
	if nc == nil {
		return nil, errors.New("signer: a NATS connection is required")
	}
	if tenant == "" {
		tenant = "default"
	}
	if prefix == "" {
		prefix = DefaultSubjectPrefix
	}
	srv := &Server{s: s, log: log}
	sub, err := nc.QueueSubscribe(prefix+"."+tenant, queueGroup, srv.handle)
	if err != nil {
		return nil, fmt.Errorf("signer: subscribe: %w", err)
	}
	srv.sub = sub
	return srv, nil
}

// Subject is what the server listens on.
func (s *Server) Subject() string { return s.sub.Subject }

// Close unsubscribes.
func (s *Server) Close() error {
	if s.sub == nil {
		return nil
	}
	return s.sub.Unsubscribe()
}

func (s *Server) handle(msg *nats.Msg) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if id := msg.Header.Get("Correlation-Id"); id != "" {
		ctx = telemetry.WithCorrelationID(ctx, id)
	}
	var req wireRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		s.reply(msg, wireResponse{Error: &wireError{Kind: errRefused, Message: "malformed request"}})
		return
	}
	if req.Op == opHotWallet {
		addr, err := s.s.HotWallet(ctx)
		if err != nil {
			s.reply(msg, wireResponse{Error: &wireError{Kind: classify(err), Message: err.Error()}})
			return
		}
		s.reply(msg, wireResponse{Address: strings.ToLower(addr.Hex())})
		return
	}
	value, err := money.ParseAmount(req.Value)
	if err != nil {
		s.reply(msg, wireResponse{Error: &wireError{Kind: errRefused, Message: "value: " + err.Error()}})
		return
	}
	tip, tipOK := new(big.Int).SetString(req.TipCap, 10)
	feeCap, feeOK := new(big.Int).SetString(req.FeeCap, 10)
	if !tipOK || !feeOK {
		s.reply(msg, wireResponse{Error: &wireError{Kind: errRefused, Message: "fees are not decimal integers"}})
		return
	}
	res, err := s.s.Sign(ctx, signer.Request{
		Kind: req.Kind, RefID: req.RefID, Attempt: req.Attempt, ChainID: req.ChainID,
		To: common.HexToAddress(req.To), Asset: req.Asset, Value: value,
		Nonce: req.Nonce, Gas: req.Gas, TipCap: tip, FeeCap: feeCap,
	})
	if err != nil {
		s.log.Warn("signing refused",
			slog.String("kind", string(req.Kind)), slog.String("ref_id", req.RefID),
			slog.String("err", err.Error()))
		s.reply(msg, wireResponse{Error: &wireError{Kind: classify(err), Message: err.Error()}})
		return
	}
	s.reply(msg, wireResponse{
		RawTx: base64.StdEncoding.EncodeToString(res.RawTx), TxHash: res.TxHash,
		From: res.From, Nonce: res.Nonce,
	})
}

func classify(err error) string {
	switch {
	case errors.Is(err, signer.ErrRefused):
		return errRefused
	case errors.Is(err, signer.ErrAlreadySigned):
		return errAlreadySigned
	case errors.Is(err, signer.ErrUnsupported):
		return errUnsupported
	default:
		return errInternal
	}
}

func (s *Server) reply(msg *nats.Msg, resp wireResponse) {
	body, err := json.Marshal(resp)
	if err != nil {
		s.log.Error("signer: marshal reply", slog.String("err", err.Error()))
		return
	}
	if err := msg.Respond(body); err != nil {
		s.log.Error("signer: respond", slog.String("err", err.Error()))
	}
}

package cmdbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// DefaultTimeout bounds one command round trip. A command that has not been
// answered within it is reported as unavailable (HTTP 503), which is honest:
// the engine may still apply it, and the caller retries with the same
// client_order_id.
const DefaultTimeout = 5 * time.Second

// TokenSource mints the internal token for the caller of ctx. The api role
// passes a closure over its Ed25519 signer; nil disables the header (dev,
// and depth requests, which are public).
type TokenSource func(ctx context.Context) (string, error)

// Config configures the client side of the bus.
type Config struct {
	Tenant        string
	SubjectPrefix string // default DefaultSubjectPrefix
	Timeout       time.Duration
	Token         TokenSource
	Metrics       *Metrics
}

func (c Config) withDefaults() Config {
	if c.Tenant == "" {
		c.Tenant = "default"
	}
	if c.SubjectPrefix == "" {
		c.SubjectPrefix = DefaultSubjectPrefix
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	return c
}

// Client is the api role's trading.CommandBus over NATS.
type Client struct {
	nc  *nats.Conn
	cfg Config
}

var _ trading.CommandBus = (*Client)(nil)

// NewClient builds the client bus.
func NewClient(nc *nats.Conn, cfg Config) (*Client, error) {
	if nc == nil {
		return nil, errors.New("cmdbus: nats connection required")
	}
	return &Client{nc: nc, cfg: cfg.withDefaults()}, nil
}

// Subject returns the subject a market's commands travel on.
func (c *Client) Subject(market string) string {
	return c.cfg.SubjectPrefix + "." + c.cfg.Tenant + "." + market
}

// PlaceOrder implements trading.CommandBus.
func (c *Client) PlaceOrder(ctx context.Context, req trading.PlaceOrderRequest) (trading.PlaceOrderResult, error) {
	resp, err := c.do(ctx, req.MarketSymbol, Request{Op: OpPlace, Market: req.MarketSymbol, Place: &req})
	if err != nil {
		return trading.PlaceOrderResult{}, err
	}
	if resp.Result == nil {
		return trading.PlaceOrderResult{}, fmt.Errorf("cmdbus: engine returned no result for %s", OpPlace)
	}
	return *resp.Result, nil
}

// CancelOrder implements trading.CommandBus.
func (c *Client) CancelOrder(ctx context.Context, market string, req trading.CancelRequest) (trading.Order, error) {
	resp, err := c.do(ctx, market, Request{Op: OpCancel, Market: market, Cancel: &req})
	if err != nil {
		return trading.Order{}, err
	}
	if resp.Order == nil {
		return trading.Order{}, fmt.Errorf("cmdbus: engine returned no order for %s", OpCancel)
	}
	return *resp.Order, nil
}

// Depth implements trading.CommandBus.
func (c *Client) Depth(ctx context.Context, market string, n int) (matching.Depth, error) {
	resp, err := c.do(ctx, market, Request{Op: OpDepth, Market: market, Depth: n})
	if err != nil {
		return matching.Depth{}, err
	}
	if resp.Book == nil {
		return matching.Depth{}, fmt.Errorf("cmdbus: engine returned no book for %s", OpDepth)
	}
	return *resp.Book, nil
}

// do performs one request-reply round trip.
func (c *Client) do(ctx context.Context, market string, r Request) (Response, error) {
	start := time.Now()
	resp, err := c.request(ctx, market, r)
	c.cfg.Metrics.observeClient(string(r.Op), status(err), time.Since(start))
	return resp, err
}

func (c *Client) request(ctx context.Context, market string, r Request) (Response, error) {
	if market == "" {
		return Response{}, fmt.Errorf("%w: market required", trading.ErrInvalidRequest)
	}
	if r.Op != OpDepth && c.cfg.Token != nil {
		tok, err := c.cfg.Token(ctx)
		if err != nil {
			return Response{}, fmt.Errorf("cmdbus: mint internal token: %w", err)
		}
		r.Token = tok
	}
	body, err := json.Marshal(r)
	if err != nil {
		return Response{}, fmt.Errorf("cmdbus: encode request: %w", err)
	}
	msg := nats.NewMsg(c.Subject(market))
	msg.Data = body
	if cid := telemetry.CorrelationID(ctx); cid != "" {
		msg.Header.Set(HeaderCorrelationID, cid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	reply, err := c.nc.RequestMsgWithContext(ctx, msg)
	if err != nil {
		// No engine is subscribed, it did not answer in time, or the caller
		// gave up: all of them mean "cannot reach the engine right now".
		return Response{}, fmt.Errorf("%w: %v", trading.ErrEngineUnavailable, err)
	}
	var resp Response
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return Response{}, fmt.Errorf("cmdbus: decode response: %w", err)
	}
	if resp.Error != nil {
		return resp, resp.Error.err()
	}
	return resp, nil
}

// status labels a metric sample.
func status(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, trading.ErrEngineUnavailable):
		return string(KindUnavailable)
	case errors.Is(err, trading.ErrInvalidRequest):
		return string(KindInvalidRequest)
	case errors.Is(err, trading.ErrMarketNotFound):
		return string(KindMarketNotFound)
	case errors.Is(err, trading.ErrOrderNotFound):
		return string(KindOrderNotFound)
	case errors.Is(err, trading.ErrClientOrderIDMismatch):
		return string(KindClientOrderIDMismatch)
	case errors.Is(err, ErrRejected):
		return string(KindRejected)
	default:
		return string(KindInternal)
	}
}

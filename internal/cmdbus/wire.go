// Package cmdbus carries trading commands from the api role to the engine
// role over NATS request-reply (docs/plan-v1.0.md §5.2 step 3, §14).
//
// It lives outside internal/trading on purpose: trading owns the
// CommandBus interface and must not depend on NATS or on internal/auth.
// Here the two meet — the wire format, the internal JWT the api mints and
// the engine verifies, and the subject layout cmd.trading.<tenant>.<market>.
//
// Both ends run the same binary, so commands and results are the trading
// types themselves rather than a hand-written DTO layer. That couples the
// wire format to the Go types: adding a field is safe in either direction
// (JSON ignores unknown fields and zeroes missing ones), renaming or
// removing one is not, so a mixed-version rollout must go through a release
// that only adds.
package cmdbus

import (
	"errors"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// HeaderCorrelationID carries the request's correlation id to the engine's
// logs (docs/plan-v1.0.md §15).
const HeaderCorrelationID = "Correlation-Id"

// DefaultSubjectPrefix is the first tokens of every command subject.
const DefaultSubjectPrefix = "cmd.trading"

// queueGroup makes a second engine instance share the subscription instead
// of answering the same command twice. Only one instance can hold the
// advisory lock anyway; this keeps a starting replica harmless.
const queueGroup = "engine"

// Op is the command a request carries.
type Op string

// Commands.
const (
	OpPlace  Op = "place"
	OpCancel Op = "cancel"
	OpDepth  Op = "depth"
)

// Request is one command on the wire. Token is the internal JWT
// (aud=internal) for the commands that move money; depth is public data and
// carries none.
type Request struct {
	Op     Op                         `json:"op"`
	Market string                     `json:"market"`
	Token  string                     `json:"token,omitempty"`
	Place  *trading.PlaceOrderRequest `json:"place,omitempty"`
	Cancel *trading.CancelRequest     `json:"cancel,omitempty"`
	Depth  int                        `json:"depth,omitempty"`
}

// Response is the engine's answer. Exactly one of Result, Order, Book or
// Error is set.
type Response struct {
	Result *trading.PlaceOrderResult `json:"result,omitempty"`
	Order  *trading.Order            `json:"order,omitempty"`
	Book   *matching.Depth           `json:"book,omitempty"`
	Error  *Error                    `json:"error,omitempty"`
}

// Kind classifies a failure so the client can rebuild the sentinel error the
// caller would have seen in-process. Without it every cross-container
// failure would reach the API as a 500 and the 404 / 422 / 503 mapping of
// internal/api would silently stop working in split deployments.
type Kind string

// Failure kinds.
const (
	KindInvalidRequest        Kind = "invalid_request"
	KindMarketNotFound        Kind = "market_not_found"
	KindOrderNotFound         Kind = "order_not_found"
	KindClientOrderIDMismatch Kind = "client_order_id_mismatch"
	KindUnavailable           Kind = "unavailable"
	KindRejected              Kind = "rejected"
	KindInternal              Kind = "internal"
)

// ErrRejected is the engine refusing a command's credential: a missing,
// expired or wrong-audience token, a tenant or account that does not match
// the token, or a token without the trade scope. The api role mints those
// tokens itself, so this is a misconfiguration, not a user error.
var ErrRejected = errors.New("cmdbus: command rejected by the engine")

// kinds maps every wire kind to the error the client rebuilds. Order
// matters for errorOf: the first sentinel that matches wins.
var kinds = []struct {
	kind Kind
	err  error
}{
	{KindInvalidRequest, trading.ErrInvalidRequest},
	{KindMarketNotFound, trading.ErrMarketNotFound},
	{KindOrderNotFound, trading.ErrOrderNotFound},
	{KindClientOrderIDMismatch, trading.ErrClientOrderIDMismatch},
	{KindUnavailable, trading.ErrEngineUnavailable},
	{KindRejected, ErrRejected},
}

// Error is a failure on the wire.
type Error struct {
	Kind    Kind   `json:"kind"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Kind, e.Message) }

// errorOf classifies a server-side error for the wire, keeping the message
// verbatim so the api logs read the same as they would in-process.
func errorOf(err error) *Error {
	for _, k := range kinds {
		if errors.Is(err, k.err) {
			return &Error{Kind: k.kind, Message: err.Error()}
		}
	}
	return &Error{Kind: KindInternal, Message: err.Error()}
}

// err rebuilds a local error that unwraps to the original sentinel and
// prints the original message.
func (e *Error) err() error {
	for _, k := range kinds {
		if e.Kind == k.kind {
			return remoteError{sentinel: k.err, msg: e.Message}
		}
	}
	return fmt.Errorf("cmdbus: engine: %s", e.Message)
}

// remoteError carries a message produced by the engine while unwrapping to
// the sentinel that internal/api maps to an HTTP status.
type remoteError struct {
	sentinel error
	msg      string
}

func (e remoteError) Error() string { return e.msg }
func (e remoteError) Unwrap() error { return e.sentinel }

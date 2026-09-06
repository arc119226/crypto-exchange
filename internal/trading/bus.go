package trading

import (
	"context"

	"github.com/arc119226/crypto-exchange/internal/matching"
)

// CommandBus carries commands from the API to the market runners. In one
// process (role=all) the Engine itself is the bus; a split deployment uses
// NATS request-reply (docs/plan-v1.0.md §5.2 step 3), implemented in a
// later step of Phase 3.
type CommandBus interface {
	PlaceOrder(ctx context.Context, req PlaceOrderRequest) (PlaceOrderResult, error)
	CancelOrder(ctx context.Context, market string, req CancelRequest) (Order, error)
	Depth(ctx context.Context, market string, n int) (matching.Depth, error)
}

var _ CommandBus = (*Engine)(nil)

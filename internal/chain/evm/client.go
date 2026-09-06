package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// TransferTopic is keccak256("Transfer(address,address,uint256)"), topic 0 of
// every ERC-20 transfer. The scanner filters on it and then matches
// topics[2] — the recipient — in memory, because a public provider will not
// accept thousands of addresses in a topic array (docs/plan-v1.0.md §6.4.1).
var TransferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

// ErrNotFound is returned when a block or receipt does not exist yet.
var ErrNotFound = errors.New("evm: not found")

// Client is the read side of an EVM node. Phase 4a only reads; signing and
// broadcasting arrive with withdrawals in 4b.
type Client struct {
	rpc *ethclient.Client
	url string
}

// Dial connects to an EVM JSON-RPC endpoint.
func Dial(ctx context.Context, url string) (*Client, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("evm: ETH_RPC_URL is empty")
	}
	c, err := ethclient.DialContext(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("evm: dial: %w", err)
	}
	return &Client{rpc: c, url: url}, nil
}

// Close releases the connection.
func (c *Client) Close() {
	if c.rpc != nil {
		c.rpc.Close()
	}
}

// ChainID reports the chain the node believes it is on. The scanner compares
// it with ETH_CHAIN_ID before touching anything.
func (c *Client) ChainID(ctx context.Context) (int64, error) {
	id, err := c.rpc.ChainID(ctx)
	if err != nil {
		return 0, fmt.Errorf("evm: chain id: %w", err)
	}
	if !id.IsInt64() {
		return 0, fmt.Errorf("evm: chain id %s does not fit int64", id)
	}
	return id.Int64(), nil
}

// Head returns the number of the latest block.
func (c *Client) Head(ctx context.Context) (uint64, error) {
	n, err := c.rpc.BlockNumber(ctx)
	if err != nil {
		return 0, fmt.Errorf("evm: head: %w", err)
	}
	return n, nil
}

// GenesisHash identifies the chain across restarts. A node whose genesis
// changed is a different chain, however familiar its chain id looks — which is
// exactly what happens when an anvil state volume is wiped.
func (c *Client) GenesisHash(ctx context.Context) (string, error) {
	h, err := c.rpc.HeaderByNumber(ctx, big.NewInt(0))
	if err != nil {
		return "", fmt.Errorf("evm: genesis: %w", err)
	}
	return strings.ToLower(h.Hash().Hex()), nil
}

// Block is one block with the fields the scanner needs. Transactions are
// included because the native-deposit path matches on tx.To.
type Block struct {
	Number     uint64
	Hash       string
	ParentHash string
	Txs        types.Transactions
}

// BlockByNumber fetches a block with its transactions.
func (c *Client) BlockByNumber(ctx context.Context, number uint64) (Block, error) {
	b, err := c.rpc.BlockByNumber(ctx, new(big.Int).SetUint64(number))
	switch {
	case errors.Is(err, ethereum.NotFound):
		return Block{}, fmt.Errorf("%w: block %d", ErrNotFound, number)
	case err != nil:
		return Block{}, fmt.Errorf("evm: block %d: %w", number, err)
	}
	return Block{
		Number:     b.NumberU64(),
		Hash:       strings.ToLower(b.Hash().Hex()),
		ParentHash: strings.ToLower(b.ParentHash().Hex()),
		Txs:        b.Transactions(),
	}, nil
}

// TransferLogs returns the ERC-20 Transfer logs emitted by contracts in a
// block range. Recipients are matched by the caller, not by the node.
func (c *Client) TransferLogs(ctx context.Context, from, to uint64, contracts []common.Address) ([]types.Log, error) {
	if len(contracts) == 0 {
		return nil, nil
	}
	logs, err := c.rpc.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: contracts,
		Topics:    [][]common.Hash{{TransferTopic}},
	})
	if err != nil {
		return nil, fmt.Errorf("evm: transfer logs %d-%d: %w", from, to, err)
	}
	return logs, nil
}

// Receipt fetches a transaction receipt (4b uses it to follow withdrawals;
// 4a uses it only in tests).
func (c *Client) Receipt(ctx context.Context, txHash string) (*types.Receipt, error) {
	r, err := c.rpc.TransactionReceipt(ctx, common.HexToHash(txHash))
	switch {
	case errors.Is(err, ethereum.NotFound):
		return nil, fmt.Errorf("%w: receipt %s", ErrNotFound, txHash)
	case err != nil:
		return nil, fmt.Errorf("evm: receipt %s: %w", txHash, err)
	}
	return r, nil
}

// LogValue keeps the endpoint out of logs at full fidelity: a hosted RPC URL
// often carries an API key in its path.
func (c *Client) LogValue() string { return redactURL(c.url) }

func redactURL(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		if j := strings.Index(raw[i+3:], "/"); j >= 0 {
			return raw[:i+3+j] + "/[redacted]"
		}
	}
	return raw
}

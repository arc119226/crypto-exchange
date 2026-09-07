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

// AnchorHash identifies the chain across restarts: a node serving a different
// hash at the same height is a different chain, however familiar its chain id
// looks — which is exactly what happens when an anvil state volume is wiped.
//
// The caller picks the height. Genesis would be the obvious anchor and is what
// this used to read, but it is also the block a pruned node is least likely to
// serve, and public testnet endpoints load-balance across backends that have
// pruned different depths. Anchoring at the block the scanner starts from asks
// the same question at a depth the node still has.
func (c *Client) AnchorHash(ctx context.Context, block uint64) (string, error) {
	h, err := c.rpc.HeaderByNumber(ctx, new(big.Int).SetUint64(block))
	if err != nil {
		return "", fmt.Errorf("evm: anchor block %d: %w", block, err)
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

// Transfer is a decoded ERC-20 Transfer log.
type Transfer struct {
	Contract common.Address
	From     common.Address
	To       common.Address
	Value    *big.Int
	Removed  bool
}

// DecodeTransfer reads a standard ERC-20 Transfer log.
//
// It insists on the standard shape — topic0 plus two indexed addresses and a
// 32-byte value — and refuses anything else. Some tokens index the value, or
// pack all three arguments into data; guessing which layout a contract used
// would mean crediting an account from a number we are not sure of, so an
// unrecognised layout is an error the scanner can alert on instead.
//
// It does not decode by ABI because no contract call is involved: a binding
// would be a large generated dependency for one topic and one uint256. Sweeps
// in 4c call ERC-20 methods for real, and that is when abigen earns its place.
func DecodeTransfer(l types.Log) (Transfer, error) {
	switch {
	case len(l.Topics) != 3:
		return Transfer{}, fmt.Errorf("evm: transfer log has %d topics, want 3", len(l.Topics))
	case l.Topics[0] != TransferTopic:
		return Transfer{}, errors.New("evm: log is not a Transfer")
	case len(l.Data) != 32:
		return Transfer{}, fmt.Errorf("evm: transfer log has %d data bytes, want 32", len(l.Data))
	}
	return Transfer{
		Contract: l.Address,
		From:     common.BytesToAddress(l.Topics[1].Bytes()),
		To:       common.BytesToAddress(l.Topics[2].Bytes()),
		Value:    new(big.Int).SetBytes(l.Data),
		Removed:  l.Removed,
	}, nil
}

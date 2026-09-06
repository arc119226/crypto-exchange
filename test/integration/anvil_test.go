//go:build integration

package integration

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
)

// foundryTag is read from .env.example so the tests, compose and CI all run
// the same anvil. Pinning matters here beyond reproducibility: the reorg
// cheatcodes differ between versions (docs/plan-v1.0.md §12 risks).
func foundryTag(t *testing.T) string {
	t.Helper()
	env, err := os.ReadFile("../../.env.example")
	require.NoError(t, err)
	for line := range strings.SplitSeq(string(env), "\n") {
		if tag, ok := strings.CutPrefix(strings.TrimSpace(line), "FOUNDRY_TAG="); ok {
			return tag
		}
	}
	t.Fatal("FOUNDRY_TAG missing from .env.example")
	return ""
}

// anvilChainID matches compose and the seed fixtures.
const anvilChainID = 31337

// anvil is a development chain under the test's control. Blocks are only
// produced when the test asks for them (--no-mining), which is what makes
// "N confirmations" and "a reorg happened here" assertable rather than timed.
type anvil struct {
	URL string
	rpc *rpc.Client
}

// startAnvil returns a chain with no blocks being mined.
//
// TEST_ETH_RPC_URL=http://host:port uses a running node instead (start one
// with `anvil --no-mining`); tests that need a clean chain say so.
func startAnvil(t *testing.T) *anvil {
	t.Helper()
	ctx := context.Background()

	url := os.Getenv("TEST_ETH_RPC_URL")
	if url == "" {
		ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			Started: true,
			ContainerRequest: testcontainers.ContainerRequest{
				Image: "ghcr.io/foundry-rs/foundry:" + foundryTag(t),
				// the image's entrypoint is `/bin/sh -c` and it runs as a user
				// that cannot bind privileged paths; compose does the same
				Entrypoint:   []string{"anvil"},
				Cmd:          []string{"--host=0.0.0.0", "--chain-id=31337", "--accounts=10", "--balance=10000", "--no-mining"},
				ExposedPorts: []string{"8545/tcp"},
				WaitingFor:   wait.ForListeningPort("8545/tcp"),
			},
		})
		if err != nil {
			if os.Getenv("CI") == "" {
				t.Skipf("docker not available, skipping anvil integration test: %v", err)
			}
			t.Fatalf("start anvil: %v", err)
		}
		t.Cleanup(func() {
			if err := testcontainers.TerminateContainer(ctr); err != nil {
				t.Logf("terminate anvil: %v", err)
			}
		})
		host, err := ctr.Host(ctx)
		require.NoError(t, err)
		port, err := ctr.MappedPort(ctx, "8545/tcp")
		require.NoError(t, err)
		url = fmt.Sprintf("http://%s:%s", host, port.Port())
	}

	c, err := rpc.DialContext(ctx, url)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	a := &anvil{URL: url, rpc: c}

	// anvil starts at block 0 with nothing mined; prove the connection works
	// before a test builds on it.
	require.NoError(t, a.call(t, nil, "eth_blockNumber"))
	return a
}

// client opens the production read client against this chain.
func (a *anvil) client(t *testing.T) *evm.Client {
	t.Helper()
	c, err := evm.Dial(context.Background(), a.URL)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func (a *anvil) call(t *testing.T, out any, method string, args ...any) error {
	t.Helper()
	return a.rpc.CallContext(context.Background(), out, method, args...)
}

// Mine produces n blocks. Every test that wants a confirmation says so here,
// so nothing depends on wall-clock timing.
func (a *anvil) Mine(t *testing.T, n int) {
	t.Helper()
	require.NoError(t, a.call(t, nil, "anvil_mine", hexUint(uint64(n)))) //nolint:gosec // small test count
}

// Accounts returns anvil's pre-funded development accounts.
func (a *anvil) Accounts(t *testing.T) []common.Address {
	t.Helper()
	var out []common.Address
	require.NoError(t, a.call(t, &out, "eth_accounts"))
	require.NotEmpty(t, out)
	return out
}

// SendETH queues a native transfer from a pre-funded account. anvil's accounts
// are unlocked, so the test never handles a key: eth_sendTransaction is enough
// and keeps signing out of the deposit tests entirely.
//
// The transaction sits in the mempool until Mine is called.
func (a *anvil) SendETH(t *testing.T, from, to common.Address, wei *big.Int) string {
	t.Helper()
	var hash string
	require.NoError(t, a.call(t, &hash, "eth_sendTransaction", map[string]any{
		"from": from.Hex(), "to": to.Hex(), "value": hexBig(wei),
	}))
	return strings.ToLower(hash)
}

// SendERC20 calls transfer(to, amount) on a token contract.
func (a *anvil) SendERC20(t *testing.T, token, from, to common.Address, amount *big.Int) string {
	t.Helper()
	var hash string
	require.NoError(t, a.call(t, &hash, "eth_sendTransaction", map[string]any{
		"from": from.Hex(), "to": token.Hex(), "data": erc20TransferData(to, amount),
	}))
	return strings.ToLower(hash)
}

// Balance is what the node says an address holds, in wei. It is the only
// check that can tell a valid signature from a plausible one.
func (a *anvil) Balance(t *testing.T, addr common.Address) *big.Int {
	t.Helper()
	var hex string
	require.NoError(t, a.call(t, &hex, "eth_getBalance", addr.Hex(), "latest"))
	out, ok := new(big.Int).SetString(strings.TrimPrefix(hex, "0x"), 16)
	require.True(t, ok, "undecodable balance %q", hex)
	return out
}

// Snapshot returns an id that Revert restores the chain to.
func (a *anvil) Snapshot(t *testing.T) string {
	t.Helper()
	var id string
	require.NoError(t, a.call(t, &id, "evm_snapshot"))
	return id
}

// Revert rolls the chain back to a snapshot, discarding the blocks after it.
// This is how a reorg is staged: revert, then mine different blocks, and the
// scanner meets a chain whose history changed under it.
func (a *anvil) Revert(t *testing.T, id string) {
	t.Helper()
	var ok bool
	require.NoError(t, a.call(t, &ok, "evm_revert", id))
	require.True(t, ok, "evm_revert %s", id)
}

func hexUint(v uint64) string { return fmt.Sprintf("0x%x", v) }

func hexBig(v *big.Int) string { return "0x" + v.Text(16) }

// erc20TransferSelector is the first four bytes of
// keccak256("transfer(address,uint256)").
var erc20TransferSelector = func() [4]byte {
	var out [4]byte
	copy(out[:], crypto.Keccak256([]byte("transfer(address,uint256)"))[:4])
	return out
}()

// erc20TransferData packs transfer(address,uint256) by hand. The selector is
// asserted against the published 0xa9059cbb in TestERC20TransferSelector, so
// this cannot drift into calling some other method.
func erc20TransferData(to common.Address, amount *big.Int) string {
	data := make([]byte, 0, 4+32+32)
	data = append(data, erc20TransferSelector[:]...)
	data = append(data, common.LeftPadBytes(to.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(amount.Bytes(), 32)...)
	return "0x" + common.Bytes2Hex(data)
}

// TestERC20TransferSelector pins the hand-packed calldata against the
// published selector. Without it a typo in the signature string would send a
// call to a method that does not exist, the token would revert, and the test
// would look like a scanner bug.
func TestERC20TransferSelector(t *testing.T) {
	require.Equal(t, "a9059cbb", common.Bytes2Hex(erc20TransferSelector[:]))
}

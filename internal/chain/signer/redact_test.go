package signer

import (
	"bytes"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// A signed transaction must never reach a log. docs/plan-v1.0.md §12 asks for
// this as a Phase 4 DoD, and the e2e leak scan cannot cover it: that scan
// compares against the known development secrets, and raw transaction bytes
// are different every run.
//
// The fields around it are the ones an operator actually wants -- the hash is
// how you find the transaction on a block explorer -- so this redacts the
// bytes rather than the whole value.
func TestResultNeverLogsTheSignedTransaction(t *testing.T) {
	res := Result{
		RawTx:  []byte{0x02, 0xf8, 0x6c, 0xde, 0xad, 0xbe, 0xef},
		TxHash: "0x22618e1fe7643b57e7d3692451cbb669e7fd4802974b0aff9fe97db58bcae1d1",
		From:   "0x1234567890abcdef1234567890abcdef12345678",
		Nonce:  7,
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("signed", slog.Any("result", res))
	out := buf.String()

	assert.NotContains(t, out, "02f86cdeadbeef", "the signed transaction is in the log as hex")
	assert.NotContains(t, out, "AviM3q2+7w", "or as base64")
	assert.NotContains(t, out, "[2 248 108", "or as Go's default slice formatting, which is what you get without a LogValuer")
	assert.Contains(t, out, telemetry.Redacted, "and says so, rather than dropping the field silently")

	// What is left has to still be worth logging.
	assert.Contains(t, out, res.TxHash, "the hash is how anyone finds this transaction again")
	assert.Contains(t, out, res.From)
	assert.Contains(t, out, "nonce=7")
}

// The request carries no secret today -- every field of it is already on chain
// or in the ledger. It gets a LogValue anyway so that adding a field which
// does carry one is a deliberate edit here rather than a silent leak there.
func TestRequestLogsWhatItIsWithoutSurprises(t *testing.T) {
	req := Request{
		Kind: KindWithdrawal, RefID: "w-1", Attempt: 0, ChainID: 11155111,
		To:    common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"),
		Asset: "ETH", Value: money.MustParse("0.003"), Nonce: 7, Gas: 21000,
		TipCap: big.NewInt(1), FeeCap: big.NewInt(2),
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("signing", slog.Any("request", req))
	out := buf.String()

	require.Contains(t, out, "withdrawal")
	require.Contains(t, out, "w-1")
	assert.Contains(t, out, "0.003")
	assert.Equal(t, 1, strings.Count(out, "request.kind="), "one group, not a repeated struct dump")
}

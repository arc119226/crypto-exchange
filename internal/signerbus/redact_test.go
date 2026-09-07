package signerbus

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// The signed transaction crosses the NATS boundary base64-encoded, so this
// struct is the second place in the system holding those bytes. The first,
// signer.Result, is redacted; this one has to be too, or the split deployment
// is the one that leaks -- which is the deployment that has a separate signer
// role precisely because it is taking key handling seriously.
func TestWireResponseNeverLogsTheSignedTransaction(t *testing.T) {
	raw := []byte{0x02, 0xf8, 0x6c, 0xde, 0xad, 0xbe, 0xef}
	resp := wireResponse{
		RawTx:  base64.StdEncoding.EncodeToString(raw),
		TxHash: "0x22618e1fe7643b57e7d3692451cbb669e7fd4802974b0aff9fe97db58bcae1d1",
		From:   "0x1234567890abcdef1234567890abcdef12345678",
		Nonce:  7,
	}

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("replying", slog.Any("response", resp))
	out := buf.String()

	assert.NotContains(t, out, resp.RawTx, "the base64 signed transaction is in the log")
	assert.Contains(t, out, telemetry.Redacted)
	assert.Contains(t, out, resp.TxHash, "the hash still has to be there to trace the reply")
}

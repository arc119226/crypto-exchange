// Package contract locks the event contract of docs/plan-v1.0.md §7: the
// JSON Schemas in api/events/v1 and the golden envelopes in
// test/golden/events. Customers integrate against those files, so a change
// in how the engine serialises an event has to show up here as a diff
// before it can reach anyone.
//
// It needs no infrastructure and runs in `make test`.
package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

const (
	schemaDir = "../../api/events/v1"
	goldenDir = "../golden/events"
)

// update rewrites the golden files: go test ./test/contract -update
var update = os.Getenv("UPDATE_GOLDEN") != ""

func amt(s string) money.Amount { return money.MustParse(s) }

// fixed inputs so the encoding, not the clock or a random id, is what the
// golden files capture.
var (
	fixedTime = time.Date(2026, 9, 5, 8, 15, 23, 412000000, time.UTC)
	fixedIDs  = map[string]string{
		"order.accepted":  "01J8Z2K3M4N5P6Q7R8S9T0V1W2",
		"order.updated":   "01J8Z2K3M4N5P6Q7R8S9T0V1W3",
		"order.filled":    "01J8Z2K3M4N5P6Q7R8S9T0V1W4",
		"order.cancelled": "01J8Z2K3M4N5P6Q7R8S9T0V1W5",
		"order.rejected":  "01J8Z2K3M4N5P6Q7R8S9T0V1W6",
		"trade.executed":  "01J8Z2K3M4N5P6Q7R8S9T0V1W7",
		"balance.updated": "01J8Z2K3M4N5P6Q7R8S9T0V1W8",
		"market.updated":  "01J8Z2K3M4N5P6Q7R8S9T0V1W9",
		// the deposit story of docs/plan-v1.0.md §6.4.1, one id per state
		"deposit.detected": "01J8Z2K3M4N5P6Q7R8S9T0V1X0",
		"deposit.credited": "01J8Z2K3M4N5P6Q7R8S9T0V1X1",
		"deposit.orphaned": "01J8Z2K3M4N5P6Q7R8S9T0V1X2",
		"deposit.dropped":  "01J8Z2K3M4N5P6Q7R8S9T0V1X3",
		"deposit.reversed": "01J8Z2K3M4N5P6Q7R8S9T0V1X4",
		// the withdrawal story of docs/plan-v1.0.md §6.4.2
		"withdrawal.requested":     "01J8Z2K3M4N5P6Q7R8S9T0V1X5",
		"withdrawal.state_changed": "01J8Z2K3M4N5P6Q7R8S9T0V1X6",
	}
)

func str(s string) *string { return &s }

func u64(v uint64) *uint64 { return &v }

func i64(v int64) *int64 { return &v }

// sample builds one envelope per event type from the payload types the
// producers actually marshal. The numbers are the worked example of
// docs/plan-v1.0.md §6.1.4 so the files read as one coherent story.
func sample(t *testing.T, eventType string) eventbus.Envelope {
	t.Helper()
	market, buyer, seller := "ETH-USDC", "acct-buyer", "acct-seller"
	env := eventbus.Envelope{
		EventID: fixedIDs[eventType], EventType: eventType, SchemaVersion: 1, TenantID: "default",
		OccurredAt: fixedTime, CorrelationID: "req_01J8Z2K2000000000000000000",
	}
	var payload any
	switch eventType {
	case trading.EventOrderAccepted:
		env.MarketID, env.AccountID, env.Seq, env.AccountSeq = str(market), str(buyer), u64(18234), i64(41)
		payload = trading.OrderAcceptedPayload{
			OrderID: "01J8Z2K3M4N5P6Q7R8S9T0V100", ClientOrderID: "b1", AccountID: buyer, Market: market,
			Side: "buy", Type: "limit", TimeInForce: "gtc", Price: ptr(amt("2000")), Qty: ptr(amt("1")), Seq: 18234,
		}
	case trading.EventOrderUpdated:
		env.MarketID, env.AccountID, env.Seq, env.AccountSeq = str(market), str(buyer), u64(18234), i64(42)
		payload = trading.OrderUpdatedPayload{
			OrderID: "01J8Z2K3M4N5P6Q7R8S9T0V100", AccountID: buyer, Market: market, Status: trading.StatusPartiallyFilled,
			FilledQty: amt("0.4"), FilledQuote: amt("796"), RemainingQty: amt("0.6"), Seq: 18234,
		}
	case trading.EventOrderFilled:
		env.MarketID, env.AccountID, env.Seq, env.AccountSeq = str(market), str(seller), u64(18234), i64(17)
		payload = trading.OrderFilledPayload{
			OrderID: "01J8Z2K3M4N5P6Q7R8S9T0V200", AccountID: seller, Market: market,
			FilledQty: amt("0.4"), FilledQuote: amt("796"), Seq: 18234,
		}
	case trading.EventOrderCancelled:
		env.MarketID, env.AccountID, env.Seq, env.AccountSeq = str(market), str(buyer), u64(18240), i64(43)
		payload = trading.OrderCancelledPayload{
			OrderID: "01J8Z2K3M4N5P6Q7R8S9T0V100", AccountID: buyer, Market: market, Reason: matching.CancelByUser,
			FilledQty: amt("0.4"), FilledQuote: amt("796"), RemainingQty: amt("0.6"), RemainingQuote: amt("0"),
			Released: amt("1200"), ReleasedAsset: "USDC", Seq: 18240,
		}
	case trading.EventOrderRejected:
		env.MarketID, env.AccountID = str(market), str(buyer)
		payload = trading.OrderRejectedPayload{
			OrderID: "01J8Z2K3M4N5P6Q7R8S9T0V300", ClientOrderID: "b-too-big", AccountID: buyer, Market: market,
			Reason: trading.RejectInsufficientBalance,
		}
	case trading.EventTradeExecuted:
		env.MarketID, env.Seq = str(market), u64(18234)
		payload = trading.TradeExecutedPayload{
			TradeID: "01J8Z2K3M4N5P6Q7R8S9T0V400", Market: market,
			MakerOrderID: "01J8Z2K3M4N5P6Q7R8S9T0V200", TakerOrderID: "01J8Z2K3M4N5P6Q7R8S9T0V100",
			MakerAccountID: seller, TakerAccountID: buyer, TakerSide: "buy",
			Price: amt("1990"), Qty: amt("0.4"), QuoteQty: amt("796"),
			MakerFee: amt("0.796"), MakerFeeAsset: "USDC", TakerFee: amt("0.0008"), TakerFeeAsset: "ETH", Seq: 18234,
		}
	case trading.EventBalanceUpdated:
		env.AccountID, env.AccountSeq = str(buyer), i64(44)
		payload = trading.BalanceUpdatedPayload{
			AccountID: buyer, Asset: "USDC", Available: amt("8004"), Hold: amt("1200"), AccountSeq: 44,
		}
	case deposit.EventDetected, deposit.EventCredited, deposit.EventOrphaned,
		deposit.EventDropped, deposit.EventReversed:
		// One payload shape for all five: what a consumer needs about a
		// deposit does not change with the way it moved, and the difference
		// lives in the event type. The numbers follow one 2 ETH deposit
		// through the machine.
		env.AccountID, env.AccountSeq = str(buyer), i64(51)
		status := map[string]string{
			deposit.EventDetected: "detected", deposit.EventCredited: "credited",
			deposit.EventOrphaned: "orphaned", deposit.EventDropped: "dropped",
			deposit.EventReversed: "reversed",
		}[eventType]
		confirmations := int32(2)
		if eventType == deposit.EventCredited || eventType == deposit.EventReversed {
			confirmations = 6
		}
		payload = deposit.Payload{
			DepositID: "01J8Z2K3M4N5P6Q7R8S9T0V600", AccountID: buyer, Asset: "ETH", Amount: amt("2"),
			Address:  "0x9858effd232b4033e47d90003d41ec34ecaeda94",
			TxHash:   "0x1f4b2c9d8e7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c",
			LogIndex: -1, BlockNumber: 18234, BlockHash: "0xa1b2c3d4e5f60718293a4b5c6d7e8f901a2b3c4d5e6f708192a3b4c5d6e7f801",
			Confirmations: confirmations, Status: status, Required: 6,
		}
	case withdrawal.EventRequested, withdrawal.EventStateChanged:
		// The same 0.5 ETH withdrawal at two points: recorded, then sent to
		// the review queue for being over the level-0 auto-approve ceiling.
		env.AccountID, env.AccountSeq = str(buyer), i64(52)
		p := withdrawal.Payload{
			WithdrawalID: "01J8Z2K3M4N5P6Q7R8S9T0V700", AccountID: buyer, Asset: "ETH", Amount: amt("0.5"),
			ToAddress: "0x70997970c51812dc3a010c7d01b50e0d17dc79c8", ChainID: 31337,
			Status: "requested",
		}
		if eventType == withdrawal.EventStateChanged {
			p.Status, p.PreviousStatus, p.Reason = "pending_review", "requested", "above_auto_approve_limit"
		}
		payload = p
	case registry.EventMarketUpdated:
		env.MarketID = str(market)
		payload = registry.MarketUpdatedPayload{
			MarketID: "01J8Z2K3M4N5P6Q7R8S9T0V500", Symbol: market, Status: registry.MarketHalted,
			ChangedFields: []string{"status"}, Version: 3, Reason: "incident 42",
		}
	default:
		t.Fatalf("no sample for %s", eventType)
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	env.Payload = body
	require.NoError(t, env.Validate(), "the sample itself must be a valid envelope")
	return env
}

func ptr[T any](v T) *T { return &v }

// allEventTypes is every type the producers declare.
func allEventTypes() []string {
	out := append([]string{}, trading.EventTypes()...)
	out = append(out, registry.EventTypes()...)
	out = append(out, deposit.EventTypes()...)
	return append(out, withdrawal.EventTypes()...)
}

// TestSchemaFilesMatchEventTypes is the drift guard: a new event type with
// no schema, or a schema for an event nobody publishes, fails here.
func TestSchemaFilesMatchEventTypes(t *testing.T) {
	entries, err := os.ReadDir(schemaDir)
	require.NoError(t, err)
	var files []string
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		if name == "envelope" {
			continue
		}
		files = append(files, name)
	}
	types := allEventTypes()
	slices.Sort(files)
	slices.Sort(types)
	assert.Equal(t, types, files,
		"api/events/v1 must hold exactly one schema per published event type (see trading.EventTypes / registry.EventTypes)")
}

// TestGoldenEnvelopes pins the serialisation and validates it against the
// published schemas.
func TestGoldenEnvelopes(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	envelopeSchema := mustCompile(t, compiler, "envelope")

	for _, eventType := range allEventTypes() {
		t.Run(eventType, func(t *testing.T) {
			env := sample(t, eventType)
			body, err := json.MarshalIndent(env, "", "  ")
			require.NoError(t, err)
			body = append(body, '\n')

			path := filepath.Join(goldenDir, eventType+".json")
			if update {
				require.NoError(t, os.WriteFile(path, body, 0o600))
			}
			want, err := os.ReadFile(path) //nolint:gosec // fixed test path
			require.NoError(t, err, "missing golden file; run with UPDATE_GOLDEN=1")
			assert.Equal(t, string(want), string(body),
				"serialisation changed: this is a contract change, not a refactor")

			// the golden file, not just the in-memory value, must satisfy
			// the schemas a customer would validate against
			var doc any
			require.NoError(t, json.Unmarshal(want, &doc))
			require.NoError(t, envelopeSchema.Validate(doc), "envelope schema")

			var envelope struct {
				Payload json.RawMessage `json:"payload"`
			}
			require.NoError(t, json.Unmarshal(want, &envelope))
			var payload any
			require.NoError(t, json.Unmarshal(envelope.Payload, &payload))
			require.NoError(t, mustCompile(t, compiler, eventType).Validate(payload), "payload schema")
		})
	}
}

// TestSubjectsAreRoutable checks that every event type maps onto a subject
// that one of the declared streams actually captures. A type with a dot in
// its second segment, or a domain nobody declared, would be published to a
// subject no stream stores.
func TestSubjectsAreRoutable(t *testing.T) {
	for _, eventType := range allEventTypes() {
		t.Run(eventType, func(t *testing.T) {
			env := sample(t, eventType)
			subject := env.Subject()
			assert.Equal(t, 6, len(strings.Split(subject, ".")), "subject %s must have six tokens", subject)
			assert.NotEmpty(t, eventbus.StreamFor(subject), "no stream captures %s", subject)
		})
	}
}

func mustCompile(t *testing.T, c *jsonschema.Compiler, name string) *jsonschema.Schema {
	t.Helper()
	path := filepath.Join(schemaDir, name+".json")
	f, err := os.Open(path) //nolint:gosec // fixed test path
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	doc, err := jsonschema.UnmarshalJSON(f)
	require.NoError(t, err)
	require.NoError(t, c.AddResource(path, doc))
	s, err := c.Compile(path)
	require.NoError(t, err)
	return s
}

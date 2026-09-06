package matching_test

import (
	"fmt"
	"testing"

	"github.com/arc119226/crypto-exchange/internal/matching"
)

// FuzzApply turns arbitrary bytes into a command sequence and checks that
// Apply never panics or errors on valid seqs, the book never crosses, and
// replay is deterministic. Run for the DoD with:
//
//	go test -run '^$' -fuzz FuzzApply -fuzztime 30s ./internal/matching
func FuzzApply(f *testing.F) {
	f.Add([]byte{0x01, 0x10, 0x20, 0x33, 0x05, 0x40, 0x11, 0x22, 0x45, 0xa0, 0x31, 0x33})
	f.Add([]byte("market orders sweep the book and self trades cancel"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		cfg := ethUSDC(matching.STPCancelNewest)
		if len(data) > 0 && data[0]&1 == 1 {
			cfg.SelfTradePolicy = matching.STPAllow
		}
		if len(data) > 1 && data[1]&1 == 1 {
			bps := int32(data[1]%200) + 1
			cfg.MaxSlippageBps = &bps
		}
		b, err := matching.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cmds := commandsFromBytes(data)
		var encoded []string
		for _, cmd := range cmds {
			evs, err := b.Apply(cmd)
			if err != nil {
				t.Fatalf("seq %d: %v", cmd.Seq, err)
			}
			if len(evs) == 0 {
				t.Fatalf("seq %d: no events", cmd.Seq)
			}
			if bid, ok := b.BestBid(); ok {
				if ask, ok := b.BestAsk(); ok && bid.Cmp(ask) >= 0 {
					t.Fatalf("crossed book after seq %d: bid %s ask %s", cmd.Seq, bid, ask)
				}
			}
			encoded = append(encoded, encodeAll(t, evs))
		}
		rb, _ := matching.New(cfg)
		for i, cmd := range cmds {
			evs, err := rb.Apply(cmd)
			if err != nil {
				t.Fatal(err)
			}
			if encodeAll(t, evs) != encoded[i] {
				t.Fatalf("replay diverged at command %d", i+1)
			}
		}
		if !rb.Snapshot().Equal(b.Snapshot()) {
			t.Fatal("replayed snapshot differs")
		}
	})
}

// commandsFromBytes decodes 5-byte records: kind, account/side, price, qty, extra.
func commandsFromBytes(data []byte) []matching.Command {
	var cmds []matching.Command
	seq := uint64(0)
	ids := []string{}
	for i := 0; i+5 <= len(data); i += 5 {
		seq++
		kind, acctSide, p, q, extra := data[i], data[i+1], data[i+2], data[i+3], data[i+4]
		acct := string(rune('A' + acctSide%3))
		side := matching.Buy
		if acctSide&0x80 != 0 {
			side = matching.Sell
		}
		id := fmt.Sprintf("f%d", seq)
		price := fmt.Sprintf("%d.%02d", 1990+int(p%21), int(extra%100))
		qty := fmt.Sprintf("0.%04d", 1+int(q)*39)
		var cmd matching.Command
		switch kind % 6 {
		case 0, 1:
			cmd = limit(seq, id, acct, side, price, qty)
			if extra&1 == 1 {
				cmd.New.TimeInForce = matching.IOC
			}
			ids = append(ids, id)
		case 2:
			cmd = marketBuy(seq, id, acct, fmt.Sprintf("%d", 5+int(q)*7))
		case 3:
			cmd = marketSell(seq, id, acct, qty)
		case 4:
			// deliberately malformed inputs must be rejected, never panic
			cmd = limit(seq, id, acct, side, price+"1", qty+"1")
		default:
			target := "missing"
			if len(ids) > 0 {
				target = ids[int(extra)%len(ids)]
			}
			cmd = cancel(seq, target)
		}
		cmds = append(cmds, cmd)
	}
	return cmds
}

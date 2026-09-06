package matching_test

import (
	"fmt"
	"testing"

	"github.com/arc119226/crypto-exchange/internal/matching"
)

// bigBook builds a book with n resting orders on each side spread over
// ~1,000 price levels per side, bids below 1990 and asks above 2010.
func bigBook(b *testing.B, n int) (*matching.Book, uint64) {
	cfg := ethUSDC(matching.STPCancelNewest)
	book, err := matching.New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	seq := uint64(0)
	for i := 0; i < n; i++ {
		seq++
		bidPrice := fmt.Sprintf("%d.%02d", 1980-(i%1000)/100, (i%1000)%100)
		if _, err := book.Apply(limit(seq, fmt.Sprintf("b%d", i), "M", matching.Buy, bidPrice, "0.1")); err != nil {
			b.Fatal(err)
		}
		seq++
		askPrice := fmt.Sprintf("%d.%02d", 2010+(i%1000)/100, (i%1000)%100)
		if _, err := book.Apply(limit(seq, fmt.Sprintf("a%d", i), "M", matching.Sell, askPrice, "0.1")); err != nil {
			b.Fatal(err)
		}
	}
	return book, seq
}

// BenchmarkApplyRestingLimit: inserting non-crossing limit orders into a
// 100k-order book (the common case: most orders rest).
func BenchmarkApplyRestingLimit(b *testing.B) {
	book, seq := bigBook(b, 50_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seq++
		price := fmt.Sprintf("%d.%02d", 1900+(i%50), i%100)
		if _, err := book.Apply(limit(seq, fmt.Sprintf("r%d", i), "T", matching.Buy, price, "0.0001")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkApplyTakerFill: a market sell that consumes one resting bid at
// the best level, alternating with a replacement bid so the book stays the
// same size. One iteration = one insert + one fill (two Apply calls).
func BenchmarkApplyTakerFill(b *testing.B) {
	book, seq := bigBook(b, 50_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		seq++
		if _, err := book.Apply(limit(seq, fmt.Sprintf("t%d", i), "T", matching.Buy, "1989.99", "0.0001")); err != nil {
			b.Fatal(err)
		}
		seq++
		if _, err := book.Apply(marketSell(seq, fmt.Sprintf("m%d", i), "S", "0.0001")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkApplyCancel: cancel + re-insert of an order deep in the book.
func BenchmarkApplyCancel(b *testing.B) {
	book, seq := bigBook(b, 50_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprintf("b%d", i%50_000)
		seq++
		if _, err := book.Apply(cancel(seq, id)); err != nil {
			b.Fatal(err)
		}
		seq++
		price := fmt.Sprintf("%d.%02d", 1980-((i%50_000)%1000)/100, ((i%50_000)%1000)%100)
		if _, err := book.Apply(limit(seq, id, "M", matching.Buy, price, "0.1")); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSnapshot(b *testing.B) {
	book, _ := bigBook(b, 50_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = book.Snapshot()
	}
}

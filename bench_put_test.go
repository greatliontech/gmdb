package gmdb

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
)

// BenchmarkPutRandom measures a Put with RANDOM, prefix-sharing keys into a
// growing keyspace — keys land at interior positions of compressed leaves,
// exercising the mid-page insert splice (page.TryInsertAt) and its
// decode/re-encode fallback. (Contrast BenchmarkPutSteadyState, whose ascending
// keys are all appends → TryAppend.) Keys share the "k" prefix + leading zero
// padding so the compressed variant groups them, then diverge in the low digits
// so inserts spread across groups. Run with a bounded -benchtime (see
// BenchmarkPutSteadyState — the keyspace grows without bound).
func BenchmarkPutRandom(b *testing.B) {
	for _, sz := range []int{200} {
		b.Run(fmt.Sprintf("val=%d", sz), func(b *testing.B) {
			db, _ := benchDB(b)
			defer db.Close()
			ctx := context.Background()
			val := make([]byte, sz)
			rng := rand.New(rand.NewPCG(0x9e3779b97f4a7c15, 0xC0FFEE))
			tx, err := db.Begin(ctx)
			if err != nil {
				b.Fatalf("Begin: %v", err)
			}
			ks, err := tx.CreateKeyspace("k")
			if err != nil {
				b.Fatalf("CreateKeyspace: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i > 0 && i%2000 == 0 {
					b.StopTimer()
					if err := tx.Commit(); err != nil {
						b.Fatalf("Commit: %v", err)
					}
					tx, err = db.Begin(ctx)
					if err != nil {
						b.Fatalf("Begin: %v", err)
					}
					ks, err = tx.OpenKeyspace("k")
					if err != nil {
						b.Fatalf("OpenKeyspace: %v", err)
					}
					b.StartTimer()
				}
				key := fmt.Appendf(nil, "k%015d", rng.Uint64()%1_000_000_000_000_000)
				if err := ks.Put(key, val); err != nil {
					b.Fatalf("Put #%d: %v", i, err)
				}
			}
			b.StopTimer()
			_ = tx.Commit()
		})
	}
}

// BenchmarkPutSteadyState measures the cost of a single-value Put into a
// growing keyspace. Keys ascend, so every Put appends to the rightmost leaf:
// the in-place append splice (page.TryAppend — page-formats.md §Insert and
// Delete) handles it without decoding, falling back to the whole-leaf
// decode (readLeafEntriesDeepCopy) + LeafBuilder re-encode only when the
// splice declines (page-full → split, or a variant/config mismatch).
//
// CPU-profile with a BOUNDED iteration count: the keyspace grows without
// bound, so a time-based -benchtime can exhaust the bench DB's MaxSize
// (~648K puts at val=200, independent of the splice — the rebuild path hits
// the same wall). A fixed count well under that samples steady state safely:
//
//	go test -run='^$' -bench=BenchmarkPutSteadyState -benchtime=300000x \
//	  -cpuprofile=/tmp/put.prof . && go tool pprof -top -nodecount=20 /tmp/put.prof
func BenchmarkPutSteadyState(b *testing.B) {
	for _, sz := range []int{200} {
		b.Run(fmt.Sprintf("val=%d", sz), func(b *testing.B) {
			db, _ := benchDB(b)
			defer db.Close()
			ctx := context.Background()
			val := make([]byte, sz)
			tx, err := db.Begin(ctx)
			if err != nil {
				b.Fatalf("Begin: %v", err)
			}
			ks, err := tx.CreateKeyspace("k")
			if err != nil {
				b.Fatalf("CreateKeyspace: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Commit periodically to bound the per-tx CoW buffer.
				if i > 0 && i%2000 == 0 {
					b.StopTimer()
					if err := tx.Commit(); err != nil {
						b.Fatalf("Commit: %v", err)
					}
					tx, err = db.Begin(ctx)
					if err != nil {
						b.Fatalf("Begin: %v", err)
					}
					ks, err = tx.OpenKeyspace("k")
					if err != nil {
						b.Fatalf("OpenKeyspace: %v", err)
					}
					b.StartTimer()
				}
				if err := ks.Put([]byte(fmt.Sprintf("k%09d", i)), val); err != nil {
					b.Fatalf("Put #%d: %v", i, err)
				}
			}
			b.StopTimer()
			_ = tx.Commit()
		})
	}
}

// BenchmarkPutSequentialFill measures the ascending-key insert workload
// end-to-end AND reports its space efficiency: pages consumed per 1000
// entries (from the HighWaterMark delta). The append-aware lopsided
// split (page-formats.md §Leaf Split, append-aware policy) packs left
// halves full, so the metric sits near the BulkLoad-packed floor;
// before it, every leaf on the ascending path stranded at ~50% fill
// and the metric ran ~2x higher. Run with a bounded -benchtime (the
// keyspace grows without bound; see BenchmarkPutSteadyState).
func BenchmarkPutSequentialFill(b *testing.B) {
	db, _ := benchDB(b)
	defer db.Close()
	ctx := context.Background()
	val := make([]byte, 100)
	tx, err := db.Begin(ctx)
	if err != nil {
		b.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		b.Fatalf("CreateKeyspace: %v", err)
	}
	startPages := db.pgr.HighWaterMark()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 && i%2000 == 0 {
			b.StopTimer()
			if err := tx.Commit(); err != nil {
				b.Fatalf("Commit: %v", err)
			}
			tx, err = db.Begin(ctx)
			if err != nil {
				b.Fatalf("Begin: %v", err)
			}
			ks, err = tx.OpenKeyspace("k")
			if err != nil {
				b.Fatalf("OpenKeyspace: %v", err)
			}
			b.StartTimer()
		}
		if err := ks.Put(fmt.Appendf(nil, "k%015d", i), val); err != nil {
			b.Fatalf("Put #%d: %v", i, err)
		}
	}
	b.StopTimer()
	_ = tx.Commit()
	if b.N > 0 {
		b.ReportMetric(float64(db.pgr.HighWaterMark()-startPages)/float64(b.N)*1000, "pages/1e3ops")
	}
}

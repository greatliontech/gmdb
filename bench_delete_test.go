package gmdb

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
)

// BenchmarkDeleteRandom measures a Delete of a present key from a populated
// keyspace — the in-place delete splice (page.TryDeleteAt) handles the common
// count>1 case without decoding the whole leaf, falling back to the
// decode/re-encode path only on a variant mismatch or when the page empties
// (which then merges/retires). Keys share the "k" prefix + zero padding so the
// compressed variant groups them; deletes hit interior leaf positions in
// shuffled order. The keyspace is refilled out-of-band (StopTimer'd) in fixed
// batches so it stays bounded and the timer covers Delete only. Both population
// and deletion commit every 2000 ops to bound the per-tx page budget. Run with a
// bounded -benchtime (the harness churns pages — see BenchmarkPutSteadyState).
func BenchmarkDeleteRandom(b *testing.B) {
	for _, sz := range []int{200} {
		b.Run(fmt.Sprintf("val=%d", sz), func(b *testing.B) {
			db, _ := benchDB(b)
			defer db.Close()
			ctx := context.Background()
			val := make([]byte, sz)
			const batch = 20000

			rng := rand.New(rand.NewPCG(0x9e3779b97f4a7c15, 0xDE1E7E))
			var base uint64 // monotonic so every refill uses fresh, unique keys

			tx, err := db.Begin(ctx)
			if err != nil {
				b.Fatalf("Begin: %v", err)
			}
			ks, err := tx.CreateKeyspace("k")
			if err != nil {
				b.Fatalf("CreateKeyspace: %v", err)
			}

			reopen := func() {
				if err = tx.Commit(); err != nil {
					b.Fatalf("Commit: %v", err)
				}
				if tx, err = db.Begin(ctx); err != nil {
					b.Fatalf("Begin: %v", err)
				}
				if ks, err = tx.OpenKeyspace("k"); err != nil {
					b.Fatalf("OpenKeyspace: %v", err)
				}
			}

			// refill populates `batch` fresh unique keys (committing every 2000),
			// and returns them in shuffled delete order.
			refill := func() [][]byte {
				keys := make([][]byte, batch)
				for j := range keys {
					k := fmt.Appendf(nil, "k%015d", base)
					base++
					keys[j] = k
					if err := ks.Put(k, val); err != nil {
						b.Fatalf("Put: %v", err)
					}
					if (j+1)%2000 == 0 {
						reopen()
					}
				}
				if batch%2000 != 0 {
					reopen()
				}
				rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
				return keys
			}

			b.StopTimer()
			keys := refill()
			pos := 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if pos >= len(keys) {
					b.StopTimer()
					keys = refill()
					pos = 0
					b.StartTimer()
				}
				if err := ks.Delete(keys[pos]); err != nil {
					b.Fatalf("Delete #%d: %v", i, err)
				}
				pos++
				if (i+1)%2000 == 0 {
					b.StopTimer()
					reopen()
					b.StartTimer()
				}
			}
			b.StopTimer()
			_ = tx.Commit()
		})
	}
}

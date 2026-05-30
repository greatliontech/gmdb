package gmdb

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkPutSteadyState measures the cost of a single-value Put into a
// growing keyspace — the path that currently decodes the whole target
// leaf (readLeafEntriesDeepCopy) and re-encodes it via LeafBuilder on
// every insert. CPU-profile this to size what an in-place splice layer
// (page-formats.md §Insert and Delete, currently unbuilt) would save:
//
//	go test -run='^$' -bench=BenchmarkPutSteadyState -benchtime=3s \
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

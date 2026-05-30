package gmdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Baseline measurements behind the on-demand-promotion design (put.go's
// store loop): the intrinsic cost of storing a value inline vs as an
// overflow chain. This cost is what ruled out the alternative of lowering
// the global overflow threshold (which would pay the overflow penalty on
// every ½–full-page value) in favour of promoting only values that
// actually block a leaf split. Run: task bench (or
// `go test -run=^$ -bench=. -benchmem .`).
//
// The inline→overflow boundary on a 4 KB page sits near ~4 KB: a value
// whose inline entry fits an otherwise-empty leaf stays inline (one leaf
// read, zero-copy slice into the mmap); a larger value promotes to an
// overflow chain (leaf read + 1+N follower reads + a reassembly copy).
// The ns/op and allocs jump across the boundary is the per-Get penalty
// that option (b) would pay on every ½–full-page value, and that option
// (a) pays only on values that actually block a split.

func benchDB(b *testing.B) (*DB, string) {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.db")
	db, err := Open(context.Background(), path, Options{PageSize: 4096, MinSize: 16, MaxSize: 65536})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	return db, path
}

func benchPopulate(b *testing.B, db *DB, n, valSize int) [][]byte {
	b.Helper()
	ctx := context.Background()
	tx, err := db.Begin(ctx)
	if err != nil {
		b.Fatalf("Begin: %v", err)
	}
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		b.Fatalf("CreateKeyspace: %v", err)
	}
	val := make([]byte, valSize)
	for i := range val {
		val[i] = byte(i)
	}
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte(fmt.Sprintf("k%07d", i))
		if err := ks.Put(keys[i], val); err != nil {
			b.Fatalf("Put #%d (val %d): %v", i, valSize, err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("Commit: %v", err)
	}
	return keys
}

// reportDisk reports total on-disk pages per stored entry — the storage
// overhead of the value-size class (inline near-full values give ~1
// entry/leaf-page = sparse trees; overflow refs pack many per leaf but
// add overflow pages elsewhere).
func reportDisk(b *testing.B, path string, n int) {
	fi, err := os.Stat(path)
	if err != nil {
		b.Fatalf("Stat: %v", err)
	}
	pages := float64(fi.Size()) / 4096
	b.ReportMetric(pages/float64(n), "diskpages/entry")
}

// BenchmarkGetByValueSize measures read cost across the inline→overflow
// boundary.
func BenchmarkGetByValueSize(b *testing.B) {
	const n = 512
	for _, sz := range []int{64, 1024, 2048, 3800, 4096, 6000, 12000} {
		b.Run(fmt.Sprintf("val=%d", sz), func(b *testing.B) {
			db, path := benchDB(b)
			defer db.Close()
			keys := benchPopulate(b, db, n, sz)

			rtx, err := db.BeginRead(context.Background())
			if err != nil {
				b.Fatalf("BeginRead: %v", err)
			}
			defer rtx.Rollback()
			rks, err := rtx.OpenKeyspaceReadOnly("k")
			if err != nil {
				b.Fatalf("OpenKeyspaceReadOnly: %v", err)
			}

			b.ReportAllocs()
			b.SetBytes(int64(sz))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				v, err := rks.Get(keys[i%n])
				if err != nil {
					b.Fatalf("Get: %v", err)
				}
				_ = v
			}
			b.StopTimer()
			reportDisk(b, path, n) // after the loop: ResetTimer deletes custom metrics
		})
	}
}

// BenchmarkPutByValueSize measures write cost across the boundary,
// including the overflow-chain allocation + write above it.
func BenchmarkPutByValueSize(b *testing.B) {
	for _, sz := range []int{64, 1024, 3800, 4096, 12000} {
		b.Run(fmt.Sprintf("val=%d", sz), func(b *testing.B) {
			val := make([]byte, sz)
			b.ReportAllocs()
			b.SetBytes(int64(sz))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				db, _ := benchDB(b)
				ctx := context.Background()
				tx, err := db.Begin(ctx)
				if err != nil {
					b.Fatalf("Begin: %v", err)
				}
				ks, err := tx.CreateKeyspace("k")
				if err != nil {
					b.Fatalf("CreateKeyspace: %v", err)
				}
				b.StartTimer()
				for j := 0; j < 64; j++ {
					if err := ks.Put([]byte(fmt.Sprintf("k%07d", j)), val); err != nil {
						b.Fatalf("Put: %v", err)
					}
				}
				b.StopTimer()
				if err := tx.Commit(); err != nil {
					b.Fatalf("Commit: %v", err)
				}
				db.Close()
				b.StartTimer()
			}
		})
	}
}

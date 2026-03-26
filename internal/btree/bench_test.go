package btree

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// benchTree creates a Tree with a given page count for benchmarks.
func benchTree(b *testing.B, numPages int) *Tree {
	b.Helper()
	cfg := page.PageConfig{PageSize: testPageSize}
	bitmapPages := cfg.BitmapPages(uint64(numPages))
	reservedPages := 2 + uint64(bitmapPages)

	data := make([]byte, numPages*testPageSize)
	bitmapData := data[2*testPageSize : (2+int(bitmapPages))*testPageSize]
	bm := bitmap.New(bitmapData, uint64(numPages), reservedPages)
	for i := reservedPages; i < uint64(numPages); i++ {
		bm.Set(i)
	}
	return New(data, cfg, bm, 0)
}

// populateTree inserts n entries with the given key/value sizes.
func populateTree(b *testing.B, tr *Tree, n int, keySize, valSize int) {
	b.Helper()
	for i := range n {
		key := make([]byte, keySize)
		copy(key, fmt.Sprintf("key:%08d", i))
		val := bytes.Repeat([]byte("v"), valSize)
		if _, _, err := tr.Put(inlineEntry(key, val)); err != nil {
			b.Fatalf("populate Put(%d): %v", i, err)
		}
	}
}

// --- Point operation benchmarks ---

func BenchmarkPutSequential(b *testing.B) {
	for _, valSize := range []int{8, 100, 500} {
		b.Run(fmt.Sprintf("val=%d", valSize), func(b *testing.B) {
			tr := benchTree(b, 65536)
			val := bytes.Repeat([]byte("v"), valSize)
			b.ResetTimer()
			b.ReportAllocs()
			for i := range b.N {
				key := fmt.Appendf(nil, "key:%08d", i)
				tr.Put(inlineEntry(key, val))
			}
		})
	}
}

func BenchmarkPutRandom(b *testing.B) {
	for _, valSize := range []int{8, 100, 500} {
		b.Run(fmt.Sprintf("val=%d", valSize), func(b *testing.B) {
			tr := benchTree(b, 65536)
			val := bytes.Repeat([]byte("v"), valSize)
			rng := rand.New(rand.NewPCG(42, 0))
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				key := fmt.Appendf(nil, "key:%08d", rng.IntN(10_000_000))
				tr.Put(inlineEntry(key, val))
			}
		})
	}
}

func BenchmarkPutReplace(b *testing.B) {
	tr := benchTree(b, 65536)
	populateTree(b, tr, 10000, 20, 100)
	val := bytes.Repeat([]byte("x"), 100)
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		key := fmt.Appendf(nil, "key:%08d", i%10000)
		tr.Put(inlineEntry(key, val))
	}
}

func BenchmarkGet(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			pages := n/2 + 1024
			tr := benchTree(b, pages)
			populateTree(b, tr, n, 20, 100)
			rng := rand.New(rand.NewPCG(42, 0))
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				key := fmt.Appendf(nil, "key:%08d", rng.IntN(n))
				tr.Get(key)
			}
		})
	}
}

// BenchmarkPutFreshTxn simulates real transaction patterns: each put hits
// a leaf that hasn't been CoW'd in this transaction (Reset between ops).
func BenchmarkPutFreshTxn(b *testing.B) {
	tr := benchTree(b, 65536)
	populateTree(b, tr, 10000, 20, 100)
	root := tr.Root()
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		tr.Reset(root)
		key := fmt.Appendf(nil, "key:%08d", i%10000)
		tr.Put(inlineEntry(key, []byte("new-value-that-is-about-100-bytes-long-to-match-the-original-value-size-in-the-tree-padding")))
	}
}

// BenchmarkDeleteFreshTxn simulates single-delete transactions.
func BenchmarkDeleteFreshTxn(b *testing.B) {
	tr := benchTree(b, 65536)
	populateTree(b, tr, 10000, 20, 100)
	root := tr.Root()
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		tr.Reset(root)
		key := fmt.Appendf(nil, "key:%08d", i%10000)
		tr.Delete(key)
	}
}

func BenchmarkDelete(b *testing.B) {
	tr := benchTree(b, 65536)
	populateTree(b, tr, 50000, 20, 100)
	b.ResetTimer()
	b.ReportAllocs()
	for i := range b.N {
		key := fmt.Appendf(nil, "key:%08d", i%50000)
		tr.Delete(key)
	}
}

// --- Scan benchmarks ---

func BenchmarkCursorScanForward(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			pages := n/2 + 1024
			tr := benchTree(b, pages)
			populateTree(b, tr, n, 20, 100)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				c := tr.NewCursor()
				for k, _ := c.First(); k != nil; k, _ = c.Next() {
				}
			}
		})
	}
}

func BenchmarkCursorScanReverse(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			pages := n/2 + 1024
			tr := benchTree(b, pages)
			populateTree(b, tr, n, 20, 100)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				c := tr.NewCursor()
				for k, _ := c.Last(); k != nil; k, _ = c.Prev() {
				}
			}
		})
	}
}

func BenchmarkCursorSeekGE(b *testing.B) {
	tr := benchTree(b, 65536)
	populateTree(b, tr, 50000, 20, 100)
	rng := rand.New(rand.NewPCG(42, 0))
	b.ResetTimer()
	b.ReportAllocs()
	c := tr.NewCursor()
	for range b.N {
		key := fmt.Appendf(nil, "key:%08d", rng.IntN(50000))
		c.SeekGE(key)
	}
}

// --- Bulk operation benchmarks ---

func BenchmarkDeleteRange(b *testing.B) {
	for _, rangeSize := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("range=%d", rangeSize), func(b *testing.B) {
			pages := 65536
			if rangeSize > 5000 {
				pages = 131072
			}
			tr := benchTree(b, pages)
			n := rangeSize * 3
			populateTree(b, tr, n, 20, 100)
			root := tr.Root()
			// Pick a stable range in the middle of the key space.
			start := fmt.Appendf(nil, "key:%08d", n/3)
			end := fmt.Appendf(nil, "key:%08d", n/3+rangeSize)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				tr.Reset(root)
				tr.DeleteRange(start, end)
			}
		})
	}
}

// --- Key size impact benchmarks ---

func BenchmarkPutKeySize(b *testing.B) {
	for _, keySize := range []int{8, 32, 128, 512} {
		b.Run(fmt.Sprintf("keylen=%d", keySize), func(b *testing.B) {
			tr := benchTree(b, 131072)
			val := []byte("v")
			prefix := bytes.Repeat([]byte("p"), keySize-8)
			b.ResetTimer()
			b.ReportAllocs()
			for i := range b.N {
				key := append(prefix, fmt.Appendf(nil, "%08d", i)...)
				tr.Put(inlineEntry(key, val))
			}
		})
	}
}

func BenchmarkGetKeySize(b *testing.B) {
	for _, keySize := range []int{8, 32, 128, 512} {
		b.Run(fmt.Sprintf("keylen=%d", keySize), func(b *testing.B) {
			tr := benchTree(b, 131072)
			prefix := bytes.Repeat([]byte("p"), keySize-8)
			n := 50000
			for i := range n {
				key := append(bytes.Clone(prefix), fmt.Appendf(nil, "%08d", i)...)
				tr.Put(inlineEntry(key, []byte("v")))
			}
			rng := rand.New(rand.NewPCG(42, 0))
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				key := append(bytes.Clone(prefix), fmt.Appendf(nil, "%08d", rng.IntN(n))...)
				tr.Get(key)
			}
		})
	}
}

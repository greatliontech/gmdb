package btree

import (
	"fmt"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/bitmap"
	"github.com/thegrumpylion/gmdb/internal/page"
)

const testPageSize = 4096

// newTestTree creates a Tree backed by a zeroed page buffer with a realistic
// layout: meta pages (0-1), bitmap pages (2..2+bitmapPages-1), data pages.
func newTestTree(t *testing.T, numPages int) *Tree {
	t.Helper()
	cfg := page.PageConfig{PageSize: testPageSize}
	bitmapPages := cfg.BitmapPages(uint64(numPages))
	reservedPages := 2 + uint64(bitmapPages)

	data := make([]byte, numPages*testPageSize)
	bitmapData := data[2*testPageSize : (2+int(bitmapPages))*testPageSize]
	bm := bitmap.New(bitmapData, uint64(numPages), reservedPages)

	// Mark all data pages as free.
	for i := reservedPages; i < uint64(numPages); i++ {
		bm.Set(i)
	}

	return New(data, cfg, bm, 0)
}

// testKey returns a key like "key:0042" for deterministic testing.
func testKey(i int) []byte {
	return []byte(fmt.Sprintf("key:%04d", i))
}

// testVal returns a value like "val:0042".
func testVal(i int) []byte {
	return []byte(fmt.Sprintf("val:%04d", i))
}

// inlineEntry creates an inline key-value LeafEntry.
func inlineEntry(key, value []byte) page.LeafEntry {
	return page.LeafEntry{Key: key, Value: value}
}

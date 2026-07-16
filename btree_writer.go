package gmdb

import (
	"github.com/greatliontech/gmdb/internal/btree"
	"github.com/greatliontech/gmdb/internal/pager"
)

// btreeWriter adapts *pager.Pager to btree.PageWriter. It is the one
// place the two vocabularies meet: the pager owns the MVCC + slab
// machinery and names it accordingly (CoW, AllocSlab, WriteDirectRaw
// — see docs/specs/pager-slab.md), while btree consumes an opaque
// page-buffer store and names it in storage-neutral terms (CopyPage,
// ZeroPage, WriteRunPage). Bridging here keeps pager internals out of
// the btree package's surface — without this adapter the btree
// interface would have to mirror the pager's method names verbatim.
//
// btreeWriter is pointer-shaped (a single embedded *pager.Pager), so
// passing it where a btree.PageWriter is expected is allocation-free.
// The embedded pager supplies the unchanged half of the interface
// directly — AllocPage, FreePage, AllocContiguous, FreeRun, and Page
// (the PageReader method) — so only the three renamed primitives are
// bridged below.
type btreeWriter struct{ *pager.Pager }

// compile-time assertion that the adapter satisfies the interface.
var _ btree.PageWriter = btreeWriter{}

// CopyPage bridges btree's copy-on-write primitive to pager.CoW.
func (w btreeWriter) CopyPage(srcID, dstID uint64) ([]byte, error) {
	return w.Pager.CoW(srcID, dstID)
}

// ZeroPage bridges btree's fresh-zeroed-page primitive to pager.AllocSlab.
func (w btreeWriter) ZeroPage(id uint64) ([]byte, error) {
	return w.Pager.AllocSlab(id)
}

// WriteRunPage bridges btree's direct run-page write to
// pager.WriteDirectRaw (run pages carry no per-page footer; the
// head-resident whole-run digest is the run's integrity cover).
func (w btreeWriter) WriteRunPage(id uint64, buf []byte) error {
	return w.Pager.WriteDirectRaw(id, buf)
}

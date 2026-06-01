package btree

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// Overflow value support per page-formats.md §Overflow Page and
// limits.md §Maximum Value Size. The btree promotes a value to an
// overflow chain when its inline encoding cannot fit on a single-
// entry leaf page. The chain is a contiguous run of 1 + N pages
// (first carries the page header with AdditionalPages = N; N
// followers carry pure value bytes).
//
// Lifecycle. Every overflow chain is owned by exactly one leaf
// entry; the chain is allocated when the leaf entry is created and
// freed (FreeRun in the same write tx) when the leaf entry is
// replaced with a new value or deleted. The btree's invariant:
// every bitmap-allocated overflow chain is reachable from a live
// leaf entry or in the write tx's pending-free set — no orphan
// chains.

// inlineEntryHeaderOverhead is the per-entry byte overhead in an
// uncompressed leaf, restart entry, or delta entry that uses the
// inline form: 1 byte CellFlags + 2 byte KeyLen + 4 byte ValueLen.
// (Delta entries replace KeyLen with SharedLen+UnsharedLen — same
// 2-byte width.) Used by needsOverflow to predict the inline entry
// size before constructing it.
const inlineEntryHeaderOverhead = 1 + 2 + 4

// overflowEntryHeaderOverhead is the per-entry byte overhead for an
// overflow-reference entry: 1 + 2 + 8 + 8 (CellFlags + KeyLen +
// OvflPage + TotalLen). Same on restart entries, delta entries, and
// uncompressed entries — the variants encode the overflow form
// identically past the key prefix.
const overflowEntryHeaderOverhead = 1 + 2 + 8 + 8

// singleEntryPageOverhead is the leaf-page overhead that an
// otherwise-empty leaf charges for one entry: header (12 bytes —
// page header + DataEnd field) + the smallest valid one-entry
// lookup-table footprint (the compressed-variant restart-table
// entry at 4 bytes; the uncompressed variant's offset-table slot
// is 2 bytes, so picking 4 is the safer over-estimate). Used by
// needsOverflow as the universal "one-entry leaf capacity" budget;
// for the strict per-variant value see internal/page/leaf.go.
const singleEntryPageOverhead = 12 + 4

// needsOverflow reports whether `(key, value)` must be stored as an
// overflow chain. The rule is strict-fit: overflow iff the inline
// encoding cannot fit on an otherwise-empty single-entry leaf page.
// This minimizes overflow chain usage (each chain costs a separate
// page allocation + a 1+N-page IO on Get); profiling can introduce
// a lower threshold later if dense-large-value workloads benefit
// from earlier promotion.
//
// At needsOverflow's threshold (key + value just exceeds the
// single-entry capacity), the resulting leaf has exactly 1 entry
// per page — space-inefficient but functionally correct.
func needsOverflow(cfg page.Config, key, value []byte) bool {
	inlineSize := inlineEntryHeaderOverhead + len(key) + len(value)
	return inlineSize > cfg.ContentEnd()-singleEntryPageOverhead
}

// overflowRefFitsLeaf reports whether an overflow-reference entry
// for `key` fits in a single-entry leaf page. False ⇒ the key is
// too large for even the overflow form (ErrKeyTooLarge surface).
func overflowRefFitsLeaf(cfg page.Config, key []byte) bool {
	refSize := overflowEntryHeaderOverhead + len(key)
	return refSize <= cfg.ContentEnd()-singleEntryPageOverhead
}

// NeedsOverflow exports needsOverflow for the bulk-load construction
// path (package gmdb), which must make the SAME base inline-vs-overflow
// decision as the Put path so a value Put would inline, BulkLoad also
// inlines, and a value Put would promote, BulkLoad also promotes — one
// threshold, no drift. needsOverflow is conservative (over-estimates leaf
// overhead), so anything it reports as inline definitely fits an
// otherwise-empty leaf built by the same page.LeafBuilder.
//
// The Put split path may *additionally* promote an already-inline value
// to overflow when a leaf is too size-skewed for any two-page split (see
// put.go's store loop / largestInlineEntry). That is an on-demand layer
// above this threshold, not a threshold change: BulkLoad never needs it
// (it builds packed leaves bottom-up without splitting), and the leaf
// records the resulting overflow cell, so the two paths' base decision
// stays identical even though Put can end up with more overflow cells.
func NeedsOverflow(cfg page.Config, key, value []byte) bool {
	return needsOverflow(cfg, key, value)
}

// OverflowRefFitsLeaf exports overflowRefFitsLeaf for the bulk-load path.
// False ⇒ the key is too large even for the overflow-reference form
// (the ErrKeyTooLarge surface).
func OverflowRefFitsLeaf(cfg page.Config, key []byte) bool {
	return overflowRefFitsLeaf(cfg, key)
}

// writeOverflowChain allocates a contiguous run of pages, encodes
// `value` across them, and returns the new LeafEntry with overflow
// fields set. On error any partial allocation is rolled back via
// FreeRun before the error is surfaced — the chain either lands
// fully or not at all.
func writeOverflowChain(pw PageWriter, cfg page.Config, key, value []byte) (page.LeafEntry, error) {
	runLen := page.OverflowRunLength(cfg, uint64(len(value)))
	firstID, err := pw.AllocContiguous(runLen)
	if err != nil {
		return page.LeafEntry{}, fmt.Errorf("btree: alloc overflow chain (run=%d): %w", runLen, err)
	}
	pages, err := pw.ZeroPageRun(firstID, runLen)
	if err != nil {
		_ = pw.FreeRun(firstID, runLen)
		return page.LeafEntry{}, fmt.Errorf("btree: alloc overflow slab run: %w", err)
	}
	if err := page.EncodeOverflowRun(pages, cfg, value); err != nil {
		_ = pw.FreeRun(firstID, runLen)
		return page.LeafEntry{}, fmt.Errorf("btree: encode overflow run: %w", err)
	}
	return page.LeafEntry{
		Flags:        page.CellFlagOverflow,
		Key:          key,
		OverflowPage: firstID,
		TotalLen:     uint64(len(value)),
	}, nil
}

// freeOverflowChainIfPresent retires the overflow chain rooted at
// entry.OverflowPage if entry carries the overflow flag. No-op
// otherwise. Used after a Put-replace or a Delete to release the
// chain owned by the displaced leaf entry; called AFTER the leaf-
// level CoW lands so a transient failure can't orphan the chain.
func freeOverflowChainIfPresent(pw PageWriter, cfg page.Config, entry page.LeafEntry) error {
	if !entry.IsOverflow() {
		return nil
	}
	runLen := page.OverflowRunLength(cfg, entry.TotalLen)
	if err := pw.FreeRun(entry.OverflowPage, runLen); err != nil {
		return fmt.Errorf("btree: free overflow chain at %d (run=%d): %w", entry.OverflowPage, runLen, err)
	}
	return nil
}

// readOverflowValue assembles the value bytes from the overflow
// chain rooted at entry.OverflowPage. Returns a heap-allocated
// slice of length entry.TotalLen — caller-owned, independent of
// the pager's slab / mmap lifetimes. (See api-surface.md §Byte
// Slice Ownership: overflow values diverge from the
// borrowed-reference rule because the value spans non-contiguous
// regions of the mmap (per-page headers / footers); a heap copy
// is the simplest correct shape. Profile-revisit if allocation
// pressure becomes material.)
func readOverflowValue(pr PageReader, cfg page.Config, entry page.LeafEntry) ([]byte, error) {
	if !entry.IsOverflow() {
		return nil, fmt.Errorf("btree: readOverflowValue called on non-overflow entry")
	}
	// Forged-length bound (checksums.md §Structural and Allocation Bounds): a forged on-disk TotalLen can imply a run of billions of
	// pages (OverflowRunLength truncates to uint32, so a naive
	// run-vs-extent guard would pass while make([]byte, TotalLen) is
	// enormous). Compute the run length in uint64 and read pages ONE AT A
	// TIME — pr.Page bounds each id against the file-resident extent
	// (checksums.md §Structural and Allocation Bounds), so a run that walks past the file aborts here, before the
	// TotalLen-sized allocation. Do not pre-size `pages` to run64 (itself
	// possibly forged-huge); a run that stays in-bounds is ≤ the file's
	// page count, which bounds TotalLen ≤ file size for the assembly.
	run64 := page.OverflowRunLength64(cfg, entry.TotalLen)
	pages := make([][]byte, 0, int(min(run64, 64)))
	for i := range run64 {
		buf, err := pr.Page(entry.OverflowPage + i)
		if err != nil {
			return nil, err
		}
		pages = append(pages, buf)
	}
	dst := make([]byte, entry.TotalLen)
	n, err := page.AssembleOverflowValue(pages, cfg, dst)
	if err != nil {
		return nil, fmt.Errorf("%w: overflow chain at %d: %w", ErrCorrupted, entry.OverflowPage, err)
	}
	if uint64(n) != entry.TotalLen {
		return nil, fmt.Errorf("%w: overflow chain at %d short-assembled %d of %d bytes",
			ErrCorrupted, entry.OverflowPage, n, entry.TotalLen)
	}
	return dst, nil
}

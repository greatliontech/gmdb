package btree

// LeafEntry-level read/write primitives used by the SetKeyspace
// surface. The Get/Put pair operates on (key, value []byte)
// — flags are always 0 (Put) and ignored (Get returns just the
// value). SetKeyspace cells carry CellFlagMultiValue and (for
// nested-tree references) CellFlagNestedTree, which the value-only
// API can't surface. GetEntry returns the full LeafEntry (Flags +
// NestedRoot + NestedCount as applicable); PutEntry inserts/replaces
// using an arbitrary LeafEntry whose Flags drive the leaf-builder
// dispatch (AddInline / AddOverflow / AddSubpage / AddNestedTreeRef).

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// GetEntry descends the tree rooted at rootID looking for key. On a
// hit returns the full LeafEntry (with Flags / Value / OverflowPage
// / TotalLen / NestedRoot / NestedCount populated per cell type).
// Slice ownership for Key / Value matches LeafIter: BORROWED from
// the leaf page buffer, valid for the tx's lifetime. Caller MUST
// copy before mutating the underlying leaf.
//
// Empty tree (rootID == 0) returns (zero, false, nil). On miss
// returns (zero, false, nil). On structural corruption returns
// ErrCorrupted (or its derivatives) wrapped.
//
// Cost: O(depth + log K) — one page resolution per level plus a
// binary search of the matched group on compressed leaves (uniform
// O(log N) on uncompressed leaves).
func GetEntry(pr PageReader, cfg page.Config, rootID uint64, key []byte) (page.LeafEntry, bool, error) {
	if rootID == 0 {
		return page.LeafEntry{}, false, nil
	}
	cur := rootID
	for depth := 0; depth <= MaxTreeDepth; depth++ {
		buf, err := pr.Page(cur)
		if err != nil {
			return page.LeafEntry{}, false, err
		}
		typ, _, _, _ := page.ReadHeader(buf)
		if page.IsLeafType(typ) {
			r := page.NewLeafReader(buf, cfg)
			if err := r.Validate(); err != nil {
				return page.LeafEntry{}, false, fmt.Errorf("%w: leaf %d at depth %d: %w", ErrCorrupted, cur, depth, err)
			}
			idx, e, found := r.SearchLeaf(key)
			if !found {
				return page.LeafEntry{}, false, nil
			}
			// SearchLeaf nils Key on a match (caller already has it
			// as the target); re-fetch via EntryAt for the full
			// entry view that callers may want for Has-style reads.
			full, _ := r.EntryAt(idx, nil)
			_ = e
			return full, true, nil
		}
		if typ != page.TypeBranch {
			return page.LeafEntry{}, false, fmt.Errorf("%w: page %d has unexpected type %d during GetEntry descent", ErrCorrupted, cur, typ)
		}
		if err := validateBranchPage(buf, cfg, cur); err != nil {
			return page.LeafEntry{}, false, err
		}
		i := page.BranchSearch(buf, cfg, key)
		next := page.BranchChildAt(buf, cfg, i)
		if next == 0 {
			return page.LeafEntry{}, false, fmt.Errorf("%w: null child pointer in branch %d during GetEntry descent", ErrCorrupted, cur)
		}
		cur = next
	}
	return page.LeafEntry{}, false, ErrTreeTooDeep
}

// PutEntry inserts or replaces e in the tree rooted at rootID. The
// caller has full control over e's encoding via Flags / Value /
// OverflowPage+TotalLen / NestedRoot+NestedCount. Used by
// SetKeyspace.Put to install Subpage or NestedTree cells whose
// encoding btree.Put does not support (Put writes flags=0 only).
//
// Returns (newRoot, displaced, err): newRoot is the post-CoW root;
// displaced is the LeafEntry that was at e.Key before the call
// (zero-valued on insert; non-zero on replace). **On replace
// (displaced.Flags != 0), the caller MUST inspect displaced and
// free any trailer-owned pages the displaced cell holds** —
// FreeSubtree on a NestedTree's NestedRoot, FreeRun on an
// Overflow chain. PutEntry does NOT free these because the
// resource model differs by cell type and the caller has the
// keyspace context (e.g., fixedValueSize for subpage decoding)
// that PutEntry does not. For subpage cells the value bytes are
// inline (no trailer pages to free); for nested-tree cells whose
// new entry shares the same NestedRoot (e.g., a btree.Put on
// the nested tree that re-uses the root), the caller MUST NOT
// free it.
//
// On error: any pages allocated during this PutEntry (the CoW'd
// leaf, split-right leaf, ascend-split branches) are freed via
// FreePage; the returned newRoot is meaningful only when err == nil.
// e's pre-existing overflow chain / nested-tree pages are NOT
// touched by PutEntry — the caller has full control.
//
// Empty tree (rootID == 0): allocates a single-leaf root containing
// just e; returns (newRoot, zero LeafEntry, nil).
//
// Mirrors btree.Put's descent + rebuild infrastructure. The only
// differences from Put: no buildPutEntry (caller-supplied), no
// chain rollback (caller-managed), and the displaced entry is
// returned to the caller instead of being freed internally.
func PutEntry(pw PageWriter, cfg page.Config, rootID uint64, e page.LeafEntry) (newRoot uint64, displaced page.LeafEntry, err error) {
	if rootID == 0 {
		id, err := putEmptyEntry(pw, cfg, e)
		return id, page.LeafEntry{}, err
	}

	// Phase 1: descend, recording the path.
	path := make([]pathFrame, 0, 8)
	cur := rootID
	for depth := 0; depth <= MaxTreeDepth; depth++ {
		buf, err := pw.Page(cur)
		if err != nil {
			return 0, page.LeafEntry{}, err
		}
		typ, _, _, _ := page.ReadHeader(buf)
		if page.IsLeafType(typ) {
			break
		}
		if typ != page.TypeBranch {
			return 0, page.LeafEntry{}, fmt.Errorf("%w: page %d has unexpected type %d during PutEntry descent", ErrCorrupted, cur, typ)
		}
		if err := validateBranchPage(buf, cfg, cur); err != nil {
			return 0, page.LeafEntry{}, err
		}
		i := page.BranchSearch(buf, cfg, e.Key)
		next := page.BranchChildAt(buf, cfg, i)
		if next == 0 {
			return 0, page.LeafEntry{}, fmt.Errorf("%w: null child pointer in branch %d during PutEntry descent", ErrCorrupted, cur)
		}
		path = append(path, pathFrame{pageID: cur, descentIdx: i})
		cur = next
	}
	if len(path) > MaxTreeDepth {
		return 0, page.LeafEntry{}, ErrTreeTooDeep
	}
	leafID := cur

	// Phase 2: leaf mutation.
	leftID, err := pw.AllocPage()
	if err != nil {
		return 0, page.LeafEntry{}, fmt.Errorf("btree: alloc CoW leaf: %w", err)
	}
	leftBuf, err := pw.CopyPage(leafID, leftID)
	if err != nil {
		return 0, page.LeafEntry{}, fmt.Errorf("btree: CoW leaf: %w", err)
	}
	entries, err := readLeafEntriesDeepCopyWithTrailers(leftBuf, cfg, leafID)
	if err != nil {
		_ = pw.FreePage(leftID)
		return 0, page.LeafEntry{}, err
	}

	entries, displaced, _ = insertOrReplaceLeaf(entries, e)

	// Attempt single-page build.
	b := page.NewLeafBuilder(leftBuf, cfg)
	fits := true
	for _, ent := range entries {
		if !b.AddEntry(ent) {
			fits = false
			break
		}
	}
	if fits {
		b.Finish()
		if err := pw.FreePage(leafID); err != nil {
			_ = pw.FreePage(leftID)
			return 0, page.LeafEntry{}, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
		}
		newID, err := ascendNoSplit(pw, cfg, path, leftID)
		if err != nil {
			return 0, page.LeafEntry{}, err
		}
		return newID, displaced, nil
	}

	// Split required — byte-balanced boundary (page-formats.md §Leaf
	// Split), not the entry-count midpoint. See findLeafSplitIndex.
	mid, ok := findLeafSplitIndex(b, leftBuf, cfg, entries)
	if !ok {
		_ = pw.FreePage(leftID)
		return 0, page.LeafEntry{}, ErrKeyTooLarge
	}

	b.Reset(leftBuf, cfg)
	for _, ent := range entries[:mid] {
		if !b.AddEntry(ent) {
			_ = pw.FreePage(leftID)
			return 0, page.LeafEntry{}, ErrKeyTooLarge
		}
	}
	b.Finish()

	rightID, err := pw.AllocPage()
	if err != nil {
		_ = pw.FreePage(leftID)
		return 0, page.LeafEntry{}, fmt.Errorf("btree: alloc split-right leaf: %w", err)
	}
	rightBuf, err := pw.ZeroPage(rightID)
	if err != nil {
		_ = pw.FreePage(leftID)
		_ = pw.FreePage(rightID)
		return 0, page.LeafEntry{}, fmt.Errorf("btree: alloc split-right buf: %w", err)
	}
	rb := page.NewLeafBuilder(rightBuf, cfg)
	for _, ent := range entries[mid:] {
		if !rb.AddEntry(ent) {
			_ = pw.FreePage(leftID)
			_ = pw.FreePage(rightID)
			return 0, page.LeafEntry{}, ErrKeyTooLarge
		}
	}
	rb.Finish()

	if err := pw.FreePage(leafID); err != nil {
		_ = pw.FreePage(leftID)
		_ = pw.FreePage(rightID)
		return 0, page.LeafEntry{}, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
	}

	sep := page.ShortestSeparator(entries[mid-1].Key, entries[mid].Key)
	newID, err := ascendWithSplit(pw, cfg, path, leftID, sep, rightID)
	if err != nil {
		return 0, page.LeafEntry{}, err
	}
	return newID, displaced, nil
}

// putEmptyEntry allocates a single-leaf root containing just e.
// Mirrors putEmpty (putEmpty's overflow-promotion path is
// inapplicable here — the caller has already constructed e with any
// overflow / nested-tree references in place).
func putEmptyEntry(pw PageWriter, cfg page.Config, e page.LeafEntry) (uint64, error) {
	id, err := pw.AllocPage()
	if err != nil {
		return 0, fmt.Errorf("btree: alloc genesis leaf: %w", err)
	}
	buf, err := pw.ZeroPage(id)
	if err != nil {
		_ = pw.FreePage(id)
		return 0, fmt.Errorf("btree: alloc genesis slab: %w", err)
	}
	b := page.NewLeafBuilder(buf, cfg)
	if !b.AddEntry(e) {
		_ = pw.FreePage(id)
		return 0, ErrKeyTooLarge
	}
	b.Finish()
	return id, nil
}

// readLeafEntriesDeepCopyWithTrailers is the entry-trailer-aware
// counterpart of readLeafEntriesDeepCopy. The base helper clones
// only Key and Value — sufficient for the common case where
// entries are plain inline or overflow (the Overflow trailer
// fields OverflowPage / TotalLen are uint64 values, copied by
// value in the LeafEntry struct, so no clone needed).
//
// For PutEntry, the same observation holds: NestedTree
// trailer fields NestedRoot / NestedCount are uint64s. So
// readLeafEntriesDeepCopy works correctly for SetKeyspace cells
// too — this helper is a forwarding alias kept for documentation
// clarity and to allow a future per-cell-type clone if some new
// cell type adds a borrowed slice into the trailer.
func readLeafEntriesDeepCopyWithTrailers(buf []byte, cfg page.Config, pageID uint64) ([]page.LeafEntry, error) {
	return readLeafEntriesDeepCopy(buf, cfg, pageID)
}

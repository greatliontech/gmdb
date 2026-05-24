package btree

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// PageWriter extends PageReader with the write-path operations
// btree mutations need. *pager.Pager satisfies it.
//
// Lifecycle contract: every page the btree allocates is either
// (a) installed in the tree (chain reachable from the returned new
// rootID), or (b) freed via FreePage before Put returns. The
// pager's slab manages the byte buffers; btree never owns them
// past the Put call.
type PageWriter interface {
	PageReader

	// AllocPage returns a fresh page ID, sourced from the
	// freespace allocator's priority order (loose → bitmap → RPL
	// reclamation → file extension; see free-space.md).
	AllocPage() (uint64, error)

	// CoW installs a fresh slab buffer at dstID, populated with
	// the current content of srcID. dstID is supplied by the
	// caller's prior AllocPage. Returns the writable buffer.
	CoW(srcID, dstID uint64) ([]byte, error)

	// AllocSlab installs a fresh zero-filled slab buffer at id
	// without reading any source page. Used for newly-encoded
	// pages (split halves, new root branch) that have no prior
	// on-disk content.
	AllocSlab(id uint64) ([]byte, error)

	// FreePage retires id. Same-tx pages (from earlier in this
	// Put or the same write tx) become loose pages reusable
	// within the tx; prior-tx pages enter the RPL at commit.
	FreePage(id uint64) error
}

// ErrKeyTooLarge is returned by Put when the combined key/value
// would not fit in a single leaf page even after split. The chunk-
// 4.7 overflow-value path lifts the value limit; the chunk-7
// limits.md max-key-size rule lifts via tree-depth constraints —
// for now, callers must keep (key+value) below leaf capacity.
var ErrKeyTooLarge = errors.New("btree: key/value too large for leaf split")

// pathFrame records one level of the descent path for the CoW-
// propagation pass. pageID is the page descended through;
// descentIdx is the BranchSearch return value used at that level
// (so the ascend pass knows which child pointer to update).
type pathFrame struct {
	pageID     uint64
	descentIdx uint16
}

// Put inserts or updates key=value in the tree rooted at rootID.
// Returns the new rootID — the caller (keyspace descriptor update,
// chunk 5+) records this in the keyspace descriptor and propagates
// the descriptor update via CoW to the meta page.
//
// Mutations:
//   - rootID==0 (empty tree): allocates a fresh leaf containing
//     just (key, value); returns the new leaf's pageID.
//   - rootID!=0, key exists: CoWs leaf, updates value in place
//     (or splits if the new value is bigger than the old's room).
//   - rootID!=0, key new: CoWs leaf, inserts entry; on overflow
//     splits leaf, propagates a separator up the path, splitting
//     branches as needed; if root splits, grows a new root.
//
// On error: any pages already allocated during this Put are freed
// via the pager's FreePage (they become loose pages for re-use
// within this tx). The returned rootID is meaningful only when
// err == nil; on err the caller retains the prior rootID.
//
// Leaf format. Every leaf produced by Put is built via
// page.LeafBuilder against cfg.RestartGroupTarget (compressed when
// ≥2 / 0, uncompressed when ==1 — see internal/page/leaf_builder.go
// for variant dispatch). The chunk-4.6β builder owns natural-break
// heuristics + table layout; btree treats leaves as opaque past
// the LeafReader / LeafBuilder interface.
func Put(pw PageWriter, cfg page.Config, rootID uint64, key, value []byte) (uint64, error) {
	if rootID == 0 {
		return putEmpty(pw, cfg, key, value)
	}

	// Phase 1: descend, recording the path.
	path := make([]pathFrame, 0, 8)
	cur := rootID
	for depth := 0; depth <= MaxTreeDepth; depth++ {
		buf := pw.Page(cur)
		typ, _, _, _ := page.ReadHeader(buf)
		if page.IsLeafType(typ) {
			break
		}
		if typ != page.TypeBranch {
			return 0, fmt.Errorf("%w: page %d has unexpected type %d during Put descent", ErrCorrupted, cur, typ)
		}
		i := page.BranchSearch(buf, cfg, key)
		next := page.BranchChildAt(buf, cfg, i)
		if next == 0 {
			return 0, fmt.Errorf("%w: null child pointer in branch %d during Put descent", ErrCorrupted, cur)
		}
		path = append(path, pathFrame{pageID: cur, descentIdx: i})
		cur = next
	}
	if len(path) > MaxTreeDepth {
		return 0, ErrTreeTooDeep
	}
	leafID := cur

	// Phase 2: leaf mutation. CoW the leaf, decode entries (deep-
	// copy), apply insert/replace, re-build into the CoW
	// destination (or split into two).
	leftID, err := pw.AllocPage()
	if err != nil {
		return 0, fmt.Errorf("btree: alloc CoW leaf: %w", err)
	}
	leftBuf, err := pw.CoW(leafID, leftID)
	if err != nil {
		return 0, fmt.Errorf("btree: CoW leaf: %w", err)
	}
	entries, err := readLeafEntriesDeepCopy(leftBuf, cfg, leafID)
	if err != nil {
		return 0, err
	}
	entries = insertOrReplaceLeaf(entries, key, value)

	// Attempt single-page build into leftBuf. LeafBuilder's
	// AddEntry returns false on page-full — no partial mutation is
	// committed at that point per internal/page/leaf_builder.go
	// (the fit check fires before any byte is written).
	b := page.NewLeafBuilder(leftBuf, cfg)
	fits := true
	for _, e := range entries {
		if !b.AddEntry(e) {
			fits = false
			break
		}
	}
	if fits {
		b.Finish()
		if err := pw.FreePage(leafID); err != nil {
			return 0, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
		}
		return ascendNoSplit(pw, cfg, path, leftID)
	}

	// Split required. Per limits.md max-key-size, no single entry
	// alone exceeds half a page, so a count-based midpoint split
	// guarantees each half fits — UNLESS the newly-inserted entry
	// is itself oversize (chunk 4.7 overflow-value lifts that).
	if len(entries) < 2 {
		_ = pw.FreePage(leftID)
		return 0, ErrKeyTooLarge
	}
	mid := len(entries) / 2

	// Re-Reset the builder on leftBuf and emit the left half.
	// The previous (partial) write into leftBuf is overwritten —
	// LeafBuilder writes entries from leafEntryStart forward and
	// Finish zeros the unused middle, so the resulting page is
	// byte-identical to a Builder run on a freshly-zeroed buffer.
	b.Reset(leftBuf, cfg)
	for _, e := range entries[:mid] {
		if !b.AddEntry(e) {
			_ = pw.FreePage(leftID)
			return 0, ErrKeyTooLarge
		}
	}
	b.Finish()

	// Allocate + build right.
	rightID, err := pw.AllocPage()
	if err != nil {
		_ = pw.FreePage(leftID)
		return 0, fmt.Errorf("btree: alloc split-right leaf: %w", err)
	}
	rightBuf, err := pw.AllocSlab(rightID)
	if err != nil {
		_ = pw.FreePage(leftID)
		_ = pw.FreePage(rightID)
		return 0, fmt.Errorf("btree: alloc split-right buf: %w", err)
	}
	rb := page.NewLeafBuilder(rightBuf, cfg)
	for _, e := range entries[mid:] {
		if !rb.AddEntry(e) {
			_ = pw.FreePage(leftID)
			_ = pw.FreePage(rightID)
			return 0, ErrKeyTooLarge
		}
	}
	rb.Finish()

	if err := pw.FreePage(leafID); err != nil {
		return 0, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
	}

	// Separator: shortest S with leftLast < S <= rightFirst.
	sep := page.ShortestSeparator(entries[mid-1].Key, entries[mid].Key)
	return ascendWithSplit(pw, cfg, path, leftID, sep, rightID)
}

// putEmpty allocates a single-leaf root containing just (key,
// value). The genesis path for an empty tree.
func putEmpty(pw PageWriter, cfg page.Config, key, value []byte) (uint64, error) {
	id, err := pw.AllocPage()
	if err != nil {
		return 0, fmt.Errorf("btree: alloc genesis leaf: %w", err)
	}
	buf, err := pw.AllocSlab(id)
	if err != nil {
		return 0, fmt.Errorf("btree: alloc genesis slab: %w", err)
	}
	b := page.NewLeafBuilder(buf, cfg)
	if !b.AddInline(key, value) {
		_ = pw.FreePage(id)
		return 0, ErrKeyTooLarge
	}
	b.Finish()
	return id, nil
}

// readLeafEntriesDeepCopy reads every entry of leaf `buf` into a
// fresh LeafEntry slice with independently-allocated Key and Value
// bytes. Validates the leaf at the boundary and surfaces structural
// faults as ErrCorrupted.
//
// **Deep copy boundary.** LeafReader returns entries whose Key
// aliases the iterator's keyBuf (for compressed delta entries) or
// the page buffer (for restart entries and uncompressed entries);
// Value always aliases the page buffer. The btree's CoW-then-
// re-build flow reuses the SAME buffer as both decode source and
// builder destination (the CoW'd leaf becomes the new leaf's
// scratch). LeafBuilder writes entries from leafEntryStart forward
// and zeros the unused middle on Finish — so any borrowed bytes
// would be clobbered before the builder finished reading them. The
// per-entry bytes.Clone here is the aliasing-safe boundary that
// lets the post-decode entry slice survive arbitrarily many builder
// passes into the source buffer.
func readLeafEntriesDeepCopy(buf []byte, cfg page.Config, pageID uint64) ([]page.LeafEntry, error) {
	r := page.NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%w: leaf %d: %w", ErrCorrupted, pageID, err)
	}
	out := make([]page.LeafEntry, 0, r.Count())
	it := r.IterForReuse(nil, nil, nil)
	for {
		e, ok := it.Next()
		if !ok {
			break
		}
		e.Key = bytes.Clone(e.Key)
		e.Value = bytes.Clone(e.Value)
		out = append(out, e)
	}
	return out, nil
}

// insertOrReplaceLeaf finds the position of `key` in the sorted-
// by-key entries slice and either replaces the entry's Value (key
// exists) or inserts a new inline entry at the right position.
// Returns the new slice. The original may be shared with the
// caller — do not retain. The replace path resets Flags so an old
// overflow entry's bookkeeping (OverflowPage / TotalLen) doesn't
// survive a same-key inline update.
func insertOrReplaceLeaf(entries []page.LeafEntry, key, value []byte) []page.LeafEntry {
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		cmp := bytes.Compare(entries[mid].Key, key)
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp > 0:
			hi = mid
		default:
			entries[mid] = page.LeafEntry{Key: key, Value: value}
			return entries
		}
	}
	entries = append(entries, page.LeafEntry{})
	copy(entries[lo+1:], entries[lo:])
	entries[lo] = page.LeafEntry{Key: key, Value: value}
	return entries
}

// ascendNoSplit walks the path in reverse, CoWing each branch and
// updating the child pointer at descentIdx from the old leafID to
// newChildID (which propagates up: each level's CoW produces a new
// pageID that becomes the next level's newChildID).
//
// For the no-split path, no separator is inserted — only the child
// pointer at descentIdx is overwritten.
//
// Returns the new rootID. For an empty path (root is the leaf
// itself), returns newChildID directly.
func ascendNoSplit(pw PageWriter, cfg page.Config, path []pathFrame, newChildID uint64) (uint64, error) {
	cur := newChildID
	// Walk from leaf-side to root.
	for i := len(path) - 1; i >= 0; i-- {
		f := path[i]
		newBranchID, err := pw.AllocPage()
		if err != nil {
			return 0, fmt.Errorf("btree: alloc branch CoW: %w", err)
		}
		buf, err := pw.CoW(f.pageID, newBranchID)
		if err != nil {
			return 0, fmt.Errorf("btree: CoW branch %d: %w", f.pageID, err)
		}
		// Re-write the child pointer at descentIdx.
		if err := branchReplaceChild(buf, cfg, f.descentIdx, cur); err != nil {
			return 0, fmt.Errorf("btree: replace child in branch %d: %w", f.pageID, err)
		}
		if err := pw.FreePage(f.pageID); err != nil {
			return 0, fmt.Errorf("btree: free old branch %d: %w", f.pageID, err)
		}
		cur = newBranchID
	}
	return cur, nil
}

// ascendWithSplit walks the path in reverse, CoWing each branch
// and propagating a (separator, rightChildID) pair upward.
//
// At each level, the existing branch is CoW'd. The child pointer
// at descentIdx is updated from the old leaf/branch ID to leftID
// (the existing-tree side of the split). The (sep, rightID) pair
// is inserted at descentIdx+1.
//
// If the resulting branch overflows, the branch splits in two:
// the lower half stays as the CoW'd page, the upper half goes to
// a freshly allocated branch, and a new separator + rightID is
// computed for the NEXT level up.
//
// If the path is empty (the root itself was a leaf that split), a
// new root branch is allocated with two children: leftID and
// rightID, separator sep.
func ascendWithSplit(pw PageWriter, cfg page.Config, path []pathFrame, leftID uint64, sep []byte, rightID uint64) (uint64, error) {
	for i := len(path) - 1; i >= 0; i-- {
		f := path[i]
		newBranchID, err := pw.AllocPage()
		if err != nil {
			return 0, fmt.Errorf("btree: alloc branch CoW (split): %w", err)
		}
		buf, err := pw.CoW(f.pageID, newBranchID)
		if err != nil {
			return 0, fmt.Errorf("btree: CoW branch %d (split): %w", f.pageID, err)
		}
		// Decode current branch, replace child at descentIdx
		// with leftID, insert (sep, rightID) at descentIdx+1.
		//
		// Deep-copy cell Keys: DecodeBranch returns Keys that
		// borrow from `buf`; we're about to re-encode INTO `buf`
		// (CoW destination doubles as source), and EncodeBranch
		// clears the buffer before writing. Without the deep
		// copy, the borrowed Keys go all-zero between decode and
		// encode-read. Same correctness boundary as
		// readLeafEntriesDeepCopy.
		leftmost, cells := page.DecodeBranch(buf, cfg)
		for i := range cells {
			cells[i].Key = bytes.Clone(cells[i].Key)
		}
		// Build newCells = cells[:descentIdx] || (sep, rightID) ||
		// cells[descentIdx:], then explicitly rewrite the
		// already-updated cell or the leftmost. Doing the Child
		// update on newCells (rather than on the source `cells`
		// slice) keeps the mutation independent of newCells's
		// backing-array aliasing — a future refactor that copies
		// cells field-by-field instead of via append won't
		// silently drop the Child update.
		newCells := make([]page.BranchCell, 0, len(cells)+1)
		newCells = append(newCells, cells[:f.descentIdx]...)
		newCells = append(newCells, page.BranchCell{Key: sep, Child: rightID})
		newCells = append(newCells, cells[f.descentIdx:]...)
		if f.descentIdx == 0 {
			leftmost = leftID
		} else {
			newCells[f.descentIdx-1].Child = leftID
		}

		// Try to encode in one branch.
		if err := page.EncodeBranch(buf, cfg, leftmost, newCells); err == nil {
			// Fits. Retire old branch; this CoW'd branch becomes
			// the child of the next-up level — switch to the
			// no-split ascend path for the remaining frames.
			if err := pw.FreePage(f.pageID); err != nil {
				return 0, fmt.Errorf("btree: free old branch %d: %w", f.pageID, err)
			}
			return ascendNoSplit(pw, cfg, path[:i], newBranchID)
		}

		// Branch split required.
		if len(newCells) < 2 {
			// Single oversize cell — unreachable under the
			// limits.md §Maximum Key Size bound (per-key cap
			// ≤ ~PageSize/2 guarantees ≥2 cells fit any
			// branch). Defense in depth against a future
			// max-key-size relaxation.
			_ = pw.FreePage(newBranchID)
			return 0, ErrKeyTooLarge
		}
		mid := len(newCells) / 2
		// In a branch split, the MIDDLE cell's Key becomes the
		// separator propagated to the next level up; the
		// middle cell's Child becomes the leftmost child of the
		// new right branch. The left branch keeps cells [0:mid]
		// and its leftmost is unchanged. The right branch gets
		// cells [mid+1:] with leftmost = newCells[mid].Child.
		nextSep := newCells[mid].Key
		nextRightLeftmost := newCells[mid].Child
		leftCells := newCells[:mid]
		rightCells := newCells[mid+1:]

		newRightID, err := pw.AllocPage()
		if err != nil {
			_ = pw.FreePage(newBranchID)
			return 0, fmt.Errorf("btree: alloc split-right branch: %w", err)
		}
		newRightBuf, err := pw.AllocSlab(newRightID)
		if err != nil {
			_ = pw.FreePage(newBranchID)
			_ = pw.FreePage(newRightID)
			return 0, fmt.Errorf("btree: alloc split-right branch slab: %w", err)
		}
		if err := page.EncodeBranch(buf, cfg, leftmost, leftCells); err != nil {
			_ = pw.FreePage(newBranchID)
			_ = pw.FreePage(newRightID)
			return 0, fmt.Errorf("btree: encode left branch split: %w", err)
		}
		if err := page.EncodeBranch(newRightBuf, cfg, nextRightLeftmost, rightCells); err != nil {
			_ = pw.FreePage(newBranchID)
			_ = pw.FreePage(newRightID)
			return 0, fmt.Errorf("btree: encode right branch split: %w", err)
		}
		if err := pw.FreePage(f.pageID); err != nil {
			return 0, fmt.Errorf("btree: free old branch %d: %w", f.pageID, err)
		}
		// Loop up with the new (sep, right) pair.
		leftID = newBranchID
		sep = nextSep
		rightID = newRightID
	}

	// Path exhausted. Root grew: allocate a new root branch with
	// leftID as leftmost and one cell (sep, rightID).
	newRootID, err := pw.AllocPage()
	if err != nil {
		return 0, fmt.Errorf("btree: alloc new root branch: %w", err)
	}
	newRootBuf, err := pw.AllocSlab(newRootID)
	if err != nil {
		_ = pw.FreePage(newRootID)
		return 0, fmt.Errorf("btree: alloc new root slab: %w", err)
	}
	cells := []page.BranchCell{{Key: sep, Child: rightID}}
	if err := page.EncodeBranch(newRootBuf, cfg, leftID, cells); err != nil {
		_ = pw.FreePage(newRootID)
		return 0, fmt.Errorf("btree: encode new root branch: %w", err)
	}
	return newRootID, nil
}

// branchReplaceChild updates the child pointer at descent index i:
//   - i == 0 → leftmost (Ptr[0])
//   - 0 < i ≤ N → cell[i-1].Child
//
// In-place rewrite — the cell directory + key data stay put; only
// the 8-byte child pointer at the cell's tail is rewritten.
func branchReplaceChild(buf []byte, cfg page.Config, i uint16, child uint64) error {
	n := page.BranchCellCount(buf)
	if i == 0 {
		page.SetBranchLeftmostChild(buf, child)
		return nil
	}
	if i > n {
		return fmt.Errorf("btree: branchReplaceChild i=%d > count=%d", i, n)
	}
	page.SetBranchCellChild(buf, cfg, i-1, child)
	return nil
}

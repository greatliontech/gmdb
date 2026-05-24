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
func Put(pw PageWriter, cfg page.Config, rootID uint64, restartInterval uint16, key, value []byte) (uint64, error) {
	if restartInterval == 0 {
		return 0, fmt.Errorf("btree: Put RestartInterval must be > 0")
	}
	if rootID == 0 {
		return putEmpty(pw, cfg, restartInterval, key, value)
	}

	// Phase 1: descend, recording the path.
	path := make([]pathFrame, 0, 8)
	cur := rootID
	for depth := 0; depth <= MaxTreeDepth; depth++ {
		buf := pw.Page(cur)
		typ, _, _, _ := page.ReadHeader(buf)
		if typ == page.TypeLeaf {
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

	// Phase 2: leaf mutation. CoW the leaf, decode entries,
	// insert/update, re-encode (or split).
	leftID, err := pw.AllocPage()
	if err != nil {
		return 0, fmt.Errorf("btree: alloc CoW leaf: %w", err)
	}
	leftBuf, err := pw.CoW(leafID, leftID)
	if err != nil {
		return 0, fmt.Errorf("btree: CoW leaf: %w", err)
	}
	entries, err := page.DecodeLeaf(leftBuf, cfg)
	if err != nil {
		return 0, fmt.Errorf("%w: decode leaf %d for insert: %w", ErrCorrupted, leafID, err)
	}
	interval := page.LeafRestartInterval(leftBuf)
	if interval == 0 {
		interval = restartInterval
	}
	enc := leafEntriesAsEncoded(entries)
	enc = insertOrReplace(enc, key, value)

	// Attempt single-leaf encode. EncodeLeaf returns errors for
	// either oversize OR malformed input (unsorted keys, unknown
	// flags). The fit-ahead probe distinguishes: if the encoded
	// size exceeds the page's content area, the split path is
	// the right response; otherwise the error is a real defect
	// (caller bug or btree-internal logic error) and must
	// surface.
	predicted := page.LeafEncodedSize(cfg, interval, enc)
	if predicted <= cfg.ContentEnd() {
		if err := page.EncodeLeaf(leftBuf, cfg, interval, enc); err != nil {
			return 0, fmt.Errorf("btree: encode leaf rejected after fit-ahead check passed: %w", err)
		}
		// Fits. Retire the old leaf, ascend the path swapping
		// (oldLeafID → leftID) at the descent index of each
		// branch.
		if err := pw.FreePage(leafID); err != nil {
			return 0, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
		}
		return ascendNoSplit(pw, cfg, path, leftID)
	}

	// Split required. Pick a midpoint by entry count (chunk 4.4
	// scope — byte-balanced split lands later if profiling shows
	// imbalance hurts). Per limits.md max-key-size, no single
	// entry alone exceeds half a page, so a count-based split
	// guarantees each half fits — UNLESS the newly-inserted
	// entry is itself oversize (chunk 4.7 overflow-value lifts
	// that). Pre-check each half's predicted size; if either
	// can't fit even with the rest peeled off, surface
	// ErrKeyTooLarge.
	if len(enc) < 2 {
		_ = pw.FreePage(leftID)
		return 0, ErrKeyTooLarge
	}
	mid := len(enc) / 2
	leftEntries := enc[:mid]
	rightEntries := enc[mid:]
	if page.LeafEncodedSize(cfg, interval, leftEntries) > cfg.ContentEnd() ||
		page.LeafEncodedSize(cfg, interval, rightEntries) > cfg.ContentEnd() {
		// A half with a single oversize entry: the value is too
		// large for leaf storage even after split. Free the CoW
		// destination so it returns to the loose pool.
		_ = pw.FreePage(leftID)
		return 0, ErrKeyTooLarge
	}

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
	if err := page.EncodeLeaf(leftBuf, cfg, interval, leftEntries); err != nil {
		_ = pw.FreePage(leftID)
		_ = pw.FreePage(rightID)
		return 0, fmt.Errorf("btree: encode left split: %w", err)
	}
	if err := page.EncodeLeaf(rightBuf, cfg, interval, rightEntries); err != nil {
		_ = pw.FreePage(leftID)
		_ = pw.FreePage(rightID)
		return 0, fmt.Errorf("btree: encode right split: %w", err)
	}
	if err := pw.FreePage(leafID); err != nil {
		return 0, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
	}

	// Separator: shortest S with leftLast < S <= rightFirst.
	sep := page.ShortestSeparator(leftEntries[len(leftEntries)-1].Key, rightEntries[0].Key)
	return ascendWithSplit(pw, cfg, path, leftID, sep, rightID)
}

// putEmpty allocates a single-leaf root containing just (key,
// value). The genesis path for an empty tree.
func putEmpty(pw PageWriter, cfg page.Config, restartInterval uint16, key, value []byte) (uint64, error) {
	id, err := pw.AllocPage()
	if err != nil {
		return 0, fmt.Errorf("btree: alloc genesis leaf: %w", err)
	}
	buf, err := pw.AllocSlab(id)
	if err != nil {
		return 0, fmt.Errorf("btree: alloc genesis slab: %w", err)
	}
	entries := []page.EncodedEntry{{Key: key, Value: value}}
	if err := page.EncodeLeaf(buf, cfg, restartInterval, entries); err != nil {
		_ = pw.FreePage(id)
		// Single-entry encode failure: the only reachable cause
		// is "too big to fit a page" — ordering, flag, and
		// overflow-with-value validations all trivially pass for
		// a single inline entry constructed from (key, value).
		// Surface as ErrKeyTooLarge (same class as the split-
		// time check). When EncodeLeaf grows a page.ErrOversize
		// sentinel, swap the string match for errors.Is.
		return 0, ErrKeyTooLarge
	}
	return id, nil
}

// leafEntriesAsEncoded converts the LeafEntry slice (post-decode
// form) back into the EncodedEntry shape EncodeLeaf consumes.
// Lossless because LeafEntry already carries every field
// EncodedEntry has.
//
// **Deep-copies Key and Value.** DecodeLeaf returns slices that
// borrow from the input page buffer (zero-copy fast path); the
// btree's CoW-then-re-encode flow uses the SAME buffer as both
// input and output (the CoW'd leaf becomes the new leaf). When
// EncodeLeaf runs `clear(buf)` at the top of its write phase, the
// borrowed Key/Value slices in `entries` would become all-zero.
// The deep copy here is the correctness boundary that lets the
// re-encode pass read the original bytes from a fresh allocation
// instead of the about-to-be-cleared page bytes.
func leafEntriesAsEncoded(entries []page.LeafEntry) []page.EncodedEntry {
	out := make([]page.EncodedEntry, len(entries))
	for i, e := range entries {
		out[i] = page.EncodedEntry{
			Flags:        e.Flags,
			Key:          bytes.Clone(e.Key),
			Value:        bytes.Clone(e.Value),
			OverflowPage: e.OverflowPage,
			TotalLen:     e.TotalLen,
		}
	}
	return out
}

// insertOrReplace finds the position of `key` in the sorted-by-key
// entries slice and either replaces the entry's value (key exists)
// or inserts a new entry at the right position. Returns the new
// slice. The original may be shared with the caller — do not
// retain.
func insertOrReplace(entries []page.EncodedEntry, key, value []byte) []page.EncodedEntry {
	// Binary search for the insertion index.
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
			// Replace.
			entries[mid] = page.EncodedEntry{Key: key, Value: value}
			return entries
		}
	}
	// Insert at position lo. Grow the slice by one and shift.
	entries = append(entries, page.EncodedEntry{})
	copy(entries[lo+1:], entries[lo:])
	entries[lo] = page.EncodedEntry{Key: key, Value: value}
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
		// leafEntriesAsEncoded.
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
	// Cell at index i-1: read directory entry to find offset +
	// key length, then write the child pointer immediately
	// after the key bytes. Reuse page.BranchCellAt — it returns
	// (Key, Child); we only need the offset arithmetic so do it
	// directly via the page-internal directory layout.
	page.SetBranchCellChild(buf, cfg, i-1, child)
	return nil
}

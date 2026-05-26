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
// rootID), or (b) freed (via FreePage for single pages, FreeRun
// for overflow chains) before Put returns. The pager's slab
// manages the byte buffers; btree never owns them past the Put
// call.
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

	// AllocContiguous returns the first page ID of a fresh
	// contiguous run of n pages. Used for overflow-chain
	// allocation per page-formats.md §Overflow Page (followers
	// have no header and must be addressable as firstID+1,
	// firstID+2, ..., firstID+n-1). The pager's bitmap
	// contiguous-run search backs this (free-space.md
	// §Contiguous-run search).
	AllocContiguous(n uint32) (uint64, error)

	// AllocSlabRun returns the slab buffers for the n pages of a
	// run previously allocated via AllocContiguous. pages[i] is
	// the buffer for firstID + uint64(i). All buffers are fresh
	// zero-filled page-sized slices.
	AllocSlabRun(firstID uint64, n uint32) (pages [][]byte, err error)

	// FreeRun retires a contiguous run of n pages starting at
	// firstID. Each id [firstID, firstID+n) is treated like an
	// individual FreePage by the pager's bookkeeping (loose
	// within this tx; RPL'd at commit if prior-tx).
	FreeRun(firstID uint64, n uint32) error
}

// ErrKeyTooLarge is returned by Put when the key is too large even
// for an overflow-reference leaf entry (key + overflow-ref header
// exceeds a single-entry leaf page's capacity). Per limits.md
// §Maximum Key Size the cap is ~(PageSize-40)/2; the chunk-7
// tree-depth bound lifts this further. Values exceeding inline
// capacity are automatically promoted to an overflow chain — the
// sentinel fires only on oversize KEYS, never on oversize values.
var ErrKeyTooLarge = errors.New("btree: key too large for overflow-reference leaf entry")

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
		buf, err := pw.Page(cur)
		if err != nil {
			return 0, err
		}
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

	// Determine if the new value needs overflow promotion.
	// Allocate the chain BEFORE the leaf re-build so a chain-alloc
	// failure rolls back via FreeRun without leaving the leaf in a
	// half-written state.
	newEntry, err := buildPutEntry(pw, cfg, key, value)
	if err != nil {
		_ = pw.FreePage(leftID)
		return 0, err
	}

	// insertOrReplaceLeaf returns the displaced entry (zero-valued
	// on insert) so we can free its overflow chain after the new
	// leaf is committed.
	var displaced page.LeafEntry
	entries, displaced = insertOrReplaceLeaf(entries, newEntry)

	// Helper: rollback the freshly-allocated new chain (if any) on
	// failure paths below. Mirrors the AllocPage→CoW→mutate→FreePage
	// ordering for chains.
	rollbackNewChain := func() {
		if newEntry.IsOverflow() {
			runLen := page.OverflowRunLength(cfg, newEntry.TotalLen)
			_ = pw.FreeRun(newEntry.OverflowPage, runLen)
		}
	}

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
		// Post-build cleanup ordering: free the displaced chain
		// FIRST while the OLD leaf still references it (so a
		// chain-free fault can't observably orphan the chain —
		// the entry is still reachable from the old leaf, which
		// hasn't been retired yet). Then free the old leaf. On
		// either failure roll back the new state (leftID + the
		// new chain if it was allocated) so the chunk's
		// "any pages allocated during this Put are freed on
		// error" contract holds.
		if err := freeOverflowChainIfPresent(pw, cfg, displaced); err != nil {
			_ = pw.FreePage(leftID)
			rollbackNewChain()
			return 0, err
		}
		if err := pw.FreePage(leafID); err != nil {
			_ = pw.FreePage(leftID)
			rollbackNewChain()
			return 0, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
		}
		return ascendNoSplit(pw, cfg, path, leftID)
	}

	// Split required. Since the new entry has been promoted to
	// overflow when it'd otherwise exceed single-page capacity,
	// the only remaining unfit case is "multi-entry leaf that
	// overflows on one entry's growth" — count-balanced midpoint
	// split. With limits.md key-size bounds + overflow promotion,
	// `len(entries) < 2` is unreachable from valid input: an
	// overflow-promoted single entry fits trivially, and an inline
	// single entry below the overflow threshold fits trivially.
	// Guard remains as defense in depth.
	if len(entries) < 2 {
		_ = pw.FreePage(leftID)
		rollbackNewChain()
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
			rollbackNewChain()
			return 0, ErrKeyTooLarge
		}
	}
	b.Finish()

	// Allocate + build right.
	rightID, err := pw.AllocPage()
	if err != nil {
		_ = pw.FreePage(leftID)
		rollbackNewChain()
		return 0, fmt.Errorf("btree: alloc split-right leaf: %w", err)
	}
	rightBuf, err := pw.AllocSlab(rightID)
	if err != nil {
		_ = pw.FreePage(leftID)
		_ = pw.FreePage(rightID)
		rollbackNewChain()
		return 0, fmt.Errorf("btree: alloc split-right buf: %w", err)
	}
	rb := page.NewLeafBuilder(rightBuf, cfg)
	for _, e := range entries[mid:] {
		if !rb.AddEntry(e) {
			_ = pw.FreePage(leftID)
			_ = pw.FreePage(rightID)
			rollbackNewChain()
			return 0, ErrKeyTooLarge
		}
	}
	rb.Finish()

	// Post-build cleanup ordering: free the displaced chain first
	// (still reachable via the not-yet-retired old leaf), then
	// the old leaf. On either failure roll back leftID + rightID
	// + the new chain.
	if err := freeOverflowChainIfPresent(pw, cfg, displaced); err != nil {
		_ = pw.FreePage(leftID)
		_ = pw.FreePage(rightID)
		rollbackNewChain()
		return 0, err
	}
	if err := pw.FreePage(leafID); err != nil {
		_ = pw.FreePage(leftID)
		_ = pw.FreePage(rightID)
		rollbackNewChain()
		return 0, fmt.Errorf("btree: free old leaf %d: %w", leafID, err)
	}

	// Separator: shortest S with leftLast < S <= rightFirst.
	sep := page.ShortestSeparator(entries[mid-1].Key, entries[mid].Key)
	return ascendWithSplit(pw, cfg, path, leftID, sep, rightID)
}

// buildPutEntry constructs the LeafEntry for a Put: an inline entry
// when the value fits, or an overflow-referencing entry (with the
// chain freshly allocated + encoded) when the inline form exceeds
// single-entry leaf capacity. Returns ErrKeyTooLarge if even the
// overflow-reference form doesn't fit (key alone is too large).
func buildPutEntry(pw PageWriter, cfg page.Config, key, value []byte) (page.LeafEntry, error) {
	if !needsOverflow(cfg, key, value) {
		return page.LeafEntry{Key: key, Value: value}, nil
	}
	if !overflowRefFitsLeaf(cfg, key) {
		return page.LeafEntry{}, ErrKeyTooLarge
	}
	return writeOverflowChain(pw, cfg, key, value)
}

// putEmpty allocates a single-leaf root containing just (key,
// value). The genesis path for an empty tree. Routes through
// buildPutEntry so a large value gets overflow-promoted the same
// way as a non-empty-tree Put.
func putEmpty(pw PageWriter, cfg page.Config, key, value []byte) (uint64, error) {
	newEntry, err := buildPutEntry(pw, cfg, key, value)
	if err != nil {
		return 0, err
	}
	id, err := pw.AllocPage()
	if err != nil {
		if newEntry.IsOverflow() {
			_ = pw.FreeRun(newEntry.OverflowPage, page.OverflowRunLength(cfg, newEntry.TotalLen))
		}
		return 0, fmt.Errorf("btree: alloc genesis leaf: %w", err)
	}
	buf, err := pw.AllocSlab(id)
	if err != nil {
		_ = pw.FreePage(id)
		if newEntry.IsOverflow() {
			_ = pw.FreeRun(newEntry.OverflowPage, page.OverflowRunLength(cfg, newEntry.TotalLen))
		}
		return 0, fmt.Errorf("btree: alloc genesis slab: %w", err)
	}
	b := page.NewLeafBuilder(buf, cfg)
	if !b.AddEntry(newEntry) {
		_ = pw.FreePage(id)
		if newEntry.IsOverflow() {
			_ = pw.FreeRun(newEntry.OverflowPage, page.OverflowRunLength(cfg, newEntry.TotalLen))
		}
		// Genesis single entry must fit by construction (overflow
		// promotion sized it down to a small reference); reaching
		// this branch implies an oversize key past
		// overflowRefFitsLeaf's check — keep ErrKeyTooLarge as the
		// surface.
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

// insertOrReplaceLeaf finds the position of `newEntry.Key` in the
// sorted-by-key entries slice and either replaces the entry there
// (key exists) or inserts newEntry at the correct sorted position.
// Returns the modified slice plus the DISPLACED entry — non-zero
// on replace (the LeafEntry that was at the replaced slot, used
// by the caller to free any owned overflow chain) and zero-valued
// on insert.
//
// The original entries slice may be shared with the caller — do
// not retain. The replace path overwrites the slot wholesale so a
// stale Flags / OverflowPage / TotalLen from the old entry doesn't
// survive into the rebuilt leaf; the displaced entry is returned
// separately so the chain-free path runs on the OLD overflow info.
func insertOrReplaceLeaf(entries []page.LeafEntry, newEntry page.LeafEntry) ([]page.LeafEntry, page.LeafEntry) {
	lo, hi := 0, len(entries)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		cmp := bytes.Compare(entries[mid].Key, newEntry.Key)
		switch {
		case cmp < 0:
			lo = mid + 1
		case cmp > 0:
			hi = mid
		default:
			displaced := entries[mid]
			entries[mid] = newEntry
			return entries, displaced
		}
	}
	entries = append(entries, page.LeafEntry{})
	copy(entries[lo+1:], entries[lo:])
	entries[lo] = newEntry
	return entries, page.LeafEntry{}
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

package btree

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// ErrNotFound is returned by Delete when the key is absent.
// api-surface.md §Invariants pins ErrNotFound at the public
// Keyspace.Delete / SetKeyspace.Delete / SetKeyspace.DeleteValue
// surface, so the btree's strict variant is propagated, not
// coerced to a silent no-op.
var ErrNotFound = errors.New("btree: key not found")

// MaxMergeThreshold caps the per-call MergeThreshold percentage per
// api-surface.md Options (range 1-50). Above 50%, redistribute thrash
// under alternating insert/delete becomes pathological — two siblings
// each hovering just below 50% would never have room to merge, and
// each delete would force a redistribute.
const MaxMergeThreshold uint8 = 50

// DefaultMergeThreshold mirrors api-surface.md Options.MergeThreshold
// default (25%). Re-exported here so chunk-4 tests can run without
// pulling the chunk-5 Options surface.
const DefaultMergeThreshold uint8 = 25

// Delete removes key from the tree rooted at rootID. Returns the new
// rootID — the chunk-5 keyspace caller records this in the keyspace
// descriptor and propagates the descriptor update via CoW to the
// meta page.
//
// Mutations:
//   - rootID == 0 (empty tree) → returns (0, ErrNotFound).
//   - Key absent → returns (rootID, ErrNotFound); no pages are
//     allocated or freed.
//   - Key present → CoWs the leaf (and every branch on the path); if
//     a touched page drops below mergeThreshold% of ContentEnd, merges
//     with or redistributes against a sibling; cascades up; if the
//     root branch shrinks to a single child, collapses the root to
//     that child; if the only leaf entry was the deleted key the
//     tree becomes empty (returned rootID = 0).
//
// On error: any pages already allocated are freed via FreePage (they
// become loose pages for re-use within this tx). The returned rootID
// is meaningful only when err is nil OR err is ErrNotFound; on
// ErrNotFound the returned ID equals the input rootID and no pages
// were mutated.
func Delete(pw PageWriter, cfg page.Config, rootID uint64, mergeThreshold uint8, key []byte) (uint64, error) {
	if mergeThreshold == 0 || mergeThreshold > MaxMergeThreshold {
		return 0, fmt.Errorf("btree: Delete MergeThreshold %d outside (0, %d]", mergeThreshold, MaxMergeThreshold)
	}
	if rootID == 0 {
		return 0, ErrNotFound
	}
	newRootID, _, found, err := deleteFrom(pw, cfg, mergeThreshold, rootID, key)
	if err != nil {
		return 0, err
	}
	if !found {
		return rootID, ErrNotFound
	}
	if newRootID == 0 {
		return 0, nil
	}
	// Root collapse: a 0-cell root branch is a degenerate passthrough
	// (1 child, 0 separators) — its sole leftmost child becomes the
	// new root. Per delete this iterates at most once (each cascade
	// level loses at most one cell), but the loop is defensive
	// against future code that could chain multiple collapses.
	//
	// Safety against read-after-free: `child` is captured from buf
	// BEFORE the FreePage call, and the loop variable `newRootID`
	// is reassigned to that fresh `child` id at the end of each
	// iteration. The freed id is never re-read; subsequent pw.Page
	// calls target the new root from the previous iteration.
	for {
		buf, err := pw.Page(newRootID)
		if err != nil {
			return 0, err
		}
		typ, _, count, _ := page.ReadHeader(buf)
		if typ != page.TypeBranch || count != 0 {
			break
		}
		child := page.BranchLeftmostChild(buf)
		if child == 0 {
			return 0, fmt.Errorf("%w: empty root branch %d has null leftmost child", ErrCorrupted, newRootID)
		}
		if err := pw.FreePage(newRootID); err != nil {
			return 0, fmt.Errorf("btree: free collapsed root branch %d: %w", newRootID, err)
		}
		newRootID = child
	}
	return newRootID, nil
}

// deleteFrom recursively deletes key from the subtree rooted at
// pageID. Returns:
//   - newID: the new pageID after CoW; 0 iff the subtree became
//     fully empty (only happens when pageID is a leaf with one entry
//     that was the deleted key, or a degenerate branch chain whose
//     terminal leaf hit that case).
//   - underflow: newID is non-zero AND its post-mutation encoded fill
//     is < mergeThreshold% of ContentEnd. The recursive parent acts
//     on this; the top-level Delete ignores it (root has no
//     siblings).
//   - found: key was present and deleted.
func deleteFrom(pw PageWriter, cfg page.Config, mergeThreshold uint8, pageID uint64, key []byte) (newID uint64, underflow, found bool, err error) {
	buf, err := pw.Page(pageID)
	if err != nil {
		return 0, false, false, err
	}
	typ, _, _, _ := page.ReadHeader(buf)
	switch {
	case page.IsLeafType(typ):
		return deleteFromLeaf(pw, cfg, mergeThreshold, pageID, buf, key)
	case typ == page.TypeBranch:
		return deleteFromBranch(pw, cfg, mergeThreshold, pageID, buf, key)
	default:
		return 0, false, false, fmt.Errorf("%w: page %d unexpected type %d during Delete descent", ErrCorrupted, pageID, typ)
	}
}

func deleteFromLeaf(pw PageWriter, cfg page.Config, mergeThreshold uint8, pageID uint64, srcBuf []byte, key []byte) (uint64, bool, bool, error) {
	entries, err := readLeafEntriesDeepCopy(srcBuf, cfg, pageID)
	if err != nil {
		return 0, false, false, err
	}
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
			// Capture the removed entry's overflow chain (if any)
			// BEFORE splicing it out of the slice — the chain is
			// freed AFTER the new leaf is committed so a transient
			// CoW failure can't orphan the chain (the entry is
			// still reachable from the original leaf at that
			// point).
			removed := entries[mid]
			entries = append(entries[:mid], entries[mid+1:]...)
			return rebuildLeafAfterDelete(pw, cfg, mergeThreshold, pageID, entries, removed)
		}
	}
	return pageID, false, false, nil
}

// rebuildLeafAfterDelete handles the post-removal encode for a leaf:
// empty result → return newID=0 (parent removes the slot); otherwise
// CoW the leaf and re-build via LeafBuilder. After the leaf-level
// CoW lands, frees the removed entry's overflow chain (if any) per
// the Invariant: Delete of an overflow entry frees its chain in the
// same write tx.
func rebuildLeafAfterDelete(pw PageWriter, cfg page.Config, mergeThreshold uint8, pageID uint64, entries []page.LeafEntry, removed page.LeafEntry) (uint64, bool, bool, error) {
	if len(entries) == 0 {
		// Last entry removed → signal "subtree gone" via newID=0.
		// The recursive parent removes the corresponding child slot.
		// No CoW allocation needed since there's nothing to encode.
		// Order: chain-free first (still reachable via the
		// not-yet-retired leaf), then leaf-free.
		if err := freeOverflowChainIfPresent(pw, cfg, removed); err != nil {
			return 0, false, false, err
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, false, false, fmt.Errorf("btree: free emptied leaf %d: %w", pageID, err)
		}
		return 0, false, true, nil
	}
	newID, err := pw.AllocPage()
	if err != nil {
		return 0, false, false, fmt.Errorf("btree: alloc CoW leaf for delete: %w", err)
	}
	newBuf, err := pw.CoW(pageID, newID)
	if err != nil {
		return 0, false, false, fmt.Errorf("btree: CoW leaf %d for delete: %w", pageID, err)
	}
	b := page.NewLeafBuilder(newBuf, cfg)
	for _, e := range entries {
		if !b.AddEntry(e) {
			// Deletion strictly shrinks the entry set — a build
			// that doesn't fit after removing an entry would
			// have failed at the original encode too. Treat as
			// structural corruption.
			return 0, false, false, fmt.Errorf("%w: leaf %d re-build after delete overflowed page", ErrCorrupted, pageID)
		}
	}
	b.Finish()
	// Post-build cleanup ordering: chain-free first (still
	// reachable via the OLD leaf which has not been retired yet),
	// then the OLD leaf. On either failure roll back the newly-
	// allocated leaf so the chunk's "any pages allocated during
	// this Delete are freed on error" contract holds.
	if err := freeOverflowChainIfPresent(pw, cfg, removed); err != nil {
		_ = pw.FreePage(newID)
		return 0, false, false, err
	}
	if err := pw.FreePage(pageID); err != nil {
		_ = pw.FreePage(newID)
		return 0, false, false, fmt.Errorf("btree: free old leaf %d: %w", pageID, err)
	}
	return newID, leafUnderflow(newBuf, cfg, mergeThreshold), true, nil
}

// leafUnderflow reports whether the leaf at `buf` falls strictly
// below mergeThreshold% of ContentEnd. The strict inequality matches
// "below the threshold" in api-surface.md (at exactly threshold the
// page is large enough, no merge).
func leafUnderflow(buf []byte, cfg page.Config, mergeThreshold uint8) bool {
	r := page.NewLeafReader(buf, cfg)
	used := cfg.ContentEnd() - r.FreeSpace()
	return used*100 < int(mergeThreshold)*cfg.ContentEnd()
}

// branchUnderflow is the analogue for a branch page.
func branchUnderflow(cfg page.Config, cells []page.BranchCell, mergeThreshold uint8) bool {
	size := page.BranchEncodedSize(cfg, cells)
	return size*100 < int(mergeThreshold)*cfg.ContentEnd()
}

func deleteFromBranch(pw PageWriter, cfg page.Config, mergeThreshold uint8, pageID uint64, srcBuf []byte, key []byte) (uint64, bool, bool, error) {
	descentIdx := page.BranchSearch(srcBuf, cfg, key)
	childID := page.BranchChildAt(srcBuf, cfg, descentIdx)
	if childID == 0 {
		return 0, false, false, fmt.Errorf("%w: null child in branch %d at descent %d", ErrCorrupted, pageID, descentIdx)
	}
	newChildID, childUnderflow, found, err := deleteFrom(pw, cfg, mergeThreshold, childID, key)
	if err != nil {
		return 0, false, false, err
	}
	if !found {
		return pageID, false, false, nil
	}
	newID, underflow, err := patchBranchAfterChildDelete(pw, cfg, mergeThreshold, pageID, descentIdx, newChildID, childUnderflow)
	if err != nil {
		return 0, false, false, err
	}
	return newID, underflow, true, nil
}

// patchBranchAfterChildDelete CoWs the branch at pageID and applies
// the single-child-replace result returned by a child's delete-side
// recursion. Three cases per the original deleteFromBranch shape:
//
//   - Case A: newChildID == 0 (child subtree fully vanished). Remove
//     the child-position and its associated cell from this branch.
//     If the last child was removed, return newID=0 to cascade up.
//   - Case B: newChildID != 0 && !childUnderflow. Patch the child
//     pointer in place.
//   - Case C: newChildID != 0 && childUnderflow. Merge with or
//     redistribute against an adjacent sibling.
//
// Returns (newID, underflow, err). Used by both deleteFromBranch
// (single-key delete) and deleteRangeFromBranch's single-child
// overlap case (chunk-5.7 DeleteRange).
func patchBranchAfterChildDelete(pw PageWriter, cfg page.Config, mergeThreshold uint8, pageID uint64, descentIdx uint16, newChildID uint64, childUnderflow bool) (uint64, bool, error) {
	// CoW the parent for mutation and decode its (leftmost, cells)
	// form. Deep-clone every cell Key — DecodeBranch returns Keys
	// that borrow from parentBuf and EncodeBranch will clear that
	// buffer before re-emit, the same aliasing boundary
	// ascendWithSplit handles for Put.
	newBranchID, err := pw.AllocPage()
	if err != nil {
		return 0, false, fmt.Errorf("btree: alloc CoW branch for delete: %w", err)
	}
	parentBuf, err := pw.CoW(pageID, newBranchID)
	if err != nil {
		return 0, false, fmt.Errorf("btree: CoW branch %d for delete: %w", pageID, err)
	}
	leftmost, cells := page.DecodeBranch(parentBuf, cfg)
	for i := range cells {
		cells[i].Key = bytes.Clone(cells[i].Key)
	}

	// Case A: child subtree fully vanished. Drop its child-position
	// from this branch, which also removes one cell (one separator).
	if newChildID == 0 {
		if descentIdx == 0 {
			if len(cells) == 0 {
				// This branch's only child vanished → branch is now
				// fully empty; cascade newID=0 up.
				if err := pw.FreePage(newBranchID); err != nil {
					return 0, false, fmt.Errorf("btree: free empty branch CoW %d: %w", newBranchID, err)
				}
				if err := pw.FreePage(pageID); err != nil {
					return 0, false, fmt.Errorf("btree: free empty old branch %d: %w", pageID, err)
				}
				return 0, false, nil
			}
			leftmost = cells[0].Child
			cells = cells[1:]
		} else {
			cells = append(cells[:descentIdx-1], cells[descentIdx:]...)
		}
		if err := page.EncodeBranch(parentBuf, cfg, leftmost, cells); err != nil {
			return 0, false, fmt.Errorf("btree: encode branch after child-removal: %w", err)
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, false, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
		}
		return newBranchID, branchUnderflow(cfg, cells, mergeThreshold), nil
	}

	// Patch the child pointer at descentIdx in place. From here on
	// descentIdx denotes the child-position of the (possibly
	// underflowing) replaced child.
	if descentIdx == 0 {
		leftmost = newChildID
	} else {
		cells[descentIdx-1].Child = newChildID
	}

	// Case B: child healthy → parent size unchanged. Skip the
	// merge/redistribute machinery.
	if !childUnderflow {
		if err := page.EncodeBranch(parentBuf, cfg, leftmost, cells); err != nil {
			return 0, false, fmt.Errorf("btree: encode branch after child-pointer update: %w", err)
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, false, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
		}
		// Pointer-only update can't shrink encoded size, but recompute
		// for defense in depth against future encoders.
		return newBranchID, branchUnderflow(cfg, cells, mergeThreshold), nil
	}

	// Case C: child is underflowing → merge with or redistribute
	// against a sibling. A non-root branch always carries ≥1 cell
	// (≥2 children); the guard below handles the transient
	// degenerate state a deeper cascade can produce, by propagating
	// underflow upward instead of stalling.
	if len(cells) == 0 {
		if err := page.EncodeBranch(parentBuf, cfg, leftmost, cells); err != nil {
			return 0, false, fmt.Errorf("btree: encode degenerate branch after underflow: %w", err)
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, false, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
		}
		return newBranchID, true, nil
	}

	// Pick a sibling. Left preferred when one exists, else right —
	// arbitrary but deterministic so tests can pin specific
	// topologies. Both child positions and the cells-index of the
	// separator are derived from descentIdx + the side.
	var (
		useLeft      bool
		siblingPos   int
		separatorIdx int
	)
	if descentIdx > 0 {
		useLeft = true
		siblingPos = int(descentIdx) - 1
		separatorIdx = int(descentIdx) - 1
	} else {
		useLeft = false
		siblingPos = int(descentIdx) + 1
		separatorIdx = int(descentIdx)
	}

	var siblingID uint64
	if siblingPos == 0 {
		siblingID = leftmost
	} else {
		siblingID = cells[siblingPos-1].Child
	}

	// Order the pair so leftPairID always holds the smaller keys.
	var leftPairID, rightPairID uint64
	if useLeft {
		leftPairID, rightPairID = siblingID, newChildID
	} else {
		leftPairID, rightPairID = newChildID, siblingID
	}
	separator := cells[separatorIdx].Key

	// Sibling types must match — all children of a branch live at
	// the same depth per the B+tree balance invariant. A mismatch
	// is structural corruption. Leaf-vs-leaf is the only valid
	// leaf pairing; the dispatcher tolerates either compressed or
	// uncompressed variant on either side (the chunk-4.6β leaf
	// format permits per-leaf variant choice — merge/redistribute
	// just rebuilds via cfg.RestartGroupTarget).
	leftSrc, err := pw.Page(leftPairID)
	if err != nil {
		return 0, false, err
	}
	rightSrc, err := pw.Page(rightPairID)
	if err != nil {
		return 0, false, err
	}
	leftTyp, _, _, _ := page.ReadHeader(leftSrc)
	rightTyp, _, _, _ := page.ReadHeader(rightSrc)
	leftIsLeaf := page.IsLeafType(leftTyp)
	rightIsLeaf := page.IsLeafType(rightTyp)
	if leftIsLeaf != rightIsLeaf {
		return 0, false, fmt.Errorf("%w: sibling page types differ left=%d right=%d", ErrCorrupted, leftTyp, rightTyp)
	}

	var (
		isMerge      bool
		mergedID     uint64
		newLeftID    uint64
		newRightID   uint64
		newSeparator []byte
	)
	switch {
	case leftIsLeaf:
		isMerge, mergedID, newLeftID, newRightID, newSeparator, err = mergeOrRedistributeLeaves(pw, cfg, leftPairID, rightPairID)
	case leftTyp == page.TypeBranch && rightTyp == page.TypeBranch:
		isMerge, mergedID, newLeftID, newRightID, newSeparator, err = mergeOrRedistributeBranches(pw, cfg, leftPairID, rightPairID, separator)
	default:
		return 0, false, fmt.Errorf("%w: sibling page %d unexpected type %d", ErrCorrupted, leftPairID, leftTyp)
	}
	if err != nil {
		return 0, false, err
	}

	// Project the helper's result back into the parent's child array.
	posLeftPair, posRightPair := int(descentIdx), siblingPos
	if useLeft {
		posLeftPair, posRightPair = siblingPos, int(descentIdx)
	}
	if isMerge {
		// posLeftPair's slot becomes the merged page; posRightPair's
		// cell (the separator + child pointer) is removed.
		if posLeftPair == 0 {
			leftmost = mergedID
		} else {
			cells[posLeftPair-1].Child = mergedID
		}
		cells = append(cells[:separatorIdx], cells[separatorIdx+1:]...)
	} else {
		if posLeftPair == 0 {
			leftmost = newLeftID
		} else {
			cells[posLeftPair-1].Child = newLeftID
		}
		// posRightPair is always ≥ 1 since posRightPair = posLeftPair + 1.
		cells[posRightPair-1].Child = newRightID
		cells[separatorIdx].Key = newSeparator
	}

	if err := page.EncodeBranch(parentBuf, cfg, leftmost, cells); err != nil {
		return 0, false, fmt.Errorf("btree: encode branch after merge/redistribute: %w", err)
	}
	if err := pw.FreePage(pageID); err != nil {
		return 0, false, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
	}
	return newBranchID, branchUnderflow(cfg, cells, mergeThreshold), nil
}

// mergeOrRedistributeLeaves combines (or rebalances) two sibling
// leaves. Returns (isMerge, mergedID, newLeftID, newRightID,
// newSeparator, err):
//   - On merge: mergedID is set; new*ID and newSeparator are zero.
//   - On redistribute: new*ID and newSeparator are set; mergedID is
//     zero.
//
// In both outcomes the input leftID and rightID are freed.
//
// Decision: build into a freshly-allocated mergedBuf via
// LeafBuilder.AddEntry; if every entry fits in one page, the merge
// is committed. Otherwise the merge buffer is freed (returning to
// the loose-page pool for re-use inside this tx) and the entries
// are redistributed across two newly-allocated pages, balancing by
// entry count and recomputing the boundary separator via
// page.ShortestSeparator.
//
// Variant policy. The surviving page(s) are built via
// cfg.RestartGroupTarget — the input leaves' on-page variant is
// not preserved. With the chunk-4.6β format the per-leaf variant
// is a build-time policy choice, not a per-page invariant; merge/
// redistribute homogenizes toward the keyspace-level target. No
// spec clause requires variant preservation across merge.
func mergeOrRedistributeLeaves(pw PageWriter, cfg page.Config, leftID, rightID uint64) (bool, uint64, uint64, uint64, []byte, error) {
	leftSrc, err := pw.Page(leftID)
	if err != nil {
		return false, 0, 0, 0, nil, err
	}
	rightSrc, err := pw.Page(rightID)
	if err != nil {
		return false, 0, 0, 0, nil, err
	}
	leftEntries, err := readLeafEntriesDeepCopy(leftSrc, cfg, leftID)
	if err != nil {
		return false, 0, 0, 0, nil, err
	}
	rightEntries, err := readLeafEntriesDeepCopy(rightSrc, cfg, rightID)
	if err != nil {
		return false, 0, 0, 0, nil, err
	}

	// Sibling key-ordering invariant: left.lastKey < right.firstKey.
	// A violation is structural corruption — surface ErrCorrupted
	// rather than producing an out-of-order merged page (LeafBuilder
	// would panic on out-of-order AddEntry anyway, but with a less
	// informative message).
	if len(leftEntries) > 0 && len(rightEntries) > 0 &&
		bytes.Compare(leftEntries[len(leftEntries)-1].Key, rightEntries[0].Key) >= 0 {
		return false, 0, 0, 0, nil, fmt.Errorf("%w: leaf siblings %d/%d not ordered across boundary", ErrCorrupted, leftID, rightID)
	}

	combined := make([]page.LeafEntry, 0, len(leftEntries)+len(rightEntries))
	combined = append(combined, leftEntries...)
	combined = append(combined, rightEntries...)

	// Try merge: build into a fresh page; succeed iff every entry
	// fits. If the build overflows, free the merge destination and
	// fall through to redistribute.
	mergedID, err := pw.AllocPage()
	if err != nil {
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc merged leaf: %w", err)
	}
	mergedBuf, err := pw.AllocSlab(mergedID)
	if err != nil {
		_ = pw.FreePage(mergedID)
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc merged leaf slab: %w", err)
	}
	b := page.NewLeafBuilder(mergedBuf, cfg)
	mergeFits := true
	for _, e := range combined {
		if !b.AddEntry(e) {
			mergeFits = false
			break
		}
	}
	if mergeFits {
		b.Finish()
		if err := pw.FreePage(leftID); err != nil {
			return false, 0, 0, 0, nil, fmt.Errorf("btree: free left leaf %d: %w", leftID, err)
		}
		if err := pw.FreePage(rightID); err != nil {
			return false, 0, 0, 0, nil, fmt.Errorf("btree: free right leaf %d: %w", rightID, err)
		}
		return true, mergedID, 0, 0, nil, nil
	}
	// Merge overflowed — release the scratch destination back to
	// the loose-page pool inside this tx and redistribute instead.
	if err := pw.FreePage(mergedID); err != nil {
		return false, 0, 0, 0, nil, fmt.Errorf("btree: free unused merge slab %d: %w", mergedID, err)
	}

	// Redistribute: split by count and validate per-half fits. With
	// limits.md key-size bounds (≤ ~PageSize/2 per entry), the
	// per-half fit check is defense in depth — combined > 1 page
	// implies ≥ 2 entries, and a count-based split balances them.
	// Both failure modes (< 2 combined entries; per-half oversize)
	// are reachable only from a structurally-invalid input (sibling
	// pages that violate the per-key max or that arrived empty when
	// they shouldn't), so they wrap ErrCorrupted to give the
	// chunk-5 keyspace surface a single errors.Is class to switch
	// on.
	if len(combined) < 2 {
		return false, 0, 0, 0, nil, fmt.Errorf("%w: redistribute leaves with <2 combined entries", ErrCorrupted)
	}
	mid := len(combined) / 2
	leftSplit := combined[:mid]
	rightSplit := combined[mid:]

	newLeftID, err := pw.AllocPage()
	if err != nil {
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc redistribute-left leaf: %w", err)
	}
	newLeftBuf, err := pw.AllocSlab(newLeftID)
	if err != nil {
		_ = pw.FreePage(newLeftID)
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc redistribute-left slab: %w", err)
	}
	lb := page.NewLeafBuilder(newLeftBuf, cfg)
	for _, e := range leftSplit {
		if !lb.AddEntry(e) {
			_ = pw.FreePage(newLeftID)
			return false, 0, 0, 0, nil, fmt.Errorf("%w: redistribute leaf left half exceeds page capacity", ErrCorrupted)
		}
	}
	lb.Finish()

	newRightID, err := pw.AllocPage()
	if err != nil {
		_ = pw.FreePage(newLeftID)
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc redistribute-right leaf: %w", err)
	}
	newRightBuf, err := pw.AllocSlab(newRightID)
	if err != nil {
		_ = pw.FreePage(newLeftID)
		_ = pw.FreePage(newRightID)
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc redistribute-right slab: %w", err)
	}
	rb := page.NewLeafBuilder(newRightBuf, cfg)
	for _, e := range rightSplit {
		if !rb.AddEntry(e) {
			_ = pw.FreePage(newLeftID)
			_ = pw.FreePage(newRightID)
			return false, 0, 0, 0, nil, fmt.Errorf("%w: redistribute leaf right half exceeds page capacity", ErrCorrupted)
		}
	}
	rb.Finish()

	if err := pw.FreePage(leftID); err != nil {
		return false, 0, 0, 0, nil, fmt.Errorf("btree: free left leaf %d: %w", leftID, err)
	}
	if err := pw.FreePage(rightID); err != nil {
		return false, 0, 0, 0, nil, fmt.Errorf("btree: free right leaf %d: %w", rightID, err)
	}
	newSep := page.ShortestSeparator(leftSplit[len(leftSplit)-1].Key, rightSplit[0].Key)
	return false, 0, newLeftID, newRightID, newSep, nil
}

// mergeOrRedistributeBranches is the branch-level analogue. Embeds
// the parent's separator into the combined cell list at the boundary:
//
//	combined = leftCells || (separator, rightLeftmost) || rightCells
//
// Merge → encode combined into one branch with leftLeftmost as the
// new branch's leftmost child.
//
// Redistribute → pick a middle cell whose Key is the new separator
// lifted to the parent and whose Child becomes the new right branch's
// leftmost child; cells before/after split into the two new branches.
// This mirrors the chunk-4.4 branch-split path's separator-lift
// convention.
func mergeOrRedistributeBranches(pw PageWriter, cfg page.Config, leftID, rightID uint64, separator []byte) (bool, uint64, uint64, uint64, []byte, error) {
	// An empty parent separator is unreachable from a tree built via
	// Put — page.ShortestSeparator always returns ≥1 byte. Reject
	// explicitly: an empty separator embedded in `combined` would
	// silently make the left subtree unreachable through this branch
	// after re-encode (BranchSearch returns 0 for any target against
	// an empty separator key). Treat as structural corruption.
	if len(separator) == 0 {
		return false, 0, 0, 0, nil, fmt.Errorf("%w: branch siblings %d/%d separated by empty key", ErrCorrupted, leftID, rightID)
	}
	leftSrc, err := pw.Page(leftID)
	if err != nil {
		return false, 0, 0, 0, nil, err
	}
	rightSrc, err := pw.Page(rightID)
	if err != nil {
		return false, 0, 0, 0, nil, err
	}
	leftLeftmost, leftCells := page.DecodeBranch(leftSrc, cfg)
	rightLeftmost, rightCells := page.DecodeBranch(rightSrc, cfg)
	// Deep-clone Keys: leftCells / rightCells borrow from leftSrc /
	// rightSrc; we'll FreePage both inputs after the new pages are
	// encoded, and a sync.Pool-backed slab allocator could in
	// principle hand the encoder a buffer aliased to one of the
	// sources. Independent storage is the safe boundary.
	for i := range leftCells {
		leftCells[i].Key = bytes.Clone(leftCells[i].Key)
	}
	for i := range rightCells {
		rightCells[i].Key = bytes.Clone(rightCells[i].Key)
	}
	sepCopy := bytes.Clone(separator)

	combined := make([]page.BranchCell, 0, len(leftCells)+1+len(rightCells))
	combined = append(combined, leftCells...)
	combined = append(combined, page.BranchCell{Key: sepCopy, Child: rightLeftmost})
	combined = append(combined, rightCells...)

	if page.BranchEncodedSize(cfg, combined) <= cfg.ContentEnd() {
		mergedID, err := pw.AllocPage()
		if err != nil {
			return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc merged branch: %w", err)
		}
		mergedBuf, err := pw.AllocSlab(mergedID)
		if err != nil {
			_ = pw.FreePage(mergedID)
			return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc merged branch slab: %w", err)
		}
		if err := page.EncodeBranch(mergedBuf, cfg, leftLeftmost, combined); err != nil {
			_ = pw.FreePage(mergedID)
			return false, 0, 0, 0, nil, fmt.Errorf("btree: encode merged branch: %w", err)
		}
		if err := pw.FreePage(leftID); err != nil {
			return false, 0, 0, 0, nil, fmt.Errorf("btree: free left branch %d: %w", leftID, err)
		}
		if err := pw.FreePage(rightID); err != nil {
			return false, 0, 0, 0, nil, fmt.Errorf("btree: free right branch %d: %w", rightID, err)
		}
		return true, mergedID, 0, 0, nil, nil
	}

	// Branch redistribute lifts one cell to the parent as the new
	// separator, so each half needs ≥1 cell post-split → combined
	// must carry ≥3 cells. Two-cell combined is unreachable from a
	// limits.md-compliant tree (merge would have succeeded), but
	// the explicit guard prevents a silent 0-cell sibling from
	// surviving a marginal merge-fails-then-redistribute path with
	// near-max-size keys.
	if len(combined) < 3 {
		return false, 0, 0, 0, nil, fmt.Errorf("%w: redistribute branches with <3 combined cells", ErrCorrupted)
	}
	mid := len(combined) / 2
	newSep := combined[mid].Key
	newRightLeftmost := combined[mid].Child
	newLeftCells := combined[:mid]
	newRightCells := combined[mid+1:]
	// Same defense-in-depth guard as mergeOrRedistributeLeaves's
	// separator check — an empty newSep would silently sever the new
	// left subtree from descent.
	if len(newSep) == 0 {
		return false, 0, 0, 0, nil, fmt.Errorf("%w: redistribute branches produced empty separator", ErrCorrupted)
	}
	if page.BranchEncodedSize(cfg, newLeftCells) > cfg.ContentEnd() {
		return false, 0, 0, 0, nil, fmt.Errorf("%w: redistribute branch left half exceeds page capacity", ErrCorrupted)
	}
	if page.BranchEncodedSize(cfg, newRightCells) > cfg.ContentEnd() {
		return false, 0, 0, 0, nil, fmt.Errorf("%w: redistribute branch right half exceeds page capacity", ErrCorrupted)
	}

	newLeftID, err := pw.AllocPage()
	if err != nil {
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc redistribute-left branch: %w", err)
	}
	newLeftBuf, err := pw.AllocSlab(newLeftID)
	if err != nil {
		_ = pw.FreePage(newLeftID)
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc redistribute-left branch slab: %w", err)
	}
	newRightID, err := pw.AllocPage()
	if err != nil {
		_ = pw.FreePage(newLeftID)
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc redistribute-right branch: %w", err)
	}
	newRightBuf, err := pw.AllocSlab(newRightID)
	if err != nil {
		_ = pw.FreePage(newLeftID)
		_ = pw.FreePage(newRightID)
		return false, 0, 0, 0, nil, fmt.Errorf("btree: alloc redistribute-right branch slab: %w", err)
	}
	if err := page.EncodeBranch(newLeftBuf, cfg, leftLeftmost, newLeftCells); err != nil {
		_ = pw.FreePage(newLeftID)
		_ = pw.FreePage(newRightID)
		return false, 0, 0, 0, nil, fmt.Errorf("btree: encode redistribute-left branch: %w", err)
	}
	if err := page.EncodeBranch(newRightBuf, cfg, newRightLeftmost, newRightCells); err != nil {
		_ = pw.FreePage(newLeftID)
		_ = pw.FreePage(newRightID)
		return false, 0, 0, 0, nil, fmt.Errorf("btree: encode redistribute-right branch: %w", err)
	}
	if err := pw.FreePage(leftID); err != nil {
		return false, 0, 0, 0, nil, fmt.Errorf("btree: free left branch %d: %w", leftID, err)
	}
	if err := pw.FreePage(rightID); err != nil {
		return false, 0, 0, 0, nil, fmt.Errorf("btree: free right branch %d: %w", rightID, err)
	}
	return false, 0, newLeftID, newRightID, newSep, nil
}

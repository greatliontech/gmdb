package btree

import (
	"bytes"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// DeleteRange deletes every key k with start <= k < end from the
// tree rooted at rootID, per range-delete.md §Algorithm. Returns
// the count of leaf entries deleted and the new rootID (0 for an
// emptied tree).
//
// Boundary semantics (range-delete.md invariant #1):
//   - start == nil: open-left, "from the beginning"; deletes every
//     k < end (or every k if end is also nil).
//   - end == nil: open-right, "to the end"; deletes every k >= start.
//   - (nil, nil): deletes every key.
//   - start == end OR start > end: empty range, returns (0, rootID,
//     nil) without mutating the tree.
//
// Three-phase algorithm: a single recursive descent fuses phases
// 1+2 (boundary-path identification + interior-subtree retire via
// FreeSubtree) and handles phase 3 (boundary leaf rebuild + branch
// rebalance) on the unwinding path. Root collapse runs at the top
// level after the descent returns.
//
// Errors: btree.ErrCorrupted on structural anomaly; pager errors
// (ErrTxTooLarge, alloc failures) pass through.
//
// On error: pages allocated during this DeleteRange may have been
// retired; the caller's tx-level pager.AbortTx restores from the
// bitmap snapshot. The returned (count, rootID) are meaningful only
// when err == nil.
func DeleteRange(pw PageWriter, cfg page.Config, rootID uint64,
	mergeThreshold uint8, start, end []byte) (uint64, uint64, error) {
	if mergeThreshold == 0 || mergeThreshold > MaxMergeThreshold {
		return 0, 0, fmt.Errorf("btree: DeleteRange MergeThreshold %d outside (0, %d]", mergeThreshold, MaxMergeThreshold)
	}
	if rootID == 0 {
		return 0, 0, nil
	}
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return 0, rootID, nil
	}

	newID, count, _, err := deleteRangeFrom(pw, cfg, mergeThreshold, rootID, start, end)
	if err != nil {
		return 0, 0, err
	}
	if count == 0 {
		return 0, rootID, nil
	}

	// Root collapse: a 0-cell branch root becomes the leftmost child.
	// Iterates because DeleteRange can produce multi-level degenerate
	// roots (an entire subtree retired up through a chain of singleton
	// branches). Bounded by MaxTreeDepth to defend against a cyclic /
	// corrupt root chain — mirrors freeSubtreeAt's depth guard.
	for depth := 0; newID != 0; depth++ {
		if depth > MaxTreeDepth {
			return 0, 0, ErrTreeTooDeep
		}
		buf := pw.Page(newID)
		typ, _, c, _ := page.ReadHeader(buf)
		if typ != page.TypeBranch || c != 0 {
			break
		}
		child := page.BranchLeftmostChild(buf)
		if child == 0 {
			return 0, 0, fmt.Errorf("%w: empty root branch %d has null leftmost child", ErrCorrupted, newID)
		}
		if err := pw.FreePage(newID); err != nil {
			return 0, 0, fmt.Errorf("btree: free collapsed root branch %d: %w", newID, err)
		}
		newID = child
	}
	return count, newID, nil
}

// deleteRangeFrom recursively deletes [start, end) from the subtree
// rooted at pageID. Returns (newID, count, underflow, err). When
// start == nil, "from the beginning"; when end == nil, "to the end".
// count == 0 with newID == pageID signals a no-op (no entry in the
// subtree fell into [start, end)).
func deleteRangeFrom(pw PageWriter, cfg page.Config, mergeThreshold uint8,
	pageID uint64, start, end []byte) (uint64, uint64, bool, error) {
	buf := pw.Page(pageID)
	typ, _, _, _ := page.ReadHeader(buf)
	switch {
	case page.IsLeafType(typ):
		return deleteRangeFromLeaf(pw, cfg, mergeThreshold, pageID, buf, start, end)
	case typ == page.TypeBranch:
		return deleteRangeFromBranch(pw, cfg, mergeThreshold, pageID, buf, start, end)
	default:
		return 0, 0, false, fmt.Errorf("%w: page %d unexpected type %d during DeleteRange descent",
			ErrCorrupted, pageID, typ)
	}
}

// deleteRangeFromLeaf rebuilds the leaf with entries whose keys lie
// outside [start, end), retires overflow chains for in-range entries.
// Returns newID=0 if the leaf becomes empty; pageID unchanged with
// count=0 if no entry fell in range (no allocation).
func deleteRangeFromLeaf(pw PageWriter, cfg page.Config, mergeThreshold uint8,
	pageID uint64, srcBuf []byte, start, end []byte) (uint64, uint64, bool, error) {
	entries, err := readLeafEntriesDeepCopy(srcBuf, cfg, pageID)
	if err != nil {
		return 0, 0, false, err
	}
	// keep aliases entries' backing array: as we range, each iteration
	// reads entries[i] (Go range copies the struct value) before any
	// potential append to keep[j], and j ≤ i because keep is always a
	// subset of entries up to index i — so the in-place rewrite is
	// safe and avoids a second allocation for the keep slice.
	keep := entries[:0]
	deleted := make([]page.LeafEntry, 0)
	for _, e := range entries {
		if keyInRange(e.Key, start, end) {
			deleted = append(deleted, e)
		} else {
			keep = append(keep, e)
		}
	}
	if len(deleted) == 0 {
		// No entry fell in range — return the original leaf unchanged.
		return pageID, 0, false, nil
	}
	if len(keep) == 0 {
		// Whole leaf in range — retire it (and every overflow chain).
		// Order: chain-free first (still reachable via the
		// not-yet-retired leaf), then leaf-free. Mirrors the
		// rebuildLeafAfterDelete empty-result path.
		for _, e := range deleted {
			if err := freeOverflowChainIfPresent(pw, cfg, e); err != nil {
				return 0, 0, false, err
			}
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, 0, false, fmt.Errorf("btree: free emptied leaf %d: %w", pageID, err)
		}
		return 0, uint64(len(deleted)), true, nil
	}

	// Partial: rebuild leaf with keep entries.
	newID, err := pw.AllocPage()
	if err != nil {
		return 0, 0, false, fmt.Errorf("btree: alloc CoW leaf for DeleteRange: %w", err)
	}
	newBuf, err := pw.CoW(pageID, newID)
	if err != nil {
		return 0, 0, false, fmt.Errorf("btree: CoW leaf %d for DeleteRange: %w", pageID, err)
	}
	b := page.NewLeafBuilder(newBuf, cfg)
	for _, e := range keep {
		if !b.AddEntry(e) {
			// Deletion strictly shrinks the entry set — a build that
			// doesn't fit after removing entries would have failed at
			// the original encode too. Treat as structural corruption.
			_ = pw.FreePage(newID)
			return 0, 0, false, fmt.Errorf("%w: leaf %d re-build after DeleteRange overflowed page",
				ErrCorrupted, pageID)
		}
	}
	b.Finish()
	for _, e := range deleted {
		if err := freeOverflowChainIfPresent(pw, cfg, e); err != nil {
			_ = pw.FreePage(newID)
			return 0, 0, false, err
		}
	}
	if err := pw.FreePage(pageID); err != nil {
		_ = pw.FreePage(newID)
		return 0, 0, false, fmt.Errorf("btree: free old leaf %d: %w", pageID, err)
	}
	return newID, uint64(len(deleted)), leafUnderflow(newBuf, cfg, mergeThreshold), nil
}

// keyInRange reports whether key k lies in [start, end). nil start =
// open-left (every k passes); nil end = open-right (every k passes);
// both nil = every k passes.
func keyInRange(k, start, end []byte) bool {
	if start != nil && bytes.Compare(k, start) < 0 {
		return false
	}
	if end != nil && bytes.Compare(k, end) >= 0 {
		return false
	}
	return true
}

// deleteRangeFromBranch implements the multi-child + single-child
// range-delete on a branch page. Phases 1+2 are fused: at this
// level, interior children (entirely inside [start, end)) are
// FreeSubtree'd; at-most-two boundary children are recursed into
// with one-sided ranges. Phase 3 happens on the unwinding path:
// the branch is CoW-rebuilt with the surviving child layout, and
// any underflowing boundary children are merged or redistributed
// with their nearest siblings in the new layout.
func deleteRangeFromBranch(pw PageWriter, cfg page.Config, mergeThreshold uint8,
	pageID uint64, srcBuf []byte, start, end []byte) (uint64, uint64, bool, error) {
	cellCount := page.BranchCellCount(srcBuf)

	leftIdx := uint16(0)
	if start != nil {
		leftIdx = page.BranchSearch(srcBuf, cfg, start)
	}
	rightIdx := cellCount
	if end != nil {
		rightIdx = page.BranchSearch(srcBuf, cfg, end)
	}

	if leftIdx == rightIdx {
		// Single child overlaps the range. Recurse into it; reuse the
		// chunk-4 patchBranchAfterChildDelete for the parent update +
		// underflow handling.
		childID := page.BranchChildAt(srcBuf, cfg, leftIdx)
		if childID == 0 {
			return 0, 0, false, fmt.Errorf("%w: null child in branch %d at descent %d",
				ErrCorrupted, pageID, leftIdx)
		}
		newChildID, count, childUnderflow, err := deleteRangeFrom(pw, cfg, mergeThreshold, childID, start, end)
		if err != nil {
			return 0, 0, false, err
		}
		if count == 0 {
			return pageID, 0, false, nil
		}
		newID, underflow, err := patchBranchAfterChildDelete(pw, cfg, mergeThreshold, pageID, leftIdx, newChildID, childUnderflow)
		if err != nil {
			return 0, 0, false, err
		}
		return newID, count, underflow, nil
	}

	// Multi-child case: leftIdx < rightIdx.
	var deletedCount uint64

	// Phase 2: FreeSubtree every interior child (descent indices
	// strictly between leftIdx and rightIdx).
	for i := leftIdx + 1; i < rightIdx; i++ {
		interiorID := page.BranchChildAt(srcBuf, cfg, i)
		if interiorID == 0 {
			return 0, 0, false, fmt.Errorf("%w: null interior child in branch %d at descent %d",
				ErrCorrupted, pageID, i)
		}
		n, err := FreeSubtree(pw, cfg, interiorID)
		if err != nil {
			return 0, 0, false, err
		}
		deletedCount += n
	}

	// Phase 1+2 right: recurse into left boundary with (start, nil) —
	// delete every k >= start within this subtree (everything in this
	// subtree is < S_leftIdx <= max_key_in_range, so the upper bound
	// is effectively open).
	leftChildID := page.BranchChildAt(srcBuf, cfg, leftIdx)
	if leftChildID == 0 {
		return 0, 0, false, fmt.Errorf("%w: null left-boundary child in branch %d at descent %d",
			ErrCorrupted, pageID, leftIdx)
	}
	newLeftID, leftCount, leftUnderflow, err := deleteRangeFrom(pw, cfg, mergeThreshold, leftChildID, start, nil)
	if err != nil {
		return 0, 0, false, err
	}
	deletedCount += leftCount

	// Recurse into right boundary with (nil, end) — delete every
	// k < end within this subtree.
	rightChildID := page.BranchChildAt(srcBuf, cfg, rightIdx)
	if rightChildID == 0 {
		return 0, 0, false, fmt.Errorf("%w: null right-boundary child in branch %d at descent %d",
			ErrCorrupted, pageID, rightIdx)
	}
	newRightID, rightCount, rightUnderflow, err := deleteRangeFrom(pw, cfg, mergeThreshold, rightChildID, nil, end)
	if err != nil {
		return 0, 0, false, err
	}
	deletedCount += rightCount

	if deletedCount == 0 {
		// Nothing was actually deleted — caller should treat as no-op.
		return pageID, 0, false, nil
	}

	// Phase 3: rebuild the branch with the surviving children.
	//
	// Surviving slots, in original descent-index order:
	//   - i ∈ [0, leftIdx): original child, kept as-is.
	//   - i == leftIdx: newLeftID if non-zero, else dropped.
	//   - i ∈ (leftIdx, rightIdx): interior, all dropped.
	//   - i == rightIdx: newRightID if non-zero, else dropped.
	//   - i ∈ (rightIdx, cellCount]: original child, kept as-is.
	//
	// We track (origIdx, child, underflow) per survivor (slot type
	// declared at package scope in this file). underflow is true ONLY
	// for the boundary positions whose recurse reported underflow;
	// other positions are healthy by assumption (their children
	// weren't touched).
	survivors := make([]slot, 0, int(cellCount)+1)
	for i := uint16(0); i <= cellCount; i++ {
		switch {
		case i < leftIdx:
			survivors = append(survivors, slot{origIdx: i, child: page.BranchChildAt(srcBuf, cfg, i)})
		case i == leftIdx:
			if newLeftID != 0 {
				survivors = append(survivors, slot{origIdx: i, child: newLeftID, underflow: leftUnderflow})
			}
		case i < rightIdx:
			// interior — dropped.
		case i == rightIdx:
			if newRightID != 0 {
				survivors = append(survivors, slot{origIdx: i, child: newRightID, underflow: rightUnderflow})
			}
		default: // i > rightIdx
			survivors = append(survivors, slot{origIdx: i, child: page.BranchChildAt(srcBuf, cfg, i)})
		}
	}

	if len(survivors) == 0 {
		// Entire branch retired.
		if err := pw.FreePage(pageID); err != nil {
			return 0, 0, false, fmt.Errorf("btree: free emptied branch %d: %w", pageID, err)
		}
		return 0, deletedCount, true, nil
	}

	// Decode the original cells once (for separator-key lookup).
	_, origCells := page.DecodeBranch(srcBuf, cfg)
	origCellKeys := make([][]byte, len(origCells))
	for i, c := range origCells {
		origCellKeys[i] = bytes.Clone(c.Key)
	}

	// Phase 3 rebalance: for each underflowing boundary slot, merge or
	// redistribute with the closest adjacent survivor. Left-preferred
	// to match the existing patchBranchAfterChildDelete convention.
	//
	// The post-FreeSubtree boundary children may have a sibling that
	// is itself a recently-CoW'd boundary child OR an original-tree
	// child. mergeOrRedistribute* handles both since the input pages
	// are looked up by id (leaf vs branch types determined by the
	// dispatcher).
	if err := rebalanceSurvivors(pw, cfg, origCellKeys, &survivors); err != nil {
		return 0, 0, false, err
	}

	// Encode the new branch.
	if len(survivors) == 0 {
		// All collapsed away by rebalance? Shouldn't happen post-fix,
		// but handle defensively.
		if err := pw.FreePage(pageID); err != nil {
			return 0, 0, false, fmt.Errorf("btree: free emptied branch (post-rebalance) %d: %w", pageID, err)
		}
		return 0, deletedCount, true, nil
	}
	newLeftmost := survivors[0].child
	newCells := make([]page.BranchCell, 0, len(survivors)-1)
	for j := 1; j < len(survivors); j++ {
		// Separator between survivors[j-1] and survivors[j] is the
		// original cell to the left of survivors[j].origIdx — i.e.,
		// origCellKeys[survivors[j].origIdx - 1]. The original
		// separator monotonicity (S_a <= S_b for a < b) guarantees
		// this key still satisfies max(left) < S <= min(right) for
		// the surviving pair, because survivor pairs are not bridged
		// across removed children (interior children are dropped, not
		// replaced — see range-delete.md §Algorithm).
		newCells = append(newCells, page.BranchCell{
			Key:   origCellKeys[survivors[j].origIdx-1],
			Child: survivors[j].child,
		})
	}

	newBranchID, err := pw.AllocPage()
	if err != nil {
		return 0, 0, false, fmt.Errorf("btree: alloc CoW branch for DeleteRange: %w", err)
	}
	parentBuf, err := pw.CoW(pageID, newBranchID)
	if err != nil {
		return 0, 0, false, fmt.Errorf("btree: CoW branch %d for DeleteRange: %w", pageID, err)
	}
	if err := page.EncodeBranch(parentBuf, cfg, newLeftmost, newCells); err != nil {
		_ = pw.FreePage(newBranchID)
		return 0, 0, false, fmt.Errorf("btree: encode branch after DeleteRange: %w", err)
	}
	if err := pw.FreePage(pageID); err != nil {
		_ = pw.FreePage(newBranchID)
		return 0, 0, false, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
	}
	// Degenerate-branch (1 child, 0 cells) signals "promote single
	// child" semantics. Treat as underflow so the parent cascades or
	// the top-level root-collapse loop handles it.
	if len(newCells) == 0 {
		return newBranchID, deletedCount, true, nil
	}
	return newBranchID, deletedCount, branchUnderflow(cfg, newCells, mergeThreshold), nil
}

// rebalanceSurvivors walks the survivors list and, for each slot
// whose underflow flag is set, merges or redistributes its child
// with the closest adjacent survivor. Updates survivors in place:
// merged slots reduce the slice length; redistributed slots get
// new child IDs and the separator key for the redistributed pair
// is rewritten (stored in origCellKeys at the appropriate slot's
// origIdx-1 position).
func rebalanceSurvivors(pw PageWriter, cfg page.Config, origCellKeys [][]byte, survivors *[]slot) error {
	for j := 0; j < len(*survivors); j++ {
		s := (*survivors)[j]
		if !s.underflow {
			continue
		}
		// Pick sibling: left-preferred, else right. If neither exists
		// (only one survivor), the parent's root-collapse / cascade
		// path handles the degenerate state.
		if len(*survivors) == 1 {
			break
		}
		var sibJ int
		if j > 0 {
			sibJ = j - 1
		} else {
			sibJ = 1
		}

		// Order the pair so leftPair holds smaller keys.
		var leftPairID, rightPairID uint64
		var leftJ, rightJ int
		if sibJ < j {
			leftPairID, rightPairID = (*survivors)[sibJ].child, s.child
			leftJ, rightJ = sibJ, j
		} else {
			leftPairID, rightPairID = s.child, (*survivors)[sibJ].child
			leftJ, rightJ = j, sibJ
		}

		// The separator between leftJ and rightJ is at
		// origCellKeys[(*survivors)[rightJ].origIdx - 1].
		sepKeyIdx := (*survivors)[rightJ].origIdx - 1
		separator := origCellKeys[sepKeyIdx]

		// Dispatch to leaves or branches.
		leftBuf := pw.Page(leftPairID)
		rightBuf := pw.Page(rightPairID)
		leftTyp, _, _, _ := page.ReadHeader(leftBuf)
		rightTyp, _, _, _ := page.ReadHeader(rightBuf)
		leftIsLeaf := page.IsLeafType(leftTyp)
		rightIsLeaf := page.IsLeafType(rightTyp)
		if leftIsLeaf != rightIsLeaf {
			return fmt.Errorf("%w: rebalance sibling page types differ left=%d right=%d",
				ErrCorrupted, leftTyp, rightTyp)
		}

		var (
			isMerge      bool
			mergedID     uint64
			newLeftID    uint64
			newRightID   uint64
			newSeparator []byte
			err          error
		)
		if leftIsLeaf {
			isMerge, mergedID, newLeftID, newRightID, newSeparator, err = mergeOrRedistributeLeaves(pw, cfg, leftPairID, rightPairID)
		} else {
			isMerge, mergedID, newLeftID, newRightID, newSeparator, err = mergeOrRedistributeBranches(pw, cfg, leftPairID, rightPairID, separator)
		}
		if err != nil {
			return err
		}

		if isMerge {
			// leftJ absorbs the merge; rightJ is removed.
			(*survivors)[leftJ].child = mergedID
			(*survivors)[leftJ].underflow = false
			*survivors = append((*survivors)[:rightJ], (*survivors)[rightJ+1:]...)
			// Adjust j after splice: if sibJ < j (left sibling used),
			// j moved down by 1; if sibJ > j (right sibling used), j
			// is unchanged but the next iteration must not skip the
			// freshly-shifted-in survivor.
			if sibJ < j {
				j--
			}
			// Don't increment j further — re-check the merged slot
			// for residual underflow on the next iteration. The
			// outer for-loop's j++ does the increment.
		} else {
			(*survivors)[leftJ].child = newLeftID
			(*survivors)[rightJ].child = newRightID
			(*survivors)[j].underflow = false
			// Update the separator between leftJ and rightJ.
			origCellKeys[sepKeyIdx] = newSeparator
		}
	}
	return nil
}

// slot is the survivor-tracking type used by deleteRangeFromBranch
// and rebalanceSurvivors. Package-private to range_delete.go's flow.
type slot struct {
	origIdx   uint16
	child     uint64
	underflow bool
}

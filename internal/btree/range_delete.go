package btree

import (
	"bytes"
	"fmt"
	"sync/atomic"

	"github.com/greatliontech/gmdb/internal/page"
)

// deleteRangeCalledHookForTest is fired once per DeleteRange
// invocation, at the function-entry point after argument validation.
// Tests that need to verify which dispatch path took the walker (vs
// a higher-level per-key loop) install via
// SetDeleteRangeCalledHookForTest. Production callers must never
// install a hook. Mirrors the readTxCleanupHookForTest pattern
// (`read_tx.go`) used to test deterministic-synchronization points;
// non-blocking is the inherited constraint (the hook fires inside
// DeleteRange's normal call path, no quiescence guarantee).
//
// The lifetime contract: the hook fires AFTER mergeThreshold /
// perCellFree / rootID / start>=end validation but BEFORE any
// page mutation, so a test installing the hook on a workload that
// would have been a no-op (rootID==0, empty range) still gets the
// signal. Caller asserts via the hook whether SetKeyspace's
// un-indexed dispatch reached btree.DeleteRange.
var deleteRangeCalledHookForTest atomic.Pointer[func()]

// SetDeleteRangeCalledHookForTest installs (or clears, if hook is
// nil) the test-only hook fired at DeleteRange entry. Returns the
// prior hook so callers can restore it via defer. The hook is
// process-global; tests using it must not run concurrently with
// other tests that share the hook.
//
// Cite: TestSetKeyspaceDeleteRangeUnindexedDispatchesToWalker /
// TestSetKeyspaceDeleteRangeIndexedDoesNotDispatchToWalker pin the
// dispatch-direction invariant in `range-delete.md §Set Keyspace
// Range Delete`. `git log --all -S deleteRangeCalledHookForTest`
// preserves the rationale chain.
func SetDeleteRangeCalledHookForTest(hook *func()) *func() {
	return deleteRangeCalledHookForTest.Swap(hook)
}

// PerCellFreeFn is the per-leaf-entry callback DeleteRange's
// boundary-leaf cleanup invokes for each entry to be removed.
// Per range-delete.md §Algorithm, it carries two responsibilities:
//
//  1. Retire any per-cell resources the entry references — for
//     Kind=0 cells the overflow chain (when CellFlagOverflow is
//     set); for Kind=1 cells additionally the nested-tree subtree
//     (CellFlagMultiValue|CellFlagNestedTree) or the inline
//     subpage (CellFlagMultiValue alone — the subpage's value
//     count is tallied but no extra page is freed; the subpage
//     bytes live in the leaf entry itself).
//  2. Return the count of user-visible VALUES this entry
//     contributes to DeleteRange's total. For Kind=0 every entry
//     contributes 1 (one key→value pair); for Kind=1 a subpage
//     contributes the subpage's Count, a nested-tree contributes
//     its NestedCount (returned from FreeSubtree), a plain
//     overflow-only or no-flag cell contributes 1.
//
// Interior subtrees fully in [start, end) are retired via the
// existing FreeSubtree which itself walks each leaf and counts
// cell-type-aware values (see subtree.go) — the callback is
// invoked ONLY at the boundary-leaf positions where the walker
// rebuilds with keep entries.
//
// On error the walker aborts the operation; the caller's
// tx-level Rollback restores via the pager bitmap snapshot per
// pager-slab.md. No partial retirement is observable post-
// Rollback.
type PerCellFreeFn func(pw PageWriter, cfg page.Config, e page.LeafEntry) (uint64, error)

// DeleteRange deletes every key k with start <= k < end from the
// tree rooted at rootID, per range-delete.md §Algorithm. Returns
// the count of user-visible VALUES deleted (1 per Kind=0 entry;
// the sum of subpage Counts + nested-tree NestedCounts + 1 per
// plain cell for Kind=1) and the new rootID (0 for an emptied
// tree). The values-vs-entries distinction is driven entirely
// by the caller's perCellFree callback — see PerCellFreeFn.
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
// perCellFree must be non-nil — DeleteRange has no implicit
// default cell-free behavior. Callers operating on Kind=0
// keyspaces typically pass a callback that calls FreeRun on
// overflow chains and returns 1; callers on Kind=1 SetKeyspaces
// pass one that additionally calls FreeSubtree on nested-tree
// cells and tallies the subpage / NestedCount.
//
// Errors: btree.ErrCorrupted on structural anomaly; pager errors
// (ErrTxTooLarge, alloc failures) pass through; any error
// returned by perCellFree propagates verbatim.
//
// On error: pages allocated during this DeleteRange may have been
// retired; the caller's tx-level pager.AbortTx restores from the
// bitmap snapshot. The returned (count, rootID) are meaningful only
// when err == nil.
func DeleteRange(pw PageWriter, cfg page.Config, rootID uint64,
	mergeThreshold uint8, start, end []byte, perCellFree PerCellFreeFn) (uint64, uint64, error) {
	if mergeThreshold == 0 || mergeThreshold > MaxMergeThreshold {
		return 0, 0, fmt.Errorf("btree: DeleteRange MergeThreshold %d outside (0, %d]", mergeThreshold, MaxMergeThreshold)
	}
	if perCellFree == nil {
		return 0, 0, fmt.Errorf("btree: DeleteRange perCellFree callback is required")
	}
	if hook := deleteRangeCalledHookForTest.Load(); hook != nil {
		(*hook)()
	}
	if rootID == 0 {
		return 0, 0, nil
	}
	if start != nil && end != nil && bytes.Compare(start, end) >= 0 {
		return 0, rootID, nil
	}

	newID, count, _, topDeep, err := deleteRangeFrom(pw, cfg, mergeThreshold, rootID, start, end, perCellFree)
	if err != nil {
		return 0, 0, err
	}
	if count == 0 {
		return 0, rootID, nil
	}

	// Top-level final-heal pass — mirrors the Delete() top-level rule.
	// See delete.go's Delete() commentary for the full reasoning; the
	// short version: when the cascade reaches the new root with a
	// sub-MT direct child still in flight (rare; requires every
	// cascade level to exhaust siblings), no higher level exists to
	// run cousinRebalanceBranch via case-C. Calling it here closes
	// that gap; the root-collapse loop below then promotes any
	// residual to root (where it becomes exempt from the floor).
	if topDeep != 0 && newID != 0 {
		nr, _, _, herr := cousinRebalanceBranch(pw, cfg, newID, topDeep, mergeThreshold)
		if herr != nil {
			return 0, 0, herr
		}
		newID = nr
	}

	newID, err = collapseDegenerateRoot(pw, cfg, newID)
	if err != nil {
		return 0, 0, err
	}
	return count, newID, nil
}

// deleteRangeFrom recursively deletes [start, end) from the subtree
// rooted at pageID. Returns (newID, count, underflow, deepUnderflowChild, err).
// When start == nil, "from the beginning"; when end == nil, "to the end".
// count == 0 with newID == pageID signals a no-op (no entry in the
// subtree fell into [start, end)).
//
// deepUnderflowChild carries the range-delete.md §Invariants fill-floor
// cousin-cascade signal: non-zero iff a sub-MT page in this subtree
// could not be healed by local rebalance at any of the cascade levels
// below — the next level up cousin-rebalances it after its own
// case-C merge. At leaves this is always 0 (no rebalance happens at
// the leaf level itself). See `patchBranchAfterChildDelete` and
// `cousinRebalanceBranch` for the mechanism.
func deleteRangeFrom(pw PageWriter, cfg page.Config, mergeThreshold uint8,
	pageID uint64, start, end []byte, perCellFree PerCellFreeFn) (uint64, uint64, bool, uint64, error) {
	buf, err := pw.Page(pageID)
	if err != nil {
		return 0, 0, false, 0, err
	}
	typ, _, _, _ := page.ReadHeader(buf)
	switch {
	case page.IsLeafType(typ):
		newID, count, underflow, err := deleteRangeFromLeaf(pw, cfg, mergeThreshold, pageID, buf, start, end, perCellFree)
		return newID, count, underflow, 0, err
	case page.IsBranchType(typ):
		if err := validateBranchPage(buf, cfg, pageID); err != nil {
			return 0, 0, false, 0, err
		}
		return deleteRangeFromBranch(pw, cfg, mergeThreshold, pageID, buf, start, end, perCellFree)
	default:
		return 0, 0, false, 0, fmt.Errorf("%w: page %d unexpected type %d during DeleteRange descent",
			ErrCorrupted, pageID, typ)
	}
}

// deleteRangeFromLeaf rebuilds the leaf with entries whose keys lie
// outside [start, end), invoking perCellFree on each in-range entry
// to retire its per-cell resources and tally its values
// contribution. Returns newID=0 if the leaf becomes empty; pageID
// unchanged with count=0 if no entry fell in range (no allocation).
// The returned count is the SUM of perCellFree returns (not
// uint64(len(deleted))) so SetKeyspace cells with multi-value
// subpage or nested-tree contributions are accounted correctly.
func deleteRangeFromLeaf(pw PageWriter, cfg page.Config, mergeThreshold uint8,
	pageID uint64, srcBuf []byte, start, end []byte, perCellFree PerCellFreeFn) (uint64, uint64, bool, error) {
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
	deletedIdxs := make([]int, 0)
	for i, e := range entries {
		// Overflow-key entries are classified on their FULL key —
		// resident bytes can tie with a boundary that only diverges
		// in the extent, and a mis-classification either deletes an
		// out-of-range key or retains an in-range one.
		k := e.Key
		if e.IsOverflowKey() {
			full, err := materializeEntryKey(pw, cfg, e)
			if err != nil {
				return 0, 0, false, err
			}
			k = full
		}
		if keyInRange(k, start, end) {
			deleted = append(deleted, e)
			deletedIdxs = append(deletedIdxs, i)
		} else {
			keep = append(keep, e)
		}
	}
	if len(deleted) == 0 {
		// No entry fell in range — return the original leaf unchanged.
		return pageID, 0, false, nil
	}
	if len(keep) == 0 {
		// Whole leaf in range — retire it (and every per-cell resource).
		// Order: per-cell-free first (still reachable via the
		// not-yet-retired leaf), then leaf-free. Mirrors the
		// rebuildLeafAfterDelete empty-result path.
		var totalCount uint64
		for _, e := range deleted {
			n, err := perCellFree(pw, cfg, e)
			if err != nil {
				return 0, 0, false, err
			}
			totalCount += n
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, 0, false, fmt.Errorf("btree: free emptied leaf %d: %w", pageID, err)
		}
		return 0, totalCount, true, nil
	}

	// Partial: rebuild leaf with keep entries.
	newID, err := pw.AllocPage()
	if err != nil {
		return 0, 0, false, fmt.Errorf("btree: alloc CoW leaf for DeleteRange: %w", err)
	}
	newBuf, err := pw.CopyPage(pageID, newID)
	if err != nil {
		return 0, 0, false, fmt.Errorf("btree: CoW leaf %d for DeleteRange: %w", pageID, err)
	}
	b := page.NewLeafBuilder(newBuf, cfg)
	built := true
	for _, e := range keep {
		if !b.AddEntry(e) {
			built = false
			break
		}
	}
	if built {
		b.Finish()
	} else {
		// The keep-set is NOT removal-monotone under a canonical
		// re-encode: restart-group re-alignment, or a variant
		// migration after a mid-life RestartGroupTarget change, can
		// grow the encoding past one page even though entries were
		// removed. Fall back to native-variant splices of the
		// original bytes — a splice delete always shrinks, so
		// removing the in-range entries one by one (descending index,
		// so earlier indices stay valid) always fits; the page keeps
		// its on-disk variant (page-formats.md §Insert and Delete).
		// Every splice has pre-delete count ≥ keep+1 ≥ 2, so the
		// count<=1 decline is unreachable; a decline is structural
		// corruption. srcBuf is the untouched original page; the
		// builder dirtied only newBuf.
		copy(newBuf, srcBuf)
		for j := len(deletedIdxs) - 1; j >= 0; j-- {
			if !page.TryDeleteAtNative(newBuf, cfg, deletedIdxs[j]) {
				_ = pw.FreePage(newID)
				return 0, 0, false, fmt.Errorf("%w: leaf %d native splice after DeleteRange re-build overflow declined at idx %d",
					ErrCorrupted, pageID, deletedIdxs[j])
			}
		}
	}
	var totalCount uint64
	for _, e := range deleted {
		n, err := perCellFree(pw, cfg, e)
		if err != nil {
			_ = pw.FreePage(newID)
			return 0, 0, false, err
		}
		totalCount += n
	}
	if err := pw.FreePage(pageID); err != nil {
		_ = pw.FreePage(newID)
		return 0, 0, false, fmt.Errorf("btree: free old leaf %d: %w", pageID, err)
	}
	return newID, totalCount, leafUnderflow(newBuf, cfg, mergeThreshold), nil
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
	pageID uint64, srcBuf []byte, start, end []byte, perCellFree PerCellFreeFn) (uint64, uint64, bool, uint64, error) {
	cellCount := page.BranchCellCount(srcBuf)

	leftIdx := uint16(0)
	if start != nil {
		var serr error
		leftIdx, serr = page.BranchSearch(srcBuf, cfg, start, keyTail(pw, cfg))
		if serr != nil {
			return 0, 0, false, 0, serr
		}
	}
	rightIdx := cellCount
	if end != nil {
		var serr error
		rightIdx, serr = page.BranchSearch(srcBuf, cfg, end, keyTail(pw, cfg))
		if serr != nil {
			return 0, 0, false, 0, serr
		}
	}

	if leftIdx == rightIdx {
		// Single child overlaps the range. Recurse into it; reuse the
		// patchBranchAfterChildDelete for the parent update +
		// underflow handling — which also threads the fill-floor
		// cousin-cascade signal (deepUnderflowChild) through its
		// case-C cousin step.
		childID := page.BranchChildAt(srcBuf, cfg, leftIdx)
		if childID == 0 {
			return 0, 0, false, 0, fmt.Errorf("%w: null child in branch %d at descent %d",
				ErrCorrupted, pageID, leftIdx)
		}
		newChildID, count, childUnderflow, childDeepUnderflow, err := deleteRangeFrom(pw, cfg, mergeThreshold, childID, start, end, perCellFree)
		if err != nil {
			return 0, 0, false, 0, err
		}
		if count == 0 {
			return pageID, 0, false, 0, nil
		}
		newID, underflow, deepUnderflowOut, err := patchBranchAfterChildDelete(pw, cfg, mergeThreshold, pageID, leftIdx, newChildID, childUnderflow, childDeepUnderflow)
		if err != nil {
			return 0, 0, false, 0, err
		}
		return newID, count, underflow, deepUnderflowOut, nil
	}

	// Multi-child case: leftIdx < rightIdx.
	var deletedCount uint64

	// Phase 2: FreeSubtree every interior child (descent indices
	// strictly between leftIdx and rightIdx).
	for i := leftIdx + 1; i < rightIdx; i++ {
		interiorID := page.BranchChildAt(srcBuf, cfg, i)
		if interiorID == 0 {
			return 0, 0, false, 0, fmt.Errorf("%w: null interior child in branch %d at descent %d",
				ErrCorrupted, pageID, i)
		}
		n, err := FreeSubtree(pw, cfg, interiorID)
		if err != nil {
			return 0, 0, false, 0, err
		}
		deletedCount += n
	}

	// Phase 1+2 right: recurse into left boundary with (start, nil) —
	// delete every k >= start within this subtree (everything in this
	// subtree is < S_leftIdx <= max_key_in_range, so the upper bound
	// is effectively open).
	leftChildID := page.BranchChildAt(srcBuf, cfg, leftIdx)
	if leftChildID == 0 {
		return 0, 0, false, 0, fmt.Errorf("%w: null left-boundary child in branch %d at descent %d",
			ErrCorrupted, pageID, leftIdx)
	}
	newLeftID, leftCount, leftUnderflow, leftDeepUnderflow, err := deleteRangeFrom(pw, cfg, mergeThreshold, leftChildID, start, nil, perCellFree)
	if err != nil {
		return 0, 0, false, 0, err
	}
	deletedCount += leftCount

	// Recurse into right boundary with (nil, end) — delete every
	// k < end within this subtree.
	rightChildID := page.BranchChildAt(srcBuf, cfg, rightIdx)
	if rightChildID == 0 {
		return 0, 0, false, 0, fmt.Errorf("%w: null right-boundary child in branch %d at descent %d",
			ErrCorrupted, pageID, rightIdx)
	}
	newRightID, rightCount, rightUnderflow, rightDeepUnderflow, err := deleteRangeFrom(pw, cfg, mergeThreshold, rightChildID, nil, end, perCellFree)
	if err != nil {
		return 0, 0, false, 0, err
	}
	deletedCount += rightCount

	if deletedCount == 0 {
		// Nothing was actually deleted — caller should treat as no-op.
		return pageID, 0, false, 0, nil
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
	// We track (origIdx, child, underflow, deepUnderflow) per survivor.
	// underflow is true ONLY for the boundary positions whose recurse
	// reported underflow; other positions are healthy by assumption
	// (their children weren't touched). deepUnderflow carries the
	// fill-floor cousin-cascade signal from the boundary recursions —
	// non-zero iff that survivor is a (possibly degenerate) branch
	// containing a sub-MT child the deeper level couldn't heal.
	survivors := make([]slot, 0, int(cellCount)+1)
	for i := uint16(0); i <= cellCount; i++ {
		switch {
		case i < leftIdx:
			survivors = append(survivors, slot{origIdx: i, child: page.BranchChildAt(srcBuf, cfg, i)})
		case i == leftIdx:
			if newLeftID != 0 {
				survivors = append(survivors, slot{origIdx: i, child: newLeftID, underflow: leftUnderflow, deepUnderflow: leftDeepUnderflow})
			}
		case i < rightIdx:
			// interior — dropped.
		case i == rightIdx:
			if newRightID != 0 {
				survivors = append(survivors, slot{origIdx: i, child: newRightID, underflow: rightUnderflow, deepUnderflow: rightDeepUnderflow})
			}
		default: // i > rightIdx
			survivors = append(survivors, slot{origIdx: i, child: page.BranchChildAt(srcBuf, cfg, i)})
		}
	}

	if len(survivors) == 0 {
		// Entire branch retired. This branch's OWN overflow-separator
		// key extents are reachable only through it (FreeSubtree covers
		// interior children's branches, not this one) — retire them
		// before the page (page-formats.md §Overflow-Key Cells,
		// lifecycle).
		for i := uint16(0); i < cellCount; i++ {
			if err := freeBranchCellExtentIfPresent(pw, cfg, page.BranchCellAt(srcBuf, cfg, i)); err != nil {
				return 0, 0, false, 0, err
			}
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, 0, false, 0, fmt.Errorf("btree: free emptied branch %d: %w", pageID, err)
		}
		return 0, deletedCount, true, 0, nil
	}

	// Decode the original cells once (for separator lookup). Cells are
	// carried WHOLE — an overflow separator's key-extent reference must
	// survive into the rebuilt parent or be explicitly retired
	// (page-formats.md §Overflow-Key Cells, lifecycle); a keys-only
	// snapshot would silently drop every extent.
	_, origCells := page.DecodeBranch(srcBuf, cfg)
	origSepCells := make([]page.BranchCell, len(origCells))
	sepDisposed := make([]bool, len(origCells))
	for i, c := range origCells {
		c.Key = bytes.Clone(c.Key)
		origSepCells[i] = c
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
	//
	// rebalanceSurvivors also closes the fill-floor invariant per
	// range-delete.md §Invariants: if a merge produces a still-below-MT
	// page, it re-merges with the next survivor; if a survivor carried
	// a deepUnderflow descendant from the boundary recursion, that
	// descendant gets cousin-rebalanced against its new siblings inside
	// the merged branch.
	if err := rebalanceSurvivors(pw, cfg, mergeThreshold, origSepCells, sepDisposed, &survivors); err != nil {
		return 0, 0, false, 0, err
	}

	// Encode the new branch.
	if len(survivors) == 0 {
		// All collapsed away by rebalance? Shouldn't happen post-fix,
		// but handle defensively — including the undisposed
		// separators' key extents (the residue pass below never runs
		// on this return).
		for i := range origSepCells {
			if sepDisposed[i] {
				continue
			}
			if err := freeBranchCellExtentIfPresent(pw, cfg, origSepCells[i]); err != nil {
				return 0, 0, false, 0, err
			}
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, 0, false, 0, fmt.Errorf("btree: free emptied branch (post-rebalance) %d: %w", pageID, err)
		}
		return 0, deletedCount, true, 0, nil
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
		c := origSepCells[survivors[j].origIdx-1]
		c.Child = survivors[j].child
		newCells = append(newCells, c)
		sepDisposed[survivors[j].origIdx-1] = true
	}

	// Retire the key extents of separators that neither survived into
	// newCells nor were disposed by a pair rebalance (leaf-pair frees /
	// branch-pair embeds) — the separators of retired interior children.
	// Their extents are referenced by nothing else.
	for i := range origSepCells {
		if sepDisposed[i] {
			continue
		}
		if err := freeBranchCellExtentIfPresent(pw, cfg, origSepCells[i]); err != nil {
			return 0, 0, false, 0, err
		}
	}

	newBranchID, err := pw.AllocPage()
	if err != nil {
		return 0, 0, false, 0, fmt.Errorf("btree: alloc CoW branch for DeleteRange: %w", err)
	}
	parentBuf, err := pw.CopyPage(pageID, newBranchID)
	if err != nil {
		return 0, 0, false, 0, fmt.Errorf("btree: CoW branch %d for DeleteRange: %w", pageID, err)
	}
	if err := page.EncodeBranch(parentBuf, cfg, newLeftmost, newCells); err != nil {
		_ = pw.FreePage(newBranchID)
		return 0, 0, false, 0, fmt.Errorf("btree: encode branch after DeleteRange: %w", err)
	}
	if err := pw.FreePage(pageID); err != nil {
		_ = pw.FreePage(newBranchID)
		return 0, 0, false, 0, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
	}
	// Degenerate-branch (1 child, 0 cells) signals "promote single
	// child" semantics. Treat as underflow so the parent cascades or
	// the top-level root-collapse loop handles it. The single survivor
	// also carries the cousin-cascade signal: if it's underflow AND
	// holds a deep underflow descendant, propagate the latter so the
	// next level up can heal both via a single cousinRebalanceBranch
	// pass. If it's underflow but has no deep descendant, the single
	// survivor's own ID is the deepUnderflowChild for upward
	// propagation — same shape as the patchBranchAfterChildDelete
	// post-merge loop's degenerate-parent exit.
	if len(newCells) == 0 {
		// Propagate `survivors[0].child` — the sole survivor's child —
		// as the deepUnderflowChild signal. By construction this is
		// exactly newBranchID.leftmost (= survivors[0].child); at the
		// next level above, that page becomes a direct child of the
		// next-level merge result via the case-C merge geometry.
		// `survivors[0].deepUnderflow` (a buried descendant inside
		// survivors[0].child) is NOT a direct child of survivors[0].
		// child, so propagating it directly would leave it outside
		// cousinRebalanceBranch's direct-child search range at the
		// next level.
		return newBranchID, deletedCount, true, survivors[0].child, nil
	}
	// Even when this level returns a healthy multi-child branch, a
	// survivor may carry a sub-MT descendant (slot.deepUnderflow != 0
	// with slot.underflow == false). Heal in-place via
	// cousinRebalanceBranch on the survivor's own child — the deep
	// page is a direct child of survivor.child by the producer's
	// contract.
	for j := range survivors {
		if survivors[j].deepUnderflow == 0 {
			continue
		}
		newCh, _, residual, cerr := cousinRebalanceBranch(pw, cfg, survivors[j].child, survivors[j].deepUnderflow, mergeThreshold)
		if cerr != nil {
			return 0, 0, false, 0, cerr
		}
		// Patch newCells / newLeftmost / newBranchID's encoded form to
		// reflect the (possibly re-encoded) survivor.child. Since this
		// runs AFTER the encode of newBranchID above, we have to re-
		// encode if anything changed.
		if newCh != survivors[j].child {
			survivors[j].child = newCh
			if j == 0 {
				newLeftmost = newCh
			} else {
				newCells[j-1].Child = newCh
			}
			if err := page.EncodeBranch(parentBuf, cfg, newLeftmost, newCells); err != nil {
				_ = pw.FreePage(newBranchID)
				return 0, 0, false, 0, fmt.Errorf("btree: re-encode branch after survivor-deep heal: %w", err)
			}
		}
		if residual != 0 {
			// At least one survivor failed to fully heal; propagate.
			// First-non-zero wins (in practice only one survivor in
			// this rare configuration ever carries a non-trivial deep).
			// Force underflow=true so the next level's case-C runs
			// (see the parallel rule in patchBranchAfterChildDelete:
			// case-B does not invoke the cousin pass, so a healthy-
			// encoded branch carrying a deep otherwise threads the
			// signal to the top and discards it).
			return newBranchID, deletedCount, true, residual, nil
		}
	}
	// A survivor still below MT after rebalanceSurvivors is one whose
	// branch redistribute DECLINED — no adjacent merge fit and no
	// redistribute could clear the floor for both halves. Thread its child
	// up as the deepUnderflowChild (and force underflow) exactly as
	// patchBranchAfterChildDelete's post-merge loop does for the single-key
	// path, so a higher level with more cousins gets a chance to heal it
	// rather than this level silently dropping the sub-MT child. By
	// construction survivors[j].child is a direct child of newBranchID
	// (newLeftmost or a newCells entry — the deep-heal loop above already
	// re-pointed it to its re-encoded value). At the next level above,
	// newBranchID merges with a sibling in that level's case-C path, and by
	// the same wrapper-propagation geometry as patchBranchAfterChildDelete
	// (delete.go) survivors[j].child stays a direct child of the merge
	// result, where the receiving cousin pass searches for it. If genuinely
	// unreachable everywhere it ascends to the exempt root (range-delete.md
	// §Invariants "where reachable").
	for j := range survivors {
		if survivors[j].underflow {
			return newBranchID, deletedCount, true, survivors[j].child, nil
		}
	}
	return newBranchID, deletedCount, branchUnderflow(cfg, newCells, mergeThreshold), 0, nil
}

// rebalanceSurvivors walks the survivors list and, for each slot
// whose underflow flag is set, merges or redistributes its child
// with the closest adjacent survivor. Updates survivors in place:
// merged slots reduce the slice length; redistributed slots get
// new child IDs and the separator key for the redistributed pair
// is rewritten (stored in origCellKeys at the appropriate slot's
// origIdx-1 position).
//
// Maintains range-delete.md §Invariants fill-floor clause across the
// two cousin-cascade producers at this level:
//
//  1. Post-merge re-rebalance: after a merge, the merged page's
//     encoded fill is computed; if still below MergeThreshold, the
//     slot's `underflow` flag stays set and `j` is rewound so the
//     outer for-loop's j++ lands on the merged slot — the next
//     iteration picks the next adjacent survivor and re-merges. The
//     loop terminates when either the slot reaches the floor or
//     len(*survivors) == 1 (no sibling here).
//
//  2. Survivor-carries-deep-underflow: when an input slot's recursion
//     handed up a non-zero deepUnderflow (a sub-MT descendant the
//     deeper level couldn't heal), the merge result contains that
//     descendant as a direct child. cousinRebalanceBranch is invoked
//     on the (branch-typed) merged page to heal it. If the cousin
//     pass leaves a still-degenerate residual, it surfaces as the
//     merged slot's new deepUnderflow for the next merge round or
//     for upward propagation by the caller.
func rebalanceSurvivors(pw PageWriter, cfg page.Config, mergeThreshold uint8, origSepCells []page.BranchCell, sepDisposed []bool, survivors *[]slot) error {
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
		sepCell := origSepCells[sepKeyIdx]

		// Capture deepUnderflow signals from BOTH sides before the
		// merge (the helpers free the inputs, so the slot info we
		// keep is what we need to thread into the cousin step).
		leftDeep := (*survivors)[leftJ].deepUnderflow
		rightDeep := (*survivors)[rightJ].deepUnderflow

		// parentFits candidate: the parent branch is later rebuilt from
		// the CURRENT survivor boundaries (deleteRangeFromBranch's
		// newCells loop) — separator keys at origCellKeys[origIdx-1]
		// with sepKeyIdx's key replaced by the redistribute's newSep.
		// Later merges only remove cells (never grow the encoding), and
		// later separator replacements re-run this same check against
		// the then-current set, so checking the current set is exact
		// for this step and conservative for the final encode.
		parentFits := func(candCell page.BranchCell) bool {
			cand := make([]page.BranchCell, 0, len(*survivors)-1)
			for jj := 1; jj < len(*survivors); jj++ {
				c := origSepCells[(*survivors)[jj].origIdx-1]
				if (*survivors)[jj].origIdx-1 == sepKeyIdx {
					c = candCell
				}
				cand = append(cand, c)
			}
			return page.BranchEncodedSize(cfg, cand) <= cfg.ContentEnd()
		}
		pair, err := mergeOrRedistributePair(pw, cfg, mergeThreshold, leftPairID, rightPairID, sepCell, parentFits)
		if err != nil {
			return err
		}
		isMerge, mergedID := pair.isMerge, pair.mergedID
		newLeftID, newRightID, newSepCell := pair.newLeftID, pair.newRightID, pair.newSepCell
		leftIsLeaf := pair.leftIsLeaf

		if !isMerge && newLeftID == 0 {
			// DECLINE: the redistribute could not restore the fill-floor
			// for both halves (either level) or the
			// parent cannot fit the recomputed separator (either level),
			// so it changed nothing. Leave both survivors as-is — the
			// underflowing one stays below MT, accepted per
			// range-delete.md §Invariants "where reachable". Advance to
			// the next survivor WITHOUT rewind: no page churn means no
			// infinite re-pairing of an unhealable pair (the
			// unreachable-floor cascade guard).
			continue
		}

		if isMerge {
			// leftJ absorbs the merge; rightJ is removed. The pair
			// disposed of the boundary separator cell (leaf pairs:
			// extent freed; branch pairs: embedded into the merged
			// branch) — mark it so the parent rebuild's residue pass
			// neither double-frees nor frees a live embedded extent.
			sepDisposed[sepKeyIdx] = true
			//
			// Cousin step. If either input carried a deepUnderflow
			// descendant AND the merge was branch-level (the only case
			// where a deepUnderflow survivor reaches here — a leaf
			// can never produce one), heal the descendants inside
			// mergedID. Process at most ONE deep: the cousin pass
			// starting at the chosen deep walks adjacent siblings
			// via rebalanceChildAtPos, which absorbs the other deep
			// when both inputs were degenerate (their two leftmost
			// children are adjacent in the merge result by
			// construction). Looping over both
			// deeps was wrong: a residual from the first iteration
			// makes the second deep no longer reachable as a direct
			// child of the resulting branch.
			residualDeep := uint64(0)
			if !leftIsLeaf {
				deep := leftDeep
				if deep == 0 {
					deep = rightDeep
				}
				if deep != 0 {
					newCur, _, residual, cerr := cousinRebalanceBranch(pw, cfg, mergedID, deep, mergeThreshold)
					if cerr != nil {
						return cerr
					}
					mergedID = newCur
					residualDeep = residual
				}
			}

			// Compute the merged page's own fill — drives whether
			// this slot needs another merge round against the next
			// adjacent survivor.
			mergedBuf, perr := pw.Page(mergedID)
			if perr != nil {
				return perr
			}
			mergedTyp, _, _, _ := page.ReadHeader(mergedBuf)
			var mergedFillUnderflow bool
			switch {
			case page.IsLeafType(mergedTyp):
				mergedFillUnderflow = leafUnderflow(mergedBuf, cfg, mergeThreshold)
			case page.IsBranchType(mergedTyp):
				_, mc := page.DecodeBranch(mergedBuf, cfg)
				mergedFillUnderflow = branchUnderflow(cfg, mc, mergeThreshold)
			default:
				return fmt.Errorf("%w: merged page %d unexpected type %d", ErrCorrupted, mergedID, mergedTyp)
			}

			(*survivors)[leftJ].child = mergedID
			(*survivors)[leftJ].underflow = mergedFillUnderflow
			(*survivors)[leftJ].deepUnderflow = residualDeep
			*survivors = append((*survivors)[:rightJ], (*survivors)[rightJ+1:]...)
			// Set j so the for-loop's j++ lands on leftJ — re-checks
			// the merged slot's underflow against any next-adjacent
			// survivor. Both ordering cases (sibJ<j and sibJ>j)
			// collapse to: merged slot is now at index leftJ, so
			// j = leftJ - 1 → j++ → leftJ.
			j = leftJ - 1
		} else {
			// Redistribute. The branch-level case requires explicit
			// deep-underflow handling per carried signal: the
			// count-balanced split preserves the left input's leftmost
			// as newLeftID.leftmost, but the right input's leftmost is
			// at the lifted-cell boundary and can land in EITHER
			// output depending on where the count-split falls — so the
			// holder is scanned, never assumed (cousinRebalanceBranch
			// errors on a miss; assuming rightDeep → newRightID failed
			// a valid delete when the split landed it on the left).
			finalLeft := newLeftID
			finalRight := newRightID
			leftResidual := uint64(0)
			rightResidual := uint64(0)
			if !leftIsLeaf {
				healedAny := false
				for _, deep := range [2]uint64{leftDeep, rightDeep} {
					if deep == 0 {
						continue
					}
					// The count-balanced split decides which output
					// received the deep (the right input's leftmost can
					// migrate into the left output) — scan for the
					// holder; cousinRebalanceBranch errors on a miss.
					// With BOTH survivors carrying deeps, the first
					// heal can merge the second deep into a sibling
					// (both can land adjacent in one output) — that
					// deep is then already healed by absorption, and
					// only then is a scan miss legitimate.
					holder, found, herr := deepHolderAfterRedistribute(pw, cfg, finalLeft, finalRight, deep)
					if herr != nil {
						return herr
					}
					if !found {
						if healedAny {
							continue // absorbed by the prior heal
						}
						return fmt.Errorf("%w: deep underflow child %d not found in either redistribute output (%d, %d)", ErrCorrupted, deep, finalLeft, finalRight)
					}
					nc, _, res, cerr := cousinRebalanceBranch(pw, cfg, holder, deep, mergeThreshold)
					if cerr != nil {
						return cerr
					}
					healedAny = true
					if holder == finalLeft {
						finalLeft = nc
						leftResidual = res
					} else {
						finalRight = nc
						rightResidual = res
					}
				}
			}
			// Re-check each redistribute output's encoded fill via
			// pw.Page — the cousin step may have absorbed a child into
			// a sibling, shrinking the output below MT. Hardcoding
			// underflow=false misses this.
			leftUf, lerr := pageUnderflow(pw, cfg, finalLeft, mergeThreshold)
			if lerr != nil {
				return lerr
			}
			rightUf, rerr := pageUnderflow(pw, cfg, finalRight, mergeThreshold)
			if rerr != nil {
				return rerr
			}
			(*survivors)[leftJ].child = finalLeft
			(*survivors)[rightJ].child = finalRight
			(*survivors)[leftJ].underflow = leftUf
			(*survivors)[rightJ].underflow = rightUf
			(*survivors)[leftJ].deepUnderflow = leftResidual
			(*survivors)[rightJ].deepUnderflow = rightResidual
			// Update the separator between leftJ and rightJ. The pair
			// already disposed of the OLD cell's extent (leaf pairs:
			// freed; branch pairs: embedded into the outputs); the
			// replacement carries its own reference and is live — it
			// is consumed by the parent rebuild or disposed by a
			// later pair, exactly like an original cell.
			origSepCells[sepKeyIdx] = newSepCell
			origSepCells[sepKeyIdx].Key = bytes.Clone(newSepCell.Key)
			sepDisposed[sepKeyIdx] = false
			// Rewind j so the for-loop's j++ re-checks the redistribute
			// output that's still below MT for another merge round
			// against its next adjacent survivor.
			// Both ordering cases collapse to: the "tracked" output
			// (= curID's side) sits at leftJ if siblingPos>j (right
			// sibling used) or rightJ if siblingPos<j (left sibling used).
			//
			// Termination: this rewind is bounded. A redistribute only
			// fires (vs DECLINE) when both halves clear the floor, so a
			// rewind here can re-underflow a half ONLY via a cousin shrink,
			// which strictly reduces the combined cell count of the
			// re-paired branch; the count cannot fall forever, so the slot
			// must eventually merge (slice shrinks) or DECLINE (j advances,
			// never rewinds). No infinite redistribute/decline ping-pong —
			// the analogue of rebalanceChildAtPos's triedLeft/triedRight.
			if leftUf || rightUf {
				j = min(leftJ, rightJ) - 1
			}
		}
	}
	return nil
}

// slot is the survivor-tracking type used by deleteRangeFromBranch
// and rebalanceSurvivors. Package-private to range_delete.go's flow.
//
// deepUnderflow carries the range-delete.md §Invariants fill-floor
// cousin-cascade signal: non-zero iff this survivor's subtree
// contains a sub-MT page the recursion at the level below couldn't
// heal locally. Threaded through rebalanceSurvivors' merge step into
// cousinRebalanceBranch.
type slot struct {
	origIdx       uint16
	child         uint64
	underflow     bool
	deepUnderflow uint64
}

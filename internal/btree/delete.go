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
// Maintains range-delete.md §Invariants fill-floor clause: after a
// successful return, every non-root page reachable from the new root
// has fill ≥ MergeThreshold% of ContentEnd. See `deleteFrom`'s
// `deepUnderflowChild` return + the cousin-cascade thread in
// `patchBranchAfterChildDelete` for the mechanism — at the top level
// here, a residual `deepUnderflowChild` (a sub-MT page that no
// intermediate level could heal because every level along the cascade
// was degenerate) ends up exactly at `newRootID` after root collapse,
// which makes it the root (exempt). The trivial top-level handling
// reflects that: any non-zero `deepUnderflowChild` at this level
// has been promoted to root by the collapse loop below.
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
	newRootID, _, found, topDeep, err := deleteFrom(pw, cfg, mergeThreshold, rootID, key)
	if err != nil {
		return 0, err
	}
	if !found {
		return rootID, ErrNotFound
	}
	if newRootID == 0 {
		return 0, nil
	}
	// Top-level final-heal pass: if the cascade left a sub-MT direct
	// child at the new root (the rare case where every cascade level
	// exhausted siblings AND the new root is not itself the residual),
	// run one last cousin pass at root. cousinRebalanceBranch finds
	// the deep at the root's direct-child level (per the wrapper-
	// propagation contract); the rebalance against root's other
	// children heals it, or the root-collapse loop below promotes
	// the residual to root (exempt from the floor). Without this,
	// the deep persists as a sub-MT non-root direct child of root
	// — a fill-floor violation on the root's children.
	if topDeep != 0 {
		nr, _, _, herr := cousinRebalanceBranch(pw, cfg, newRootID, topDeep, mergeThreshold)
		if herr != nil {
			return 0, herr
		}
		newRootID = nr
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
		if err := validateBranchPage(buf, cfg, newRootID); err != nil {
			return 0, err
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
//   - deepUnderflowChild: non-zero iff this level's local rebalance
//     reduced a branch to a single child still below MT (the
//     "cousin-cascade" case from range-delete.md §Invariants fill-floor
//     clause). The caller threads this upward; the level whose own
//     case-C merge produces a sibling-rich branch containing the deep
//     child runs `cousinRebalanceBranch` to heal it. At leaves this
//     return is always 0.
func deleteFrom(pw PageWriter, cfg page.Config, mergeThreshold uint8, pageID uint64, key []byte) (newID uint64, underflow, found bool, deepUnderflowChild uint64, err error) {
	buf, err := pw.Page(pageID)
	if err != nil {
		return 0, false, false, 0, err
	}
	typ, _, _, _ := page.ReadHeader(buf)
	switch {
	case page.IsLeafType(typ):
		newID, underflow, found, err = deleteFromLeaf(pw, cfg, mergeThreshold, pageID, buf, key)
		return newID, underflow, found, 0, err
	case typ == page.TypeBranch:
		if err := validateBranchPage(buf, cfg, pageID); err != nil {
			return 0, false, false, 0, err
		}
		return deleteFromBranch(pw, cfg, mergeThreshold, pageID, buf, key)
	default:
		return 0, false, false, 0, fmt.Errorf("%w: page %d unexpected type %d during Delete descent", ErrCorrupted, pageID, typ)
	}
}

func deleteFromLeaf(pw PageWriter, cfg page.Config, mergeThreshold uint8, pageID uint64, srcBuf []byte, key []byte) (uint64, bool, bool, error) {
	// Validate once at the boundary (the splice helpers do not bound-check
	// structurally — they assume a builder-produced page) and locate the key
	// with SearchLeaf, so neither the fast nor the slow path re-validates or
	// re-searches. Mirrors the Put append/insert fast-path wiring.
	r := page.NewLeafReader(srcBuf, cfg)
	if err := r.Validate(); err != nil {
		return 0, false, false, fmt.Errorf("%w: leaf %d: %w", ErrCorrupted, pageID, err)
	}
	idx, removed, found := r.SearchLeaf(key)
	if !found {
		return pageID, false, false, nil
	}

	// Fast path: in-place delete splice for any leaf with more than one entry.
	// TryDeleteAt handles both variants (canonical for uncompressed, localized
	// for compressed) and declines (→ slow path) only on a variant mismatch (a
	// page whose on-disk variant differs from the configured one after a mid-life
	// RGT change — the rare case); count<=1 (page would empty) is routed straight
	// to the slow path by this gate. On a decline the speculative CoW is freed
	// and the slow path runs (and migrates the variant).
	if r.Count() > 1 {
		newID, err := pw.AllocPage()
		if err != nil {
			return 0, false, false, fmt.Errorf("btree: alloc CoW leaf for delete: %w", err)
		}
		cowBuf, err := pw.CoW(pageID, newID)
		if err != nil {
			_ = pw.FreePage(newID)
			return 0, false, false, fmt.Errorf("btree: CoW leaf %d for delete: %w", pageID, err)
		}
		if page.TryDeleteAt(cowBuf, cfg, idx) {
			// Free the removed entry's overflow chain (if any) AFTER the leaf-
			// level CoW lands and BEFORE the old leaf is retired, so a transient
			// failure can't orphan a chain still reachable via the old leaf —
			// same ordering as rebuildLeafAfterDelete. Only `removed`'s chain
			// fields (Flags, OverflowPage, TotalLen) are used here; SearchLeaf
			// populated them (its returned Key is nil, which the chain-free path
			// does not need).
			if err := freeOverflowChainIfPresent(pw, cfg, removed); err != nil {
				_ = pw.FreePage(newID)
				return 0, false, false, err
			}
			if err := pw.FreePage(pageID); err != nil {
				_ = pw.FreePage(newID)
				return 0, false, false, fmt.Errorf("btree: free old leaf %d: %w", pageID, err)
			}
			return newID, leafUnderflow(cowBuf, cfg, mergeThreshold), true, nil
		}
		// Splice declined (variant mismatch) — free the speculative CoW page and
		// fall through to decode/rebuild, which migrates the leaf to the
		// configured variant.
		_ = pw.FreePage(newID)
	}

	// Slow path: decode + rebuild. Handles count<=1 (empty → free page),
	// uncompressed leaves, and variant migration. Reuses idx from SearchLeaf;
	// `removed` is re-taken as the deep-copied entry so the rebuild's chain-free
	// runs on owned bytes, identical to before the fast path existed.
	entries := leafEntriesDeepCopyFrom(r)
	removed = entries[idx]
	entries = append(entries[:idx], entries[idx+1:]...)
	return rebuildLeafAfterDelete(pw, cfg, mergeThreshold, pageID, entries, removed)
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

// branchUnderflow is the analogue for a branch page. The fill-floor is
// measured on the LOGICAL (uncompressed) content size, not the physical
// compressed size (range-delete.md §Invariants): within-page prefix
// truncation (page-formats.md §Branch Page) is a storage optimization that
// shrinks a same-cluster branch's bytes without reducing its fanout, so a
// logically-dense branch must not read as underfull just because its shared
// prefix compressed away. Capacity ("does it fit a page?") still uses the
// physical BranchEncodedSize; only this floor check is logical.
func branchUnderflow(cfg page.Config, cells []page.BranchCell, mergeThreshold uint8) bool {
	size := page.BranchLogicalSize(cells)
	return size*100 < int(mergeThreshold)*cfg.ContentEnd()
}

// pageUnderflow dispatches to leafUnderflow or branchUnderflow based
// on the page type at id. Used by the redistribute path in
// rebalanceSurvivors and other fill-floor re-check sites that need a
// type-agnostic check after the page may have been mutated by a
// downstream cousin step.
func pageUnderflow(pw PageReader, cfg page.Config, id uint64, mergeThreshold uint8) (bool, error) {
	buf, err := pw.Page(id)
	if err != nil {
		return false, err
	}
	typ, _, _, _ := page.ReadHeader(buf)
	switch {
	case page.IsLeafType(typ):
		return leafUnderflow(buf, cfg, mergeThreshold), nil
	case typ == page.TypeBranch:
		_, cells := page.DecodeBranch(buf, cfg)
		return branchUnderflow(cfg, cells, mergeThreshold), nil
	default:
		return false, fmt.Errorf("%w: page %d unexpected type %d in pageUnderflow", ErrCorrupted, id, typ)
	}
}

func deleteFromBranch(pw PageWriter, cfg page.Config, mergeThreshold uint8, pageID uint64, srcBuf []byte, key []byte) (uint64, bool, bool, uint64, error) {
	descentIdx := page.BranchSearch(srcBuf, cfg, key)
	childID := page.BranchChildAt(srcBuf, cfg, descentIdx)
	if childID == 0 {
		return 0, false, false, 0, fmt.Errorf("%w: null child in branch %d at descent %d", ErrCorrupted, pageID, descentIdx)
	}
	newChildID, childUnderflow, found, childDeepUnderflow, err := deleteFrom(pw, cfg, mergeThreshold, childID, key)
	if err != nil {
		return 0, false, false, 0, err
	}
	if !found {
		return pageID, false, false, 0, nil
	}
	newID, underflow, deepUnderflowChild, err := patchBranchAfterChildDelete(pw, cfg, mergeThreshold, pageID, descentIdx, newChildID, childUnderflow, childDeepUnderflow)
	if err != nil {
		return 0, false, false, 0, err
	}
	return newID, underflow, true, deepUnderflowChild, nil
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
//     redistribute against an adjacent sibling, then run the
//     post-merge re-rebalance loop and (if the recursion handed up
//     a deepUnderflowChildIn) cousin-rebalance the merged result.
//     These two extra steps maintain the range-delete.md §Invariants
//     fill-floor clause — see those functions for the algorithm.
//
// Returns (newID, underflow, deepUnderflowChildOut, err). deepUnderflowChildIn
// is the sub-MT descendant the recursion at the child level could not
// heal locally (0 if none); deepUnderflowChildOut is the corresponding
// signal this level propagates upward (0 if fully healed here).
// Used by both deleteFromBranch (single-key delete) and
// deleteRangeFromBranch's single-child overlap case (chunk-5.7
// DeleteRange).
func patchBranchAfterChildDelete(pw PageWriter, cfg page.Config, mergeThreshold uint8, pageID uint64, descentIdx uint16, newChildID uint64, childUnderflow bool, deepUnderflowChildIn uint64) (uint64, bool, uint64, error) {
	// CoW the parent for mutation and decode its (leftmost, cells)
	// form. Deep-clone every cell Key — DecodeBranch returns Keys
	// that borrow from parentBuf and EncodeBranch will clear that
	// buffer before re-emit, the same aliasing boundary
	// ascendWithSplit handles for Put.
	newBranchID, err := pw.AllocPage()
	if err != nil {
		return 0, false, 0, fmt.Errorf("btree: alloc CoW branch for delete: %w", err)
	}
	parentBuf, err := pw.CoW(pageID, newBranchID)
	if err != nil {
		return 0, false, 0, fmt.Errorf("btree: CoW branch %d for delete: %w", pageID, err)
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
					return 0, false, 0, fmt.Errorf("btree: free empty branch CoW %d: %w", newBranchID, err)
				}
				if err := pw.FreePage(pageID); err != nil {
					return 0, false, 0, fmt.Errorf("btree: free empty old branch %d: %w", pageID, err)
				}
				return 0, false, 0, nil
			}
			leftmost = cells[0].Child
			cells = cells[1:]
		} else {
			cells = append(cells[:descentIdx-1], cells[descentIdx:]...)
		}
		if err := page.EncodeBranch(parentBuf, cfg, leftmost, cells); err != nil {
			return 0, false, 0, fmt.Errorf("btree: encode branch after child-removal: %w", err)
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, false, 0, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
		}
		// Case A vanishes the recursed-into subtree entirely, so any
		// deepUnderflowChildIn was inside that vanished subtree —
		// nothing to thread upward.
		return newBranchID, branchUnderflow(cfg, cells, mergeThreshold), 0, nil
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
			return 0, false, 0, fmt.Errorf("btree: encode branch after child-pointer update: %w", err)
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, false, 0, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
		}
		// Pointer-only update can't shrink encoded size, but recompute
		// for defense in depth against future encoders. Case B
		// cannot receive a non-zero deepUnderflowChildIn from a
		// well-formed recursion (deep underflow is set only when the
		// recursion's branch went degenerate, which makes
		// childUnderflow=true) — but if a future caller passes one,
		// thread it through rather than silently dropping the signal.
		return newBranchID, branchUnderflow(cfg, cells, mergeThreshold), deepUnderflowChildIn, nil
	}

	// Case C: child is underflowing → merge with or redistribute
	// against a sibling. A non-root branch always carries ≥1 cell
	// (≥2 children); the guard below handles the transient
	// degenerate state a deeper cascade can produce, by propagating
	// underflow upward instead of stalling. If deepUnderflowChildIn
	// was set, it lives somewhere in newChildID's subtree — we
	// cannot heal it at this level if we have no sibling here, so
	// thread it upward unchanged for the next level to handle.
	if len(cells) == 0 {
		if err := page.EncodeBranch(parentBuf, cfg, leftmost, cells); err != nil {
			return 0, false, 0, fmt.Errorf("btree: encode degenerate branch after underflow: %w", err)
		}
		if err := pw.FreePage(pageID); err != nil {
			return 0, false, 0, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
		}
		return newBranchID, true, deepUnderflowChildIn, nil
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
		return 0, false, 0, err
	}
	rightSrc, err := pw.Page(rightPairID)
	if err != nil {
		return 0, false, 0, err
	}
	leftTyp, _, _, _ := page.ReadHeader(leftSrc)
	rightTyp, _, _, _ := page.ReadHeader(rightSrc)
	leftIsLeaf := page.IsLeafType(leftTyp)
	rightIsLeaf := page.IsLeafType(rightTyp)
	if leftIsLeaf != rightIsLeaf {
		return 0, false, 0, fmt.Errorf("%w: sibling page types differ left=%d right=%d", ErrCorrupted, leftTyp, rightTyp)
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
		isMerge, mergedID, newLeftID, newRightID, newSeparator, err = mergeOrRedistributeBranches(pw, cfg, mergeThreshold, leftPairID, rightPairID, separator)
	default:
		return 0, false, 0, fmt.Errorf("%w: sibling page %d unexpected type %d", ErrCorrupted, leftPairID, leftTyp)
	}
	if err != nil {
		return 0, false, 0, err
	}

	// Project the helper's result back into the parent's child array.
	posLeftPair, posRightPair := int(descentIdx), siblingPos
	if useLeft {
		posLeftPair, posRightPair = siblingPos, int(descentIdx)
	}
	var insertedPos int
	var insertedID uint64
	switch {
	case isMerge:
		// posLeftPair's slot becomes the merged page; posRightPair's
		// cell (the separator + child pointer) is removed.
		if posLeftPair == 0 {
			leftmost = mergedID
		} else {
			cells[posLeftPair-1].Child = mergedID
		}
		cells = append(cells[:separatorIdx], cells[separatorIdx+1:]...)
		insertedPos = posLeftPair
		insertedID = mergedID
	case newLeftID == 0:
		// DECLINE (branch only): the redistribute could not restore the
		// floor for both halves, so it changed nothing. The parent's cells
		// are unchanged — the underflowing child remains at descentIdx. Let
		// the post-merge re-rebalance loop below give it a chance against
		// OTHER siblings (a merge may yet fit), and thread it up as
		// deepUnderflowChild if none can heal it. No page churn, so the
		// cousin cascade cannot relocate the deficit.
		insertedPos = int(descentIdx)
		insertedID = newChildID
	default:
		if posLeftPair == 0 {
			leftmost = newLeftID
		} else {
			cells[posLeftPair-1].Child = newLeftID
		}
		// posRightPair is always ≥ 1 since posRightPair = posLeftPair + 1.
		cells[posRightPair-1].Child = newRightID
		cells[separatorIdx].Key = newSeparator
		// Redistribute restored the floor for both halves (the decline
		// guard in mergeOrRedistributeBranches guarantees it); the
		// recursed-into side (= descentIdx) is healthy. The post-merge
		// re-rebalance loop below still runs for defense in depth.
		insertedPos = int(descentIdx)
		if useLeft {
			insertedID = newRightID
		} else {
			insertedID = newLeftID
		}
	}
	_ = posRightPair

	// Cousin-rebalance step. When the recursion at the child level
	// could not heal a sub-MT descendant (deepUnderflowChildIn != 0)
	// AND the local case-C merge produced a branch result containing
	// that descendant as a child, run cousinRebalanceBranch on the
	// merged result. Same `(newID, branchUnderflow, residualDeepID)`
	// contract as rebalanceChildAtPos but operating on an already-
	// encoded branch (the helper handles the re-CoW + free).
	//
	// Reachability: the only producer of deepUnderflowChildIn is a
	// recursion that reduced its own branch to a single sub-MT child
	// (rebalanceChildAtPos's "no siblings" exit). That means
	// childUnderflow is also true (the degenerate branch's own
	// encoded fill is ~0). So this branch path only fires when the
	// case-C merge is branch-level — leaf-merge would imply the
	// recursion was a leaf, which cannot produce deepUnderflowChildIn.
	deepUnderflowChildOut := uint64(0)
	if deepUnderflowChildIn != 0 && isMerge && !leftIsLeaf {
		newMergedID, _, residual, err := cousinRebalanceBranch(pw, cfg, mergedID, deepUnderflowChildIn, mergeThreshold)
		if err != nil {
			return 0, false, 0, err
		}
		// Update parent's slot to point at the (possibly re-encoded)
		// merged page.
		if posLeftPair == 0 {
			leftmost = newMergedID
		} else {
			cells[posLeftPair-1].Child = newMergedID
		}
		mergedID = newMergedID
		insertedID = newMergedID
		if residual != 0 {
			// Propagate the wrapper (newMergedID — a direct child of
			// THIS parent CoW) rather than the buried `residual`. At
			// the next level above, this parent merges with sibling
			// into merge_GP; newMergedID stays at a direct-child
			// position of merge_GP (mergedID.leftmost or
			// cells[posLeftPair-1].Child by the case-C merge geometry),
			// so the receiving cousinRebalanceBranch finds it as a
			// direct child. Propagating `residual` directly would
			// leave it buried under newMergedID at merge_GP — outside
			// cousinRebalanceBranch's direct-child search range
			// (review Round 2 H-1).
			deepUnderflowChildOut = newMergedID
		}
	}

	// Post-merge re-rebalance loop. If the merged/redistributed result
	// at insertedPos is itself below MT AND this parent has more cells
	// to merge against, run rebalanceChildAtPos to walk the inserted
	// child rightward/leftward through adjacent siblings until it
	// reaches the fill floor or only one child remains in this parent.
	if len(cells) > 0 {
		finalPos, finalID, finalUnderflow, err := rebalanceChildAtPos(pw, cfg, mergeThreshold, &leftmost, &cells, insertedPos, insertedID)
		if err != nil {
			return 0, false, 0, err
		}
		_ = finalPos
		// If the loop exited with the inserted child still below MT,
		// propagate finalID as deepUnderflowChildOut. finalID is a
		// direct child of this branch's CoW (at *leftmost when the
		// loop fully consumed cells, or at cells[finalPos-1].Child
		// when both-sides-tried/declined exhaustion broke the loop early with
		// cells > 0 still). Either way the wrapper-propagation
		// invariant holds: at the next level above, finalID stays at
		// a direct-child position of merge_GP by the case-C merge
		// geometry. The earlier `&& len(cells) == 0` gate silently
		// dropped the cells>0 exit (review Round 3 M-1).
		//
		// When BOTH this step's finalID AND the cousin step's
		// newMergedID would propagate, last-non-zero wins: the next
		// level's cousinRebalanceBranch scan (delete.go all-children
		// scan) will find the other surviving sub-MT direct child of
		// merge_GP when it walks merge_GP's children.
		if finalUnderflow {
			deepUnderflowChildOut = finalID
		}
	}

	if err := page.EncodeBranch(parentBuf, cfg, leftmost, cells); err != nil {
		return 0, false, 0, fmt.Errorf("btree: encode branch after merge/redistribute: %w", err)
	}
	if err := pw.FreePage(pageID); err != nil {
		return 0, false, 0, fmt.Errorf("btree: free old branch %d: %w", pageID, err)
	}
	// Force parentUnderflow when a deepUnderflowChild is in flight.
	// The signal-receiving level's case-B path (childUnderflow=false)
	// does NOT invoke the cousin pass — it only threads the signal
	// upward, so the deep never finds new siblings and the cascade
	// reaches the top exempt-root unhealed (silent fill-floor
	// violation). Tagging this branch as semantically-underflow even
	// when its encoded fill is fine forces the next level to run
	// case-C (merge with sibling), which gives the deep the new
	// siblings it needs via the merge result + cousinRebalanceBranch.
	// (Root cause of the iterative review-round corner-case spiral —
	// addresses Round 3 M-1 + the producer-1 cells>0 reachability
	// the round-2 wrapper propagation didn't close.)
	parentUnderflow := branchUnderflow(cfg, cells, mergeThreshold)
	if deepUnderflowChildOut != 0 {
		parentUnderflow = true
	}
	return newBranchID, parentUnderflow, deepUnderflowChildOut, nil
}

// rebalanceChildAtPos runs the post-merge re-rebalance loop inside a
// parent's in-memory branch state. The parent's encoded form has not
// yet been written (the caller will EncodeBranch the resulting
// `*leftmost`/`*cells` after this returns) — this helper mutates
// those in place plus the underlying pager state via
// mergeOrRedistribute*.
//
// Starting position (startPos, startID): startPos==0 means the child
// at *leftmost; startPos==k means cells[k-1].Child. The loop checks
// the current child's fill; if below MergeThreshold AND ≥1 cell
// remains in the parent, picks an adjacent sibling (left-preferred,
// per the existing mergeOrRedistribute* contract in
// patchBranchAfterChildDelete), runs mergeOrRedistribute*, and
// re-walks. Exits when either the current child reaches the floor or
// `len(*cells) == 0` (no more siblings here).
//
// Returns (finalPos, finalID, finalUnderflow, err):
//   - finalUnderflow=true ⇒ len(*cells)==0 AND the lone surviving
//     child finalID==*leftmost is still below MergeThreshold. The
//     caller propagates finalID upward as deepUnderflowChild so a
//     higher level can heal it via cousinRebalanceBranch after its
//     own cascade-merge.
//   - finalUnderflow=false ⇒ the current child reached the floor;
//     the parent's (leftmost, cells) is the heal-converged state.
func rebalanceChildAtPos(pw PageWriter, cfg page.Config, mergeThreshold uint8, leftmost *uint64, cells *[]page.BranchCell, startPos int, startID uint64) (int, uint64, bool, error) {
	curPos := startPos
	curID := startID
	// triedLeft/triedRight: whether the left/right adjacent sibling has
	// already been paired and DECLINED (merge overflows a page and a
	// redistribute cannot restore the floor for both halves — see
	// mergeOrRedistributeBranches). A decline changes no pages, so
	// re-pairing the same sibling reruns the identical decision forever; a
	// single "last tried" marker is insufficient because with both
	// neighbours present the loop would ping-pong (try left, try right,
	// re-allow left, …). Once both adjacent slots are tried/declined,
	// exit with finalUnderflow=true so the caller threads the residual up.
	// Reset on merge (cell layout changed). A successful merge or
	// redistribute heals curID, so the next iteration returns before these
	// matter; they bound only the decline path.
	triedLeft, triedRight := false, false
	for {
		// Read current child to compute fill.
		buf, err := pw.Page(curID)
		if err != nil {
			return 0, 0, false, err
		}
		typ, _, _, _ := page.ReadHeader(buf)
		var underflow bool
		switch {
		case page.IsLeafType(typ):
			underflow = leafUnderflow(buf, cfg, mergeThreshold)
		case typ == page.TypeBranch:
			_, childCells := page.DecodeBranch(buf, cfg)
			underflow = branchUnderflow(cfg, childCells, mergeThreshold)
		default:
			return 0, 0, false, fmt.Errorf("%w: page %d unexpected type %d during rebalanceChildAtPos", ErrCorrupted, curID, typ)
		}
		if !underflow {
			return curPos, curID, false, nil
		}
		if len(*cells) == 0 {
			// Parent has no more siblings to absorb this child.
			// Caller threads curID upward as deepUnderflowChild.
			return curPos, curID, true, nil
		}
		// Pick adjacent sibling: left-preferred if curPos>0, else right,
		// skipping a side already tried-and-declined (see the triedLeft/
		// triedRight commentary above the loop).
		var siblingPos, separatorIdx int
		switch {
		case curPos > 0 && !triedLeft:
			siblingPos = curPos - 1
			separatorIdx = curPos - 1
		case curPos+1 <= len(*cells) && !triedRight:
			siblingPos = curPos + 1
			separatorIdx = curPos
		default:
			// Both adjacent slots either don't exist or were already
			// tried-and-declined; another iteration cannot make progress at
			// this level. Propagate the residual underflow upward.
			return curPos, curID, true, nil
		}
		var siblingID uint64
		if siblingPos == 0 {
			siblingID = *leftmost
		} else {
			siblingID = (*cells)[siblingPos-1].Child
		}
		// Order the pair so leftPair holds smaller keys.
		var leftPairID, rightPairID uint64
		var posLeftPair, posRightPair int
		if siblingPos < curPos {
			leftPairID, rightPairID = siblingID, curID
			posLeftPair, posRightPair = siblingPos, curPos
		} else {
			leftPairID, rightPairID = curID, siblingID
			posLeftPair, posRightPair = curPos, siblingPos
		}
		separator := (*cells)[separatorIdx].Key
		// Dispatch by type.
		leftSrc, err := pw.Page(leftPairID)
		if err != nil {
			return 0, 0, false, err
		}
		leftTyp, _, _, _ := page.ReadHeader(leftSrc)
		leftIsLeaf := page.IsLeafType(leftTyp)
		var (
			isMerge      bool
			mergedID     uint64
			newLeftID    uint64
			newRightID   uint64
			newSeparator []byte
		)
		if leftIsLeaf {
			isMerge, mergedID, newLeftID, newRightID, newSeparator, err = mergeOrRedistributeLeaves(pw, cfg, leftPairID, rightPairID)
		} else {
			isMerge, mergedID, newLeftID, newRightID, newSeparator, err = mergeOrRedistributeBranches(pw, cfg, mergeThreshold, leftPairID, rightPairID, separator)
		}
		if err != nil {
			return 0, 0, false, err
		}
		switch {
		case isMerge:
			if posLeftPair == 0 {
				*leftmost = mergedID
			} else {
				(*cells)[posLeftPair-1].Child = mergedID
			}
			*cells = append((*cells)[:separatorIdx], (*cells)[separatorIdx+1:]...)
			curPos = posLeftPair
			curID = mergedID
			// Cell layout changed — re-explore both neighbours of curPos.
			triedLeft, triedRight = false, false
		case newLeftID == 0:
			// DECLINE: the branch redistribute could not restore the floor
			// for both halves (large lifted separator), so it changed
			// nothing. Mark this side tried; when both are tried the loop
			// exits with the underflow threaded up. No page churn, so the
			// cascade cannot relocate the deficit.
			if siblingPos < curPos {
				triedLeft = true
			} else {
				triedRight = true
			}
		default:
			// Redistribute restored the floor for both halves (the decline
			// guard guarantees it), so curID is now ≥ MT and the next
			// iteration returns. Project the two new pages into the parent.
			if posLeftPair == 0 {
				*leftmost = newLeftID
			} else {
				(*cells)[posLeftPair-1].Child = newLeftID
			}
			(*cells)[posRightPair-1].Child = newRightID
			(*cells)[separatorIdx].Key = bytes.Clone(newSeparator)
			if siblingPos < curPos {
				curPos = posRightPair
				curID = newRightID
			} else {
				curPos = posLeftPair
				curID = newLeftID
			}
			triedLeft, triedRight = false, false
		}
		// Loop re-checks underflow on curID.
	}
}

// cousinRebalanceBranch heals a sub-MT descendant `deepID` that lives
// inside an already-encoded branch `branchID`. The deep descendant
// arrived here from a recursion that exhausted siblings at its own
// level and threaded `deepID` upward via deepUnderflowChild; the
// caller's local case-C merge then produced `branchID` as a
// sibling-rich result whose children include `deepID` (as either
// `leftmost` or some `cells[i].Child`).
//
// Algorithm: decode branchID's state, locate deepID's position, run
// rebalanceChildAtPos starting there, encode the resulting layout
// into a fresh branch page (freeing the original branchID's slab).
//
// Returns (newBranchID, branchUnderflow, residualDeepID, err):
//   - newBranchID is the re-CoW'd branch reflecting the cousin merge
//     (or the same branchID if no mutation was needed — but the
//     helper always reallocates for clarity).
//   - residualDeepID is non-zero iff the cousin rebalance reduced
//     this branch to a single sub-MT child; the caller threads it
//     one more level up. 0 means fully healed at this level.
func cousinRebalanceBranch(pw PageWriter, cfg page.Config, branchID uint64, deepID uint64, mergeThreshold uint8) (uint64, bool, uint64, error) {
	buf, err := pw.Page(branchID)
	if err != nil {
		return 0, false, 0, err
	}
	if err := validateBranchPage(buf, cfg, branchID); err != nil {
		return 0, false, 0, err
	}
	leftmost, cells := page.DecodeBranch(buf, cfg)
	for i := range cells {
		cells[i].Key = bytes.Clone(cells[i].Key)
	}
	// Find deepID's position. It must be a direct child of branchID
	// (the cousin-cascade thread always lands here at the merge that
	// promoted the deep child to a direct-child slot — see
	// patchBranchAfterChildDelete's case-C cousin step for the
	// argument).
	deepPos := -1
	if leftmost == deepID {
		deepPos = 0
	} else {
		for i, c := range cells {
			if c.Child == deepID {
				deepPos = i + 1
				break
			}
		}
	}
	if deepPos == -1 {
		return 0, false, 0, fmt.Errorf("%w: cousinRebalanceBranch: deep underflow child %d not found in branch %d", ErrCorrupted, deepID, branchID)
	}
	// Snapshot pre-rebalance to detect no-op pass (caller's defensive
	// pre-check found the deep child was actually already healthy in
	// this branch). A no-op returns branchID unchanged, avoiding
	// AllocPage + AllocSlab + FreePage churn that would otherwise
	// inflate the pager's pending alloc/free lists with no semantic
	// effect (review M-1).
	prevLen := len(cells)
	prevLeftmost := leftmost
	finalPos, finalID, finalUnderflow, err := rebalanceChildAtPos(pw, cfg, mergeThreshold, &leftmost, &cells, deepPos, deepID)
	if err != nil {
		return 0, false, 0, err
	}
	if len(cells) == prevLen && leftmost == prevLeftmost && !finalUnderflow {
		return branchID, branchUnderflow(cfg, cells, mergeThreshold), 0, nil
	}
	// Cascade-absorption spine walk (review H-3). When the
	// rebalanceChildAtPos pass merged a degenerate wrapper branch
	// (carrying a sub-MT descendant as its sole child) into a
	// healthy sibling-rich result, the merge result's leftmost is
	// now that descendant — still sub-MT, but at a different
	// nesting depth than the rebalance loop tracks (the loop only
	// checks `curID`'s own fill, not its leftmost descendant's).
	// Walk down finalID's leftmost spine; for each non-root level
	// whose leftmost is sub-MT, recursively cousin-rebalance.
	// Termination is bounded by tree depth: each recursion strictly
	// descends. If a recursive call exhausts siblings at its level,
	// the residual cascades back here as `recResidual` and the
	// caller's own cascade continues unchanged.
	residual := uint64(0)
	if finalUnderflow {
		// The local heal could not lift deepID to the floor — either no
		// siblings remain (len(cells)==0) or every adjacent pairing declined
		// (a merge would overflow a page and a redistribute could not restore
		// the floor for both halves; see mergeOrRedistributeBranches). Thread
		// deepID up as the residual instead of running the descendant scan.
		// The scan recurses on THIS branch, so for an unhealable child it
		// re-encodes and re-recurses without progress (the unreachable-floor
		// OOM); it is sound only AFTER a successful heal, where a merge may
		// have buried a sub-MT descendant a cousin pass must reach.
		residual = finalID
	} else {
		// Post-rebalance descendant scan (Round 2 review H-1 + H-3).
		// The rebalance may have absorbed a wrapper-branch into a
		// merge result whose sub-MT descendant ended up at a
		// non-leftmost position (e.g. when the in-rebalance merge
		// picked a LEFT sibling, the absorbed wrapper's leftmost
		// lands at mergedID.cells[len(leftSibling.cells)].Child, NOT
		// at leftmost). A leftmost-only spine walk missed this.
		// Instead, scan ALL of finalID's direct children for sub-MT;
		// for each, recursively cousin-heal. Bounded by tree depth
		// × per-branch fanout × cousin recursion depth (= tree depth)
		// in the worst case; for the common (heal-at-first-merge)
		// case this terminates in a single iteration.
		curScan := finalID
		for {
			scanBuf, perr := pw.Page(curScan)
			if perr != nil {
				return 0, false, 0, perr
			}
			scanTyp, _, _, _ := page.ReadHeader(scanBuf)
			if scanTyp != page.TypeBranch {
				break
			}
			scanLeftmost, scanCells := page.DecodeBranch(scanBuf, cfg)
			if len(scanCells) == 0 {
				break
			}
			// Find the first sub-MT direct child: leftmost first, then cells.
			candidates := make([]uint64, 0, 1+len(scanCells))
			candidates = append(candidates, scanLeftmost)
			for _, c := range scanCells {
				candidates = append(candidates, c.Child)
			}
			subID, sfind := uint64(0), false
			for _, candidate := range candidates {
				cbuf, ferr := pw.Page(candidate)
				if ferr != nil {
					return 0, false, 0, ferr
				}
				ctyp, _, _, _ := page.ReadHeader(cbuf)
				var cuf bool
				switch {
				case page.IsLeafType(ctyp):
					cuf = leafUnderflow(cbuf, cfg, mergeThreshold)
				case ctyp == page.TypeBranch:
					_, cc := page.DecodeBranch(cbuf, cfg)
					cuf = branchUnderflow(cfg, cc, mergeThreshold)
				default:
					return 0, false, 0, fmt.Errorf("%w: page %d unexpected type %d during cousin descendant scan", ErrCorrupted, candidate, ctyp)
				}
				if cuf {
					subID = candidate
					sfind = true
					break
				}
			}
			if !sfind {
				break
			}
			newScan, _, recResidual, rerr := cousinRebalanceBranch(pw, cfg, curScan, subID, mergeThreshold)
			if rerr != nil {
				return 0, false, 0, rerr
			}
			// Update OUR branch's slot pointing at finalID.
			if finalPos == 0 {
				leftmost = newScan
			} else {
				cells[finalPos-1].Child = newScan
			}
			curScan = newScan
			finalID = newScan
			if recResidual != 0 {
				// The recursive call exhausted: newScan is a degenerate
				// wrapping branch that holds recResidual transitively.
				// Propagate newScan (the wrapper — a direct child of
				// OUR returned branch) so the caller's cousin at the
				// next level above finds it as a direct child of the
				// next-level merge result. (Review Round 2 H-1.)
				residual = newScan
				break
			}
			// Else: loop in case the rebalance introduced a new sub-MT
			// descendant a level deeper.
		}
	}
	// Allocate a fresh branch page for the re-encoded layout. Same
	// re-CoW idiom Put uses for split children — the helper owns the
	// freed slot reuse via the pager's loose-page pool.
	newBranchID, err := pw.AllocPage()
	if err != nil {
		return 0, false, 0, fmt.Errorf("btree: cousinRebalanceBranch alloc: %w", err)
	}
	newBuf, err := pw.AllocSlab(newBranchID)
	if err != nil {
		return 0, false, 0, fmt.Errorf("btree: cousinRebalanceBranch alloc slab: %w", err)
	}
	if err := page.EncodeBranch(newBuf, cfg, leftmost, cells); err != nil {
		return 0, false, 0, fmt.Errorf("btree: cousinRebalanceBranch encode: %w", err)
	}
	if err := pw.FreePage(branchID); err != nil {
		return 0, false, 0, fmt.Errorf("btree: cousinRebalanceBranch free old %d: %w", branchID, err)
	}
	return newBranchID, branchUnderflow(cfg, cells, mergeThreshold), residual, nil
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
// are redistributed across two newly-allocated pages at a
// byte-balanced split point (findLeafSplitIndex, page-formats.md
// §Leaf Split), recomputing the boundary separator via
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

	// Redistribute across two pages at a byte-balanced boundary
	// (page-formats.md §Leaf Split), not the entry-count midpoint: a
	// count split of size-skewed siblings can place more than a page of
	// bytes on one half and spuriously fail. The combined entries arrived
	// from two valid sibling pages, so a feasible two-page partition
	// always exists (at minimum the original page boundary, which is
	// at least as balanced as the input); findLeafSplitIndex returning
	// ok=false therefore means a structurally-invalid input — a stored
	// entry exceeding page capacity — a genuine ErrCorrupted, not a
	// balance failure. newLeftBuf doubles as the split-measurement
	// scratch before it holds the real left half.
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
	mid, ok := findLeafSplitIndex(lb, newLeftBuf, cfg, combined)
	if !ok {
		_ = pw.FreePage(newLeftID)
		return false, 0, 0, 0, nil, fmt.Errorf("%w: redistribute leaves have no feasible two-page split", ErrCorrupted)
	}
	leftSplit := combined[:mid]
	rightSplit := combined[mid:]

	lb.Reset(newLeftBuf, cfg)
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
func mergeOrRedistributeBranches(pw PageWriter, cfg page.Config, mergeThreshold uint8, leftID, rightID uint64, separator []byte) (bool, uint64, uint64, uint64, []byte, error) {
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
	// Both siblings are read fresh here (NOT on the descent path that was
	// validated on the way down), so validate their directories before
	// DecodeBranch / BranchCellAt iterate them — mirrors the sibling-leaf
	// Validate in mergeOrRedistributeLeaves, completing the branch-page-
	// validation contract for the merge/redistribute path.
	if err := validateBranchPage(leftSrc, cfg, leftID); err != nil {
		return false, 0, 0, 0, nil, err
	}
	if err := validateBranchPage(rightSrc, cfg, rightID); err != nil {
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
	// Choose the boundary by BYTE size, not the cell-count midpoint
	// (btree-byte-balanced-split, page-formats.md §Leaf Split). The merge
	// above already failed, so `combined` exceeds one page; a count midpoint
	// can overflow a half on size-skewed separators (spurious ErrCorrupted)
	// or leave a half below MergeThreshold (range-delete.md §Invariants
	// fill-floor). Balancing to ~50% both fits each half and keeps it above
	// the floor. !ok ⇒ no feasible two-page split, which from two sibling
	// branches that each already fit one page (separators ≤ the limits.md
	// §Maximum Key Size bound) is unreachable ⇒ genuine corruption.
	mid, ok := findBranchSplitIndex(cfg, combined)
	if !ok {
		return false, 0, 0, 0, nil, fmt.Errorf("%w: redistribute branches have no feasible two-page split", ErrCorrupted)
	}
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

	// Decline (no alloc, both inputs untouched) when this redistribute
	// cannot restore the fill-floor for BOTH halves. A branch redistribute
	// lifts the boundary separator to the parent, so both halves lose its
	// bytes; with a large separator the halves land below MergeThreshold
	// even though `combined` exceeds one page. Performing it would merely
	// relocate the sub-MT deficit to a sibling, which the cousin-rebalance
	// cascade then chases without converging — the fill-floor is unreachable
	// here (range-delete.md §Invariants "where reachable"). Declining lets
	// the caller accept the underflowing child as-is and terminate. In the
	// reachable regime (short separators) both halves clear the floor, so
	// this never fires and redistribute proceeds unchanged.
	if branchUnderflow(cfg, newLeftCells, mergeThreshold) || branchUnderflow(cfg, newRightCells, mergeThreshold) {
		return false, 0, 0, 0, nil, nil
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

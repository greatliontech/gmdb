package btree

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/page"
)

// FreeSubtree retires every page reachable from rootID and returns
// the count of "user-visible values" freed. Used by
// DeleteKeyspace (count discarded for Kind=0; uses count
// for desc.Count delta on Kind=1), DeleteRange (count
// surfaces as part of the rowsDeleted return), and
// per-key bulk-free for SetKeyspaces (called on the cell's
// NestedRoot to retire one key's nested tree — `set-keyspace.md
// §Bulk Free`). For the per-key path, this function retires only
// the nested subtree pages; the parent leaf's cell is removed by
// the caller via `btree.Delete` on the parent tree. Walks the tree DFS:
// branches recurse into children, leaves enumerate their entries to
// retire overflow chains + nested-tree subtrees AND tally the value
// count, then each page (leaf or branch) is retired via FreePage.
//
// Count semantics. The returned count is the number of
// user-visible values freed:
//
//   - Plain leaf entry (no flags / overflow): contributes 1 (one
//     key→value pair).
//   - Subpage cell (CellFlagMultiValue, NestedTree clear): contributes
//     `Subpage.Count` (the inline value count). Decoded by reading
//     the 2-byte Count header at offset 0 of e.Value — works
//     regardless of the keyspace's FixedValueSize.
//   - Nested-tree-reference cell (CellFlagMultiValue|NestedTree):
//     recursively retires the nested B+tree via
//     freeSubtreeAt(NestedRoot, depth+1); the recursive count is
//     added (each nested leaf entry = one value in the set).
//
// For Kind=0 keyspaces (plain key→value, no MultiValue cells)
// behaviour is unchanged: count = number of leaf entries.
//
// Empty subtree (rootID == 0) returns (0, nil) — the caller
// may pass an unmaterialised keyspace's desc.Root.
//
// Errors:
//   - ErrTreeTooDeep on a cycle / structurally-corrupted tree (the
//     descent exceeds MaxTreeDepth — same bound the Get / Has
//     readers use).
//   - ErrCorrupted on an unexpected page type or a null child
//     pointer (mirrors the Get / Has corruption surface).
//   - The first FreePage / FreeRun error encountered is returned
//     verbatim with positional context.
//
// On error: some pages may already have been retired AND the
// returned count is meaningless. The caller's failure path is the
// existing pager.AbortTx machinery — the bitmap snapshot taken at
// Begin restores the pre-FreeSubtree state (retiredPages and
// loosePages are both cleared and the bitmap bits restored). No
// partial retirement is observable post-AbortTx; no new tx-poison
// machinery is needed.
func FreeSubtree(pw PageWriter, cfg page.Config, rootID uint64) (uint64, error) {
	if rootID == 0 {
		return 0, nil
	}
	return freeSubtreeAt(pw, cfg, rootID, 0)
}

// freeSubtreeAt recursively retires pageID and everything below it
// and returns the count of leaf entries freed under this subtree.
// depth is the descent depth — bounded by MaxTreeDepth to catch
// cyclic / corrupt trees, same bound as Get / Has.
func freeSubtreeAt(pw PageWriter, cfg page.Config, pageID uint64, depth int) (uint64, error) {
	if depth > MaxTreeDepth {
		return 0, ErrTreeTooDeep
	}
	var count uint64
	buf, err := pw.Page(pageID)
	if err != nil {
		return 0, err
	}
	typ, _, cellCount, _ := page.ReadHeader(buf)
	switch {
	case typ == page.TypeBranch:
		if err := validateBranchPage(buf, cfg, pageID); err != nil {
			return 0, err
		}
		// Recurse into the leftmost child + each cell's right child.
		// page.BranchChildAt uses descent-index semantics: i=0 is the
		// leftmost (Ptr[0]); i ∈ [1, cellCount] are the cell-pointed
		// children — so the valid index range is [0, cellCount],
		// giving cellCount+1 children total.
		//
		// Capture every child id BEFORE the recursive descent so the
		// branch's mmap-resident buffer doesn't need to outlive the
		// recursion.
		children := make([]uint64, 0, int(cellCount)+1)
		for i := uint16(0); i <= cellCount; i++ {
			c := page.BranchChildAt(buf, cfg, i)
			if c == 0 {
				return 0, fmt.Errorf("%w: null child pointer in branch page %d descent index %d at depth %d",
					ErrCorrupted, pageID, i, depth)
			}
			children = append(children, c)
		}
		// Retire the key extents of overflow branch separators in the
		// retired subtree — the interior walk is the ONLY reference
		// holder for these runs (page-formats.md §Overflow-Key Cells;
		// range-delete.md: no overflow page survives the delete of its
		// only referencing entry or separator).
		for i := uint16(0); i < cellCount; i++ {
			c := page.BranchCellAt(buf, cfg, i)
			if err := freeBranchCellExtentIfPresent(pw, cfg, c); err != nil {
				return 0, err
			}
		}
		for _, c := range children {
			n, err := freeSubtreeAt(pw, cfg, c, depth+1)
			if err != nil {
				return 0, err
			}
			count += n
		}
	case page.IsLeafType(typ):
		// Retire every overflow chain owned by this leaf BEFORE
		// retiring the leaf itself. The order matters only for the
		// abstract reachability invariant (a leaf still holds the
		// only references to its overflow pages until it is freed);
		// FreePage / FreeRun are purely bookkeeping so the actual
		// bitmap state is order-independent.
		r := page.NewLeafReader(buf, cfg)
		if err := r.Validate(); err != nil {
			return 0, fmt.Errorf("%w: leaf %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		it := r.IterForReuse(nil, nil, nil)
		// Collect nested-tree roots to recurse into AFTER finishing
		// the leaf walk. Today this is safe-but-not-strictly-needed:
		// `pw.Page(id)` returns a tx-lifetime slice (mmap or slab)
		// that survives any concurrent recursive descent into other
		// pages — no aliasing risk. The deferral is defensive
		// coding against a future LeafIter implementation that
		// retains a borrow into a recycled buffer mid-recursion, OR
		// a future PageWriter that may CoW the leaf's underlying
		// buffer when the recursion frees pages it once shared.
		// Collection cost is bounded by the count of nested-tree
		// cells in this leaf (small in practice).
		var nestedRoots []uint64
		for {
			e, ok := it.Next()
			if !ok {
				break
			}
			if err := freeKeyExtentIfPresent(pw, cfg, e); err != nil {
				return 0, fmt.Errorf("btree: leaf %d: %w", pageID, err)
			}
			switch {
			case e.IsOverflow():
				count++ // one value (the overflow-stored value)
				runLen := page.OverflowRunLength(cfg, e.TotalLen)
				if err := pw.FreeRun(e.OverflowPage, runLen); err != nil {
					return 0, fmt.Errorf("btree: free overflow chain at %d (run=%d) for leaf %d: %w",
						e.OverflowPage, runLen, pageID, err)
				}
			case e.IsNestedTree():
				// Defer the recursive retire until after the leaf
				// walk so any iterator state borrowed from buf
				// (delta-key reconstruction buffers, group caches)
				// is released before the recursive call may CoW or
				// re-resolve pages.
				if e.NestedRoot == 0 {
					return 0, fmt.Errorf("%w: nested-tree cell on leaf %d has NestedRoot=0", ErrCorrupted, pageID)
				}
				nestedRoots = append(nestedRoots, e.NestedRoot)
			case e.IsSubpage():
				// Subpage value count: read directly from the
				// subpage's 2-byte Count header. SubpageReader can be
				// constructed with fixedValueSize=0 because Count is
				// independent of the variable/fixed mode.
				sp := page.NewSubpageReader(e.Value, 0)
				count += uint64(sp.Count())
			default:
				count++ // plain (no flags) key→value pair
			}
		}
		// Recurse into any deferred nested-tree subtrees.
		for _, nr := range nestedRoots {
			n, err := freeSubtreeAt(pw, cfg, nr, depth+1)
			if err != nil {
				return 0, err
			}
			count += n
		}
	default:
		return 0, fmt.Errorf("%w: page %d has unexpected type %d at depth %d (expected branch=%d or a leaf type)",
			ErrCorrupted, pageID, typ, depth, page.TypeBranch)
	}
	// Retire the page itself. Branch-or-leaf, the FreePage contract
	// is the same: same-tx pages enter loosePages; prior-tx pages
	// enter retiredPages (RPL'd at commit).
	if err := pw.FreePage(pageID); err != nil {
		return 0, fmt.Errorf("btree: free subtree page %d at depth %d: %w", pageID, depth, err)
	}
	return count, nil
}

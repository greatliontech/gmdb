package btree

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// FreeSubtree retires every page reachable from rootID. Used by the
// chunk-5.6 DeleteKeyspace path to release a keyspace's data B+tree
// in bulk without per-key Delete cascades. Walks the tree DFS:
// branches recurse into children, leaves enumerate their entries to
// retire overflow chains, then each page (leaf or branch) is
// retired via FreePage.
//
// Empty subtree (rootID == 0) is a no-op — the chunk-5.6 caller may
// pass an unmaterialised keyspace's desc.Root.
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
// On error: some pages may already have been retired. The caller's
// failure path is the existing pager.AbortTx machinery — the
// bitmap snapshot taken at Begin restores the pre-FreeSubtree state
// (retiredPages and loosePages are both cleared and the bitmap bits
// restored). No partial retirement is observable post-AbortTx; no
// new tx-poison machinery is needed.
func FreeSubtree(pw PageWriter, cfg page.Config, rootID uint64) error {
	if rootID == 0 {
		return nil
	}
	return freeSubtreeAt(pw, cfg, rootID, 0)
}

// freeSubtreeAt recursively retires pageID and everything below it.
// depth is the descent depth — bounded by MaxTreeDepth to catch
// cyclic / corrupt trees, same bound as Get / Has.
func freeSubtreeAt(pw PageWriter, cfg page.Config, pageID uint64, depth int) error {
	if depth > MaxTreeDepth {
		return ErrTreeTooDeep
	}
	buf := pw.Page(pageID)
	typ, _, count, _ := page.ReadHeader(buf)
	switch {
	case typ == page.TypeBranch:
		// Recurse into the leftmost child + each cell's right child.
		// page.BranchChildAt uses descent-index semantics: i=0 is the
		// leftmost (Ptr[0]); i ∈ [1, count] are the cell-pointed
		// children — so the valid index range is [0, count], giving
		// count+1 children total.
		//
		// Capture every child id BEFORE the recursive descent so the
		// branch's mmap-resident buffer doesn't need to outlive the
		// recursion.
		children := make([]uint64, 0, int(count)+1)
		for i := uint16(0); i <= count; i++ {
			c := page.BranchChildAt(buf, cfg, i)
			if c == 0 {
				return fmt.Errorf("%w: null child pointer in branch page %d descent index %d at depth %d",
					ErrCorrupted, pageID, i, depth)
			}
			children = append(children, c)
		}
		for _, c := range children {
			if err := freeSubtreeAt(pw, cfg, c, depth+1); err != nil {
				return err
			}
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
			return fmt.Errorf("%w: leaf %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		it := r.IterForReuse(nil, nil, nil)
		for {
			e, ok := it.Next()
			if !ok {
				break
			}
			if e.IsOverflow() {
				runLen := page.OverflowRunLength(cfg, e.TotalLen)
				if err := pw.FreeRun(e.OverflowPage, runLen); err != nil {
					return fmt.Errorf("btree: free overflow chain at %d (run=%d) for leaf %d: %w",
						e.OverflowPage, runLen, pageID, err)
				}
			}
		}
	default:
		return fmt.Errorf("%w: page %d has unexpected type %d at depth %d (expected branch=%d or leaf=%d/%d)",
			ErrCorrupted, pageID, typ, depth, page.TypeBranch, page.TypeLeaf, page.TypeLeafUncompressed)
	}
	// Retire the page itself. Branch-or-leaf, the FreePage contract
	// is the same: same-tx pages enter loosePages; prior-tx pages
	// enter retiredPages (RPL'd at commit).
	if err := pw.FreePage(pageID); err != nil {
		return fmt.Errorf("btree: free subtree page %d at depth %d: %w", pageID, depth, err)
	}
	return nil
}

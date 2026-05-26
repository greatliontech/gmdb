package btree

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// RelocatePages walks the B+tree rooted at root and relocates every page
// for which shouldRelocate(id) reports true to a fresh id from pw's
// allocator, preserving the tree's contents and structure exactly. It is
// the per-tree primitive behind incremental compaction
// (background-maintenance.md §Incremental Compaction): the orchestration
// layer supplies a predicate that selects pages in the region being
// evacuated and re-points the keyspace descriptor at the returned root.
//
// Relocating a node forces every ancestor on its root-path to be CoW'd so
// the child pointers track the new ids — the compaction cascade. The walk
// runs bottom-up: children are processed first, then a node is CoW'd to a
// new id iff it is itself eligible (and budget remains) OR one of its
// children moved (a mandatory pointer fix — skipping it would leave a
// dangling pointer to a freed page). Old pages are retired via
// pw.FreePage (→ RPL for prior-tx pages, loose within this tx).
//
// Bounding: at most maxMoves *eligible* pages are relocated per call.
// Mandatory ancestor pointer-fix CoWs are not counted against maxMoves —
// once a descendant moves, its ancestors must be re-pointed regardless.
// The total CoW count is therefore bounded by maxMoves×(1+depth); the
// caller sizes maxMoves so that fits MaxTxBufferBytes (Inv-M4). A CoW that
// nonetheless overruns the slab budget surfaces ErrTxTooLarge, which the
// caller rolls back — never on-disk corruption.
//
// Convergence: because relocated pages are handed low(er) ids by a
// consolidating allocator, a page moved out of the evacuation region stops
// matching the predicate, so successive passes make monotone progress.
//
// Returns the (possibly new) root id and the count of eligible pages
// moved. A root of 0 (empty tree) is a no-op.
func RelocatePages(pw PageWriter, cfg page.Config, root uint64, shouldRelocate func(id uint64) bool, maxMoves int) (newRoot uint64, moved int, err error) {
	if root == 0 {
		return 0, 0, nil
	}
	budget := maxMoves
	newRoot, _, err = relocateNode(pw, cfg, root, shouldRelocate, &budget, &moved, 0)
	if err != nil {
		return 0, 0, err
	}
	return newRoot, moved, nil
}

// relocateNode processes one page bottom-up. It returns the page's
// (possibly new) id and whether that id changed (movedSelf) so the parent
// can re-point. budget and moved are shared across the whole walk.
func relocateNode(pw PageWriter, cfg page.Config, id uint64, shouldRelocate func(uint64) bool, budget, moved *int, depth int) (newID uint64, movedSelf bool, err error) {
	if depth > MaxTreeDepth {
		return 0, false, ErrTreeTooDeep
	}
	buf, err := pw.Page(id)
	if err != nil {
		return 0, false, err
	}
	typ, _, _, _ := page.ReadHeader(buf)

	if page.IsLeafType(typ) {
		// A leaf has no in-page child pointers to fix. Its overflow refs
		// point to overflow pages, which this primitive does not relocate
		// — they remain valid. So relocation is a verbatim CoW.
		if shouldRelocate(id) && *budget > 0 {
			nid, _, err := relocateVerbatim(pw, id)
			if err != nil {
				return 0, false, err
			}
			*budget--
			*moved++
			return nid, true, nil
		}
		return id, false, nil
	}
	if typ != page.TypeBranch {
		return 0, false, fmt.Errorf("%w: page %d has unexpected type %d during relocation", ErrCorrupted, id, typ)
	}
	if err := validateBranchPage(buf, cfg, id); err != nil {
		return 0, false, err
	}

	// Recurse into every child (slot 0 = leftmost, slot k = cells[k-1]),
	// reading child ids from the still-immutable source buffer. Record the
	// slots whose child moved so the CoW below can re-point them.
	n := page.BranchCellCount(buf)
	type childUpdate struct {
		slot  uint16
		child uint64
	}
	var updates []childUpdate
	for slot := uint16(0); slot <= n; slot++ {
		child := page.BranchChildAt(buf, cfg, slot)
		if child == 0 {
			return 0, false, fmt.Errorf("%w: null child pointer in branch %d during relocation", ErrCorrupted, id)
		}
		nc, cmoved, err := relocateNode(pw, cfg, child, shouldRelocate, budget, moved, depth+1)
		if err != nil {
			return 0, false, err
		}
		if cmoved {
			updates = append(updates, childUpdate{slot: slot, child: nc})
		}
	}

	eligible := shouldRelocate(id) && *budget > 0
	// CoW this branch iff it is itself being relocated OR a child moved
	// (a mandatory pointer fix). The two can co-occur (an eligible branch
	// whose children also moved); it is at most one CoW regardless.
	if !eligible && len(updates) == 0 {
		return id, false, nil
	}
	nid, nbuf, err := relocateVerbatim(pw, id)
	if err != nil {
		return 0, false, err
	}
	for _, u := range updates {
		if u.slot == 0 {
			page.SetBranchLeftmostChild(nbuf, u.child)
		} else {
			page.SetBranchCellChild(nbuf, cfg, u.slot-1, u.child)
		}
	}
	if eligible {
		*budget--
		*moved++
	}
	return nid, true, nil
}

// relocateVerbatim allocates a fresh id, CoW-copies id's content into it,
// retires id, and returns the new id plus the writable destination buffer
// (the caller applies any branch child-pointer fixes to it). The copy is
// byte-for-byte.
func relocateVerbatim(pw PageWriter, id uint64) (uint64, []byte, error) {
	nid, err := pw.AllocPage()
	if err != nil {
		return 0, nil, err
	}
	buf, err := pw.CoW(id, nid)
	if err != nil {
		return 0, nil, err
	}
	if err := pw.FreePage(id); err != nil {
		return 0, nil, err
	}
	return nid, buf, nil
}

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
// Overflow chains owned by a leaf are also relocated when their first page
// is eligible: the runLen-page chain is copied to a fresh contiguous run
// and the owning leaf entry's ref rewritten (which re-encodes the leaf —
// keys unchanged, so still one page). Nested-tree subtrees (a SetKeyspace
// member set promoted to its own B+tree, rooted at a leaf cell's
// NestedRoot — set-keyspace.md §Nested B+tree Reference Cell) are
// relocated recursively: the primitive descends into NestedRoot exactly
// as into any other tree, and rewrites the owning leaf entry's NestedRoot
// when the nested root's id changes (re-encoding the leaf — the NestedRoot
// trailer is a fixed 8-byte field, so the cell keeps its size and the leaf
// still fits one page). NestedCount rides through the re-encode unchanged,
// preserving the keyspace Count-equality contract (set-keyspace.md E1).
// RPL segment pages are deliberately NOT relocated by this primitive —
// they are managed (allocated, chained, reclaimed) by the commit pipeline,
// and rewriting them out-of-band would race that machinery; RPL pages
// instead drain via reclamation and new segments self-place via the
// allocator. The caller's predicate should exclude them.
//
// Bounding: at most maxMoves *eligible* pages are relocated per call
// (a chain counts runLen — and is an indivisible quantum: a single eligible
// chain relocates its whole run even if runLen overshoots the remaining
// budget; a nested-tree subtree is relocated page-by-page like any tree, so
// it consumes budget incrementally and a pass may relocate it only
// partially). Mandatory ancestor pointer-fix CoWs and the leaf
// re-encode forced by a chain or nested-root relocation are not counted
// against maxMoves — once a descendant/chain/nested-root moves, the
// parent/leaf must be rewritten regardless. The total CoW count is
// therefore bounded by ~maxMoves×(1+depth), and the caller sizes maxMoves
// so that fits MaxTxBufferBytes (background-maintenance.md §Invariants). A CoW or slab alloc that
// nonetheless overruns the budget surfaces ErrTxTooLarge, which the caller
// rolls back — never on-disk corruption.
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
		return relocateLeaf(pw, cfg, id, buf, shouldRelocate, budget, moved, depth)
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

// relocateLeaf relocates a leaf together with any of its off-leaf referents
// selected by shouldRelocate: overflow chains and nested-tree subtrees.
//   - An overflow chain whose first page is eligible is copied to a fresh
//     contiguous run and the owning entry's OverflowPage rewritten.
//   - A nested-tree cell's subtree is relocated recursively (relocateNode on
//     NestedRoot — it handles the nested tree's own branches, leaves,
//     overflow chains, and any further nesting); if the nested root's id
//     changes the owning entry's NestedRoot is rewritten. NestedCount is
//     carried through untouched, preserving set-keyspace.md E1.
//
// Either rewrite re-encodes the leaf. Because every rewritten field
// (OverflowPage, NestedRoot) is a fixed 8-byte trailer and all keys are
// unchanged, the re-encoded leaf is byte-for-byte the same size and still
// fits one page (no split). The leaf is also relocated if the leaf page
// itself is eligible. Chain pages (runLen each) and nested-subtree pages
// are counted against budget/moved as they move; a leaf re-encoded solely
// to carry updated refs is mandatory overhead, counted only when the leaf
// itself is eligible. depth bounds the descent across the nesting boundary
// (continued, not reset — matches freeSubtreeAt).
func relocateLeaf(pw PageWriter, cfg page.Config, id uint64, buf []byte, shouldRelocate func(uint64) bool, budget, moved *int, depth int) (uint64, bool, error) {
	entries, err := readLeafEntriesDeepCopy(buf, cfg, id)
	if err != nil {
		return 0, false, err
	}
	refsRewritten := false
	for k := range entries {
		e := &entries[k]
		switch {
		case e.IsOverflow():
			if !shouldRelocate(e.OverflowPage) || *budget <= 0 {
				continue
			}
			runLen := page.OverflowRunLength(cfg, e.TotalLen)
			nf, err := relocateOverflowChain(pw, e.OverflowPage, runLen)
			if err != nil {
				return 0, false, err
			}
			e.OverflowPage = nf
			*budget -= int(runLen)
			*moved += int(runLen)
			refsRewritten = true
		case e.IsNestedTree():
			// A nested-tree cell must carry a non-zero root (same
			// corruption contract enforced by freeSubtreeAt and the
			// Walk* readers). Validate unconditionally — even when the
			// budget can't fund the descent — so a corrupt cell never
			// slips past relocation silently.
			if e.NestedRoot == 0 {
				return 0, false, fmt.Errorf("%w: nested-tree cell on leaf %d has NestedRoot=0", ErrCorrupted, id)
			}
			if *budget <= 0 {
				continue
			}
			nr, nestedMoved, err := relocateNode(pw, cfg, e.NestedRoot, shouldRelocate, budget, moved, depth+1)
			if err != nil {
				return 0, false, err
			}
			if nestedMoved {
				e.NestedRoot = nr
				refsRewritten = true
			}
		}
	}

	leafEligible := shouldRelocate(id) && *budget > 0
	if !refsRewritten && !leafEligible {
		return id, false, nil
	}
	nid, err := pw.AllocPage()
	if err != nil {
		return 0, false, err
	}
	if refsRewritten {
		// Re-encode with the updated overflow / nested-tree refs (keys and
		// every other field unchanged ⇒ fits the same page, no split).
		nbuf, err := pw.ZeroPage(nid)
		if err != nil {
			return 0, false, err
		}
		b := page.NewLeafBuilder(nbuf, cfg)
		for i := range entries {
			if !b.AddEntry(entries[i]) {
				return 0, false, fmt.Errorf("%w: leaf %d overflowed its page when re-encoded during relocation", ErrCorrupted, id)
			}
		}
		b.Finish()
	} else if _, err := pw.CopyPage(id, nid); err != nil {
		return 0, false, err
	}
	if err := pw.FreePage(id); err != nil {
		return 0, false, err
	}
	if leafEligible {
		*budget--
		*moved++
	}
	return nid, true, nil
}

// relocateOverflowChain copies the runLen-page overflow chain at oldFirst to
// a fresh contiguous run, retires the old run, and returns the new first id.
// The copy is byte-for-byte (the stored footer comes along; commit
// recomputes it). pw.Page verifies each source page's footer, so a bitrotted
// chain page aborts relocation (rolled back) rather than propagating.
func relocateOverflowChain(pw PageWriter, oldFirst uint64, runLen uint32) (uint64, error) {
	newFirst, err := pw.AllocContiguous(runLen)
	if err != nil {
		return 0, err
	}
	dst, err := pw.ZeroPageRun(newFirst, runLen)
	if err != nil {
		return 0, err
	}
	for i := range runLen {
		src, err := pw.Page(oldFirst + uint64(i))
		if err != nil {
			return 0, err
		}
		copy(dst[i], src)
	}
	if err := pw.FreeRun(oldFirst, runLen); err != nil {
		return 0, err
	}
	return newFirst, nil
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
	buf, err := pw.CopyPage(id, nid)
	if err != nil {
		return 0, nil, err
	}
	if err := pw.FreePage(id); err != nil {
		return 0, nil, err
	}
	return nid, buf, nil
}

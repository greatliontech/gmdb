package btree

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/page"
)

// PageKind classifies a page visited by Walk.
type PageKind uint8

const (
	// PageKindBranch is an interior B+tree branch page.
	PageKindBranch PageKind = iota
	// PageKindLeaf is a B+tree leaf page.
	PageKindLeaf
	// PageKindOverflow is a FOLLOWER page within an overflow run —
	// pure extent bytes, no header, no footer (page-formats.md
	// §Overflow Page); its integrity is covered by the head's
	// whole-run digest, so per-page verifiers must skip it.
	PageKindOverflow
	// PageKindOverflowHead is the head page of an overflow run — the
	// page carrying the TypeOverflow header, AdditionalPages count,
	// and (when checksums are enabled) the whole-run XXH3-64 digest
	// covering the entire run (checksums.md §Overflow-Run Digest).
	PageKindOverflowHead
)

// VisitFunc is invoked by Walk once per reachable page (branch, leaf,
// or overflow). Returning a non-nil error aborts the walk and Walk
// returns that error verbatim. depth is the descent depth of the page's
// owning B+tree node (overflow pages report their leaf's depth + 1).
type VisitFunc func(pageID uint64, kind PageKind, depth int) error

// Walk performs a read-only depth-first traversal of the B+tree rooted
// at rootID, invoking visit once for every reachable page: each branch
// and leaf page, every page of each leaf entry's overflow run, and —
// recursively — every page of each nested-tree subtree. rootID == 0 is
// a no-op (an unmaterialised tree).
//
// hwm is the snapshot's HighWaterMark; any page id >= hwm (or a
// nested/overflow run extending past it) is rejected as ErrCorrupted
// BEFORE the page is resolved, so a forged child pointer cannot SIGBUS
// by reading past the file's end through the MaxSize-reservation mmap.
// Each page's structure is validated (page.ValidateBranch /
// LeafReader.Validate) before its children are read, so Walk never
// panics on a forged or corrupt page; a structural failure returns a
// wrapped ErrCorrupted and a cycle returns ErrTreeTooDeep.
//
// Walk is the read-only analogue of FreeSubtree, shared by Check (page
// accounting + structural validation) and CopyTo (verbatim-copy page
// enumeration).
func Walk(pr PageReader, cfg page.Config, rootID, hwm uint64, visit VisitFunc) error {
	if rootID == 0 {
		return nil
	}
	return walkAt(pr, cfg, rootID, hwm, 0, visit)
}

// WalkKV enumerates the (key, value) pairs of a plain key→value B+tree
// (the keyspace B+tree, an index registry sub-tree, an index data tree)
// in ascending key order, calling fn for each. It descends with the same
// hwm + ValidateBranch + LeafReader.Validate guards as Walk, so it never
// panics on a forged page — the safe enumeration Check uses on untrusted
// snapshots in place of the (unguarded) read cursor. Overflow values are
// assembled (the run bounded by hwm). A nested-tree or subpage cell —
// unexpected in a plain KV tree — returns a wrapped ErrCorrupted. The
// key/value slices passed to fn are borrowed and valid only for the
// duration of the call; fn must copy anything it retains.
func WalkKV(pr PageReader, cfg page.Config, root, hwm uint64, fn func(key, value []byte) error) error {
	if root == 0 {
		return nil
	}
	keyBuf := make([]byte, 0, 256)
	return walkKVAt(pr, cfg, root, hwm, 0, keyBuf, fn)
}

// WalkLeafEntries walks the B+tree rooted at root in ascending key order
// and calls fn for every leaf ENTRY — including the multi-value cells
// (subpage, nested-tree) that WalkKV rejects. It descends with the same
// hwm + ValidateBranch + LeafReader.Validate guards as Walk / WalkKV, so
// it never panics on a forged page. fn receives the raw page.LeafEntry:
// its Key / Value slices are borrowed and valid only for the duration of
// the call (copy anything retained), overflow values are NOT assembled
// (fn sees the overflow reference), and nested-tree roots are NOT
// recursed (fn sees the nested-tree cell). This is the guarded enumerator
// Check uses for a SetKeyspace's outer tree, whose entries map a set key
// to a subpage or nested-tree of members.
func WalkLeafEntries(pr PageReader, cfg page.Config, root, hwm uint64, fn func(e page.LeafEntry) error) error {
	if root == 0 {
		return nil
	}
	keyBuf := make([]byte, 0, 256)
	return walkLeafEntriesAt(pr, cfg, root, hwm, 0, keyBuf, fn)
}

// walkNode is the one guarded descent skeleton shared by Walk /
// WalkKV / WalkLeafEntries: depth bound, HighWaterMark bound, page
// read, branch validation + null-child capture + recursion, leaf
// validation. onBranch (nil = skip) fires on each validated branch
// before its children; onLeaf receives each validated leaf's reader
// and may re-enter the walk for nested roots (Walk does).
func walkNode(pr PageReader, cfg page.Config, pageID, hwm uint64, depth int,
	onBranch func(pageID uint64, depth int) error,
	onLeaf func(pageID uint64, depth int, r page.LeafReader) error,
) error {
	if depth > MaxTreeDepth {
		return ErrTreeTooDeep
	}
	if pageID >= hwm {
		return fmt.Errorf("%w: page id %d >= HighWaterMark %d at depth %d", ErrCorrupted, pageID, hwm, depth)
	}
	buf, err := pr.Page(pageID)
	if err != nil {
		return err
	}
	typ, _, cellCount, _ := page.ReadHeader(buf)
	switch {
	case page.IsBranchType(typ):
		if err := page.ValidateBranch(buf, cfg); err != nil {
			return fmt.Errorf("%w: branch %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		if onBranch != nil {
			if err := onBranch(pageID, depth); err != nil {
				return err
			}
		}
		// Capture child ids before recursing so the branch buffer need
		// not outlive the descent (mirrors freeSubtreeAt).
		children := make([]uint64, 0, int(cellCount)+1)
		for i := uint16(0); i <= cellCount; i++ {
			c := page.BranchChildAt(buf, cfg, i)
			if c == 0 {
				return fmt.Errorf("%w: null child pointer in branch %d index %d at depth %d", ErrCorrupted, pageID, i, depth)
			}
			children = append(children, c)
		}
		for _, c := range children {
			if err := walkNode(pr, cfg, c, hwm, depth+1, onBranch, onLeaf); err != nil {
				return err
			}
		}
	case page.IsLeafType(typ):
		r := page.NewLeafReader(buf, cfg)
		if err := r.Validate(); err != nil {
			return fmt.Errorf("%w: leaf %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		return onLeaf(pageID, depth, r)
	default:
		return fmt.Errorf("%w: page %d unexpected type %d at depth %d (want a branch or leaf type)",
			ErrCorrupted, pageID, typ, depth)
	}
	return nil
}

func walkLeafEntriesAt(pr PageReader, cfg page.Config, pageID, hwm uint64, depth int, keyBuf []byte, fn func(e page.LeafEntry) error) error {
	return walkNode(pr, cfg, pageID, hwm, depth, nil,
		func(_ uint64, _ int, r page.LeafReader) error {
			it := r.IterForReuse(keyBuf, nil, nil)
			for {
				e, ok := it.Next()
				if !ok {
					return nil
				}
				if err := fn(e); err != nil {
					return err
				}
			}
		})
}

func walkKVAt(pr PageReader, cfg page.Config, pageID, hwm uint64, depth int, keyBuf []byte, fn func(key, value []byte) error) error {
	return walkNode(pr, cfg, pageID, hwm, depth, nil,
		func(leafID uint64, _ int, r page.LeafReader) error {
			it := r.IterForReuse(keyBuf, nil, nil)
			for {
				e, ok := it.Next()
				if !ok {
					return nil
				}
				// WalkKV yields FULL keys: an overflow-key entry's
				// resident bytes are materialized with its extent tail
				// (bounded by hwm like value runs) so consumers — the
				// CopyTo rebuilders, Check's extractor replay — never
				// see a truncated key (page-formats.md §Overflow-Key
				// Cells).
				key := e.Key
				if e.IsOverflowKey() {
					extRun := uint64(keyExtentRunLen(cfg, e.KeyTotalLen))
					if e.KeyExtPage == 0 || e.KeyExtPage >= hwm || e.KeyExtPage+extRun > hwm {
						return fmt.Errorf("%w: key extent [%d,+%d) on leaf %d out of range (hwm=%d)",
							ErrCorrupted, e.KeyExtPage, extRun, leafID, hwm)
					}
					full, kerr := materializeEntryKey(pr, cfg, e)
					if kerr != nil {
						return kerr
					}
					key = full
				}
				switch {
				case e.IsOverflow():
					// Forged-length bound (checksums.md §Structural and Allocation Bounds): uint64 run length — a forged TotalLen whose
					// uint32 run truncates to a small value is caught here
					// (run64 > hwm), before readOverflowValue allocates.
					run64 := page.OverflowRunLength64(cfg, e.TotalLen)
					if e.OverflowPage == 0 || e.OverflowPage >= hwm || e.OverflowPage+run64 > hwm {
						return fmt.Errorf("%w: overflow run [%d,+%d) on leaf %d out of range (hwm=%d)",
							ErrCorrupted, e.OverflowPage, run64, leafID, hwm)
					}
					val, err := readOverflowValue(pr, cfg, e)
					if err != nil {
						return err
					}
					if err := fn(key, val); err != nil {
						return err
					}
				case e.IsNestedTree() || e.IsSubpage():
					return fmt.Errorf("%w: unexpected multi-value cell in plain key→value tree at leaf %d", ErrCorrupted, leafID)
				default:
					if err := fn(key, e.Value); err != nil {
						return err
					}
				}
			}
		})
}

// walkVisitKeyExtent bounds-checks and visits every page of one key
// extent run — the key-half analog of the value-run visit in walkAt,
// with the same header cross-check discipline.
func walkVisitKeyExtent(pr PageReader, cfg page.Config, ownerID uint64, extPage uint64, keyTotalLen uint32, hwm uint64, d int, visit VisitFunc) error {
	extRun := uint64(keyExtentRunLen(cfg, keyTotalLen))
	if extPage == 0 || extPage >= hwm || extPage+extRun > hwm {
		return fmt.Errorf("%w: key extent [%d,+%d) on page %d out of range (hwm=%d)",
			ErrCorrupted, extPage, extRun, ownerID, hwm)
	}
	// Runs are read whole (PageRun — a per-page Page read would
	// footer-verify footer-less run pages); the header cross-check
	// against the reference-derived length stays.
	run, oerr := pr.PageRun(extPage)
	if oerr != nil {
		return oerr
	}
	additional, derr := page.DecodeOverflowFirstPage(run)
	if derr != nil {
		return fmt.Errorf("%w: key extent at %d on page %d: %w", ErrCorrupted, extPage, ownerID, derr)
	}
	if uint64(additional)+1 != extRun {
		return fmt.Errorf("%w: key extent at %d on page %d: header AdditionalPages %d+1 disagrees with the KeyTotalLen-derived run %d",
			ErrCorrupted, extPage, ownerID, additional, extRun)
	}
	if err := visit(extPage, PageKindOverflowHead, d+1); err != nil {
		return err
	}
	for j := uint64(1); j < extRun; j++ {
		if err := visit(extPage+j, PageKindOverflow, d+1); err != nil {
			return err
		}
	}
	return nil
}

func walkAt(pr PageReader, cfg page.Config, pageID, hwm uint64, depth int, visit VisitFunc) error {
	return walkNode(pr, cfg, pageID, hwm, depth,
		func(branchID uint64, d int) error {
			if err := visit(branchID, PageKindBranch, d); err != nil {
				return err
			}
			// Overflow branch separators carry key extents reachable
			// ONLY through this branch — the walk must visit them or
			// Check's accounting reports them leaked (page-formats.md
			// §Overflow-Key Cells, lifecycle).
			buf, err := pr.Page(branchID)
			if err != nil {
				return err
			}
			n := page.BranchCellCount(buf)
			for i := uint16(0); i < n; i++ {
				c := page.BranchCellAt(buf, cfg, i)
				if c.IsOverflowKey() {
					if err := walkVisitKeyExtent(pr, cfg, branchID, c.KeyExtPage, c.KeyTotalLen, hwm, d, visit); err != nil {
						return err
					}
				}
			}
			return nil
		},
		func(leafID uint64, d int, r page.LeafReader) error {
			if err := visit(leafID, PageKindLeaf, d); err != nil {
				return err
			}
			it := r.IterForReuse(nil, nil, nil)
			var nestedRoots []uint64
			for {
				e, ok := it.Next()
				if !ok {
					break
				}
				if e.IsOverflowKey() {
					if err := walkVisitKeyExtent(pr, cfg, leafID, e.KeyExtPage, e.KeyTotalLen, hwm, d, visit); err != nil {
						return err
					}
				}
				switch {
				case e.IsOverflow():
					// Forged-length bound (checksums.md §Structural and Allocation Bounds): uint64 run length (the visit loop is then
					// bounded by hwm, never by a truncated forged run).
					run64 := page.OverflowRunLength64(cfg, e.TotalLen)
					if e.OverflowPage == 0 || e.OverflowPage >= hwm || e.OverflowPage+run64 > hwm {
						return fmt.Errorf("%w: overflow run [%d,+%d) on leaf %d out of range (hwm=%d)",
							ErrCorrupted, e.OverflowPage, run64, leafID, hwm)
					}
					// Cross-check the run's head header against the
					// leaf reference (checksums.md §Structural and
					// Allocation Bounds): the read path rejects a wrong
					// Type or AdditionalPages at the same gate
					// (readRunExtent), so a walk that skipped the
					// header would let Check pass a database clean
					// while every Get of the key fails ErrCorrupted.
					run, oerr := pr.PageRun(e.OverflowPage)
					if oerr != nil {
						return oerr
					}
					additional, derr := page.DecodeOverflowFirstPage(run)
					if derr != nil {
						return fmt.Errorf("%w: overflow run at %d on leaf %d: %w",
							ErrCorrupted, e.OverflowPage, leafID, derr)
					}
					if uint64(additional)+1 != run64 {
						return fmt.Errorf("%w: overflow run at %d on leaf %d: header AdditionalPages %d+1 disagrees with the TotalLen-derived run %d",
							ErrCorrupted, e.OverflowPage, leafID, additional, run64)
					}
					if err := visit(e.OverflowPage, PageKindOverflowHead, d+1); err != nil {
						return err
					}
					for j := uint64(1); j < run64; j++ {
						if err := visit(e.OverflowPage+j, PageKindOverflow, d+1); err != nil {
							return err
						}
					}
				case e.IsNestedTree():
					if e.NestedRoot == 0 {
						return fmt.Errorf("%w: nested-tree cell on leaf %d has NestedRoot=0", ErrCorrupted, leafID)
					}
					nestedRoots = append(nestedRoots, e.NestedRoot)
				}
			}
			for _, nr := range nestedRoots {
				if err := walkAt(pr, cfg, nr, hwm, d+1, visit); err != nil {
					return err
				}
			}
			return nil
		})
}

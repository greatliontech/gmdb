package btree

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// PageKind classifies a page visited by Walk.
type PageKind uint8

const (
	// PageKindBranch is an interior B+tree branch page.
	PageKindBranch PageKind = iota
	// PageKindLeaf is a B+tree leaf page.
	PageKindLeaf
	// PageKindOverflow is one page within a leaf entry's overflow run.
	PageKindOverflow
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

func walkKVAt(pr PageReader, cfg page.Config, pageID, hwm uint64, depth int, keyBuf []byte, fn func(key, value []byte) error) error {
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
	case typ == page.TypeBranch:
		if err := page.ValidateBranch(buf, cfg); err != nil {
			return fmt.Errorf("%w: branch %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		children := make([]uint64, 0, int(cellCount)+1)
		for i := uint16(0); i <= cellCount; i++ {
			c := page.BranchChildAt(buf, cfg, i)
			if c == 0 {
				return fmt.Errorf("%w: null child pointer in branch %d index %d at depth %d", ErrCorrupted, pageID, i, depth)
			}
			children = append(children, c)
		}
		for _, c := range children {
			if err := walkKVAt(pr, cfg, c, hwm, depth+1, keyBuf, fn); err != nil {
				return err
			}
		}
	case page.IsLeafType(typ):
		r := page.NewLeafReader(buf, cfg)
		if err := r.Validate(); err != nil {
			return fmt.Errorf("%w: leaf %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		it := r.IterForReuse(keyBuf, nil, nil)
		for {
			e, ok := it.Next()
			if !ok {
				break
			}
			switch {
			case e.IsOverflow():
				runLen := page.OverflowRunLength(cfg, e.TotalLen)
				if e.OverflowPage == 0 || e.OverflowPage+uint64(runLen) > hwm {
					return fmt.Errorf("%w: overflow run [%d,+%d) on leaf %d out of range (hwm=%d)",
						ErrCorrupted, e.OverflowPage, runLen, pageID, hwm)
				}
				val, err := readOverflowValue(pr, cfg, e)
				if err != nil {
					return err
				}
				if err := fn(e.Key, val); err != nil {
					return err
				}
			case e.IsNestedTree() || e.IsSubpage():
				return fmt.Errorf("%w: unexpected multi-value cell in plain key→value tree at leaf %d", ErrCorrupted, pageID)
			default:
				if err := fn(e.Key, e.Value); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("%w: page %d unexpected type %d at depth %d (want branch=%d or leaf=%d/%d)",
			ErrCorrupted, pageID, typ, depth, page.TypeBranch, page.TypeLeaf, page.TypeLeafUncompressed)
	}
	return nil
}

func walkAt(pr PageReader, cfg page.Config, pageID, hwm uint64, depth int, visit VisitFunc) error {
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
	case typ == page.TypeBranch:
		if err := page.ValidateBranch(buf, cfg); err != nil {
			return fmt.Errorf("%w: branch %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		if err := visit(pageID, PageKindBranch, depth); err != nil {
			return err
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
			if err := walkAt(pr, cfg, c, hwm, depth+1, visit); err != nil {
				return err
			}
		}
	case page.IsLeafType(typ):
		r := page.NewLeafReader(buf, cfg)
		if err := r.Validate(); err != nil {
			return fmt.Errorf("%w: leaf %d at depth %d: %w", ErrCorrupted, pageID, depth, err)
		}
		if err := visit(pageID, PageKindLeaf, depth); err != nil {
			return err
		}
		it := r.IterForReuse(nil, nil, nil)
		var nestedRoots []uint64
		for {
			e, ok := it.Next()
			if !ok {
				break
			}
			switch {
			case e.IsOverflow():
				runLen := page.OverflowRunLength(cfg, e.TotalLen)
				if e.OverflowPage == 0 || e.OverflowPage+uint64(runLen) > hwm {
					return fmt.Errorf("%w: overflow run [%d,+%d) on leaf %d out of range (hwm=%d)",
						ErrCorrupted, e.OverflowPage, runLen, pageID, hwm)
				}
				for j := range runLen {
					if err := visit(e.OverflowPage+uint64(j), PageKindOverflow, depth+1); err != nil {
						return err
					}
				}
			case e.IsNestedTree():
				if e.NestedRoot == 0 {
					return fmt.Errorf("%w: nested-tree cell on leaf %d has NestedRoot=0", ErrCorrupted, pageID)
				}
				nestedRoots = append(nestedRoots, e.NestedRoot)
			}
		}
		for _, nr := range nestedRoots {
			if err := walkAt(pr, cfg, nr, hwm, depth+1, visit); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: page %d unexpected type %d at depth %d (want branch=%d or leaf=%d/%d)",
			ErrCorrupted, pageID, typ, depth, page.TypeBranch, page.TypeLeaf, page.TypeLeafUncompressed)
	}
	return nil
}

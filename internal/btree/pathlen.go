package btree

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/page"
)

// PathLen reports the number of pages on a root-to-leaf descent of
// the tree rooted at root — the tree's uniform height (B+trees keep
// every leaf at the same depth, so the leftmost descent measures all
// of them). Returns 0 for an empty tree (root == 0).
//
// Callers use this to price a same-size upsert exactly: a value
// update that cannot split CoWs precisely one page per path level.
func PathLen(pr PageReader, cfg page.Config, root uint64) (int, error) {
	if root == 0 {
		return 0, nil
	}
	n := 0
	cur := root
	for {
		buf, err := pr.Page(cur)
		if err != nil {
			return 0, err
		}
		n++
		if n > MaxTreeDepth {
			return 0, ErrTreeTooDeep
		}
		typ, _, _, _ := page.ReadHeader(buf)
		if page.IsLeafType(typ) {
			return n, nil
		}
		if typ != page.TypeBranch {
			return 0, fmt.Errorf("%w: page %d has unexpected type %d during PathLen descent", ErrCorrupted, cur, typ)
		}
		if err := validateBranchPage(buf, cfg, cur); err != nil {
			return 0, err
		}
		next := page.BranchChildAt(buf, cfg, 0)
		if next == 0 {
			return 0, fmt.Errorf("%w: null child pointer in branch %d during PathLen descent", ErrCorrupted, cur)
		}
		cur = next
	}
}

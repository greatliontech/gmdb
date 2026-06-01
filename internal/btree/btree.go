package btree

import (
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// PageReader is the read-only page resolution interface the btree
// needs. *pager.Pager satisfies it.
//
// Callers MUST supply a reader whose Page(id) returns a slice of
// length cfg.PageSize. The btree does not re-validate the size — the
// reader's verifying Page enforces the file-resident bound and the
// per-page checksum, and is the boundary at which an out-of-range id
// (Inv-RV3) or a bitrotted page (Inv-RV1) surfaces as an error.
type PageReader interface {
	// Page returns the page bytes at id, or an error. A conforming
	// reader bounds id against the file-resident extent before any
	// access (so a forged/out-of-range id yields ErrCorrupted, never
	// a SIGBUS) and, when checksums are enabled, verifies the
	// per-page checksum footer on first access in the transaction
	// (mismatch yields ErrBadPageChecksum). The returned slice stays
	// valid for the duration of the caller's enclosing transaction;
	// btree reads it only within the call that obtained it.
	Page(id uint64) ([]byte, error)
}

// MaxTreeDepth caps the descent loop to catch malformed cyclic
// branch chains. Real trees never exceed ~10 levels (10 levels at
// 4 KB pages with ~100-byte keys ≈ 100^10 = 10^20 leaves, far
// beyond any single database). The constant is a sanity bound on
// the descent loop, not a hard limit on practical depth.
const MaxTreeDepth = 64

// ErrCorrupted is returned by Get/Has on any structural integrity
// violation observed during descent: bad page type, null child
// pointer, leaf structural fault, cyclic branch chain. The chunk-5
// db-level mapping translates this to gmdb.ErrCorrupted — same
// pattern as pager.ErrCorrupted.
var ErrCorrupted = errors.New("btree: structural corruption detected")

// ErrTreeTooDeep is returned by Get when the descent loop hits
// MaxTreeDepth, indicating the on-disk tree has a cyclic branch
// chain or other structural corruption. Wraps ErrCorrupted so
// callers can errors.Is past either sentinel.
var ErrTreeTooDeep = fmt.Errorf("%w: descent exceeded MaxTreeDepth (cycle or corrupt tree)", ErrCorrupted)

// validateBranchPage validates a branch page's directory structure
// before its separators/children are read — the chunk-4.6β "the first
// resolver validates an arbitrary on-disk page" contract, extended from
// leaves (LeafReader.Validate) to branches. A forged directory (count
// past capacity, a cell offset outside the page) would otherwise make
// BranchSearch / BranchCellAt / BranchChildAt read out of the page's
// bounds; this turns that into a wrapped ErrCorrupted. Cheap (a
// directory scan) and paid once per branch on its first read during a
// descent, mirroring the per-leaf Validate already on the read path.
func validateBranchPage(buf []byte, cfg page.Config, id uint64) error {
	if err := page.ValidateBranch(buf, cfg); err != nil {
		return fmt.Errorf("%w: branch %d: %w", ErrCorrupted, id, err)
	}
	return nil
}

// Get traverses the tree rooted at rootID looking for key. Returns
// the value bytes on hit, (nil, false, nil) on miss, or an error
// on structural corruption (bad page type, null child pointer,
// malformed leaf, cyclic chain).
//
// Empty tree (rootID == 0) returns (nil, false, nil) — the
// convention used by keyspace descriptors for "no entries yet."
//
// Value ownership. For an inline entry the returned slice is
// BORROWED from the leaf page buffer (valid for the tx's
// lifetime), matching api-surface.md §Byte Slice Ownership. For
// an overflow entry the returned slice is HEAP-ALLOCATED — the
// value is assembled from a 1+N-page contiguous run that has
// header / footer gaps between value bytes, so a single
// contiguous mmap slice can't span it. Caller-owned, lifetime is
// caller-controlled.
//
// The PageReader's Page(id) is called at every level: O(depth)
// page resolutions plus 1+N more for an overflow read. With a
// typical depth of 3–5 and 4 KB pages, each inline Get touches a
// small handful of cache-warm pages.
//
// Validation boundary. Every leaf page resolved during descent is
// passed through LeafReader.Validate before SearchLeaf — the
// chunk-4.6β contract per internal/page/leaf.go (NewLeafReader is
// O(1) and assumes structural validity; arbitrary on-disk pages
// must be validated by their first resolver). Any validation
// failure surfaces as ErrCorrupted, preserving the chunk-4.3
// errors.Is contract under the new leaf format.
func Get(pr PageReader, cfg page.Config, rootID uint64, key []byte) ([]byte, bool, error) {
	if rootID == 0 {
		return nil, false, nil
	}
	cur := rootID
	for depth := 0; depth <= MaxTreeDepth; depth++ {
		buf, err := pr.Page(cur)
		if err != nil {
			return nil, false, err
		}
		typ, _, _, _ := page.ReadHeader(buf)
		switch {
		case typ == page.TypeBranch:
			if err := validateBranchPage(buf, cfg, cur); err != nil {
				return nil, false, err
			}
			i := page.BranchSearch(buf, cfg, key)
			next := page.BranchChildAt(buf, cfg, i)
			if next == 0 {
				return nil, false, fmt.Errorf("%w: null child pointer in branch page %d at descent index %d",
					ErrCorrupted, cur, i)
			}
			cur = next
		case page.IsLeafType(typ):
			r := page.NewLeafReader(buf, cfg)
			if err := r.Validate(); err != nil {
				return nil, false, fmt.Errorf("%w: leaf %d: %w", ErrCorrupted, cur, err)
			}
			_, entry, found := r.SearchLeaf(key)
			if !found {
				return nil, false, nil
			}
			if entry.IsOverflow() {
				val, err := readOverflowValue(pr, cfg, entry)
				if err != nil {
					return nil, true, err
				}
				return val, true, nil
			}
			return entry.Value, true, nil
		default:
			return nil, false, fmt.Errorf("%w: page %d has unexpected type %d (expected branch=%d or leaf=%d/%d)",
				ErrCorrupted, cur, typ, page.TypeBranch, page.TypeLeaf, page.TypeLeafUncompressed)
		}
	}
	return nil, false, ErrTreeTooDeep
}

// Has reports whether key is present in the tree rooted at rootID,
// without materialising the value bytes (still calls SearchLeaf
// internally — the optimisation is in the caller's allocation
// avoidance, not in the lookup cost). Returns the same error set
// as Get, sans the overflow-value assembly path.
//
// Overflow entries: Has returns (true, nil) without paying the
// chain read — membership is determinable from the leaf entry's
// presence alone.
func Has(pr PageReader, cfg page.Config, rootID uint64, key []byte) (bool, error) {
	if rootID == 0 {
		return false, nil
	}
	cur := rootID
	for depth := 0; depth <= MaxTreeDepth; depth++ {
		buf, err := pr.Page(cur)
		if err != nil {
			return false, err
		}
		typ, _, _, _ := page.ReadHeader(buf)
		switch {
		case typ == page.TypeBranch:
			if err := validateBranchPage(buf, cfg, cur); err != nil {
				return false, err
			}
			i := page.BranchSearch(buf, cfg, key)
			next := page.BranchChildAt(buf, cfg, i)
			if next == 0 {
				return false, fmt.Errorf("%w: null child pointer in branch page %d at descent index %d",
					ErrCorrupted, cur, i)
			}
			cur = next
		case page.IsLeafType(typ):
			r := page.NewLeafReader(buf, cfg)
			if err := r.Validate(); err != nil {
				return false, fmt.Errorf("%w: leaf %d: %w", ErrCorrupted, cur, err)
			}
			_, _, found := r.SearchLeaf(key)
			return found, nil
		default:
			return false, fmt.Errorf("%w: page %d has unexpected type %d (expected branch=%d or leaf=%d/%d)",
				ErrCorrupted, cur, typ, page.TypeBranch, page.TypeLeaf, page.TypeLeafUncompressed)
		}
	}
	return false, ErrTreeTooDeep
}

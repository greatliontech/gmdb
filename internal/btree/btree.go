package btree

import (
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// PageReader is the read-only page resolution interface the btree
// needs. *pager.Pager satisfies it (Page resolves slab-then-mmap
// for writers, mmap-only for readers).
//
// Callers MUST supply a reader whose Page(id) returns a slice of
// length cfg.PageSize. The btree does not validate the size at
// every call — the pager's Page method panics on out-of-reservation
// access, which is the boundary at which size mismatches surface.
type PageReader interface {
	// Page returns the page bytes at id. The returned slice is
	// valid for the duration of the caller's enclosing tx (per
	// the pager-slab.md byte-slice ownership invariant).
	Page(id uint64) []byte
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

// ErrOverflowValueUnsupported is returned by Get when the matched
// leaf entry is an overflow reference. Overflow value assembly
// lands in chunk 4.7 — until then, the caller surfaces this to the
// user as a "value too large to inline" failure rather than
// silently returning nil. This sentinel is the chunk-4.3-bounded
// surface; replaced by an actual assembly path in 4.7.
var ErrOverflowValueUnsupported = errors.New("btree: overflow value assembly not yet implemented (chunk 4.7)")

// Get traverses the tree rooted at rootID looking for key. Returns
// the value bytes (borrowed from the page buffer, valid for the
// tx's lifetime) on hit; (nil, false, nil) on miss; an error on
// structural corruption (bad page type, null child pointer,
// malformed leaf, cyclic chain, or overflow value).
//
// Empty tree (rootID == 0) returns (nil, false, nil) — the
// convention used by keyspace descriptors for "no entries yet."
//
// The PageReader's Page(id) is called at every level: O(depth)
// page resolutions. With a typical depth of 3–5 and 4 KB pages,
// each Get touches a small handful of cache-warm pages.
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
		buf := pr.Page(cur)
		typ, _, _, _ := page.ReadHeader(buf)
		switch {
		case typ == page.TypeBranch:
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
				return nil, true, ErrOverflowValueUnsupported
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
// as Get.
//
// For an overflow value, Has returns (true, nil) — membership is
// well-defined even without the assembled value, unlike Get which
// must return ErrOverflowValueUnsupported until 4.7.
func Has(pr PageReader, cfg page.Config, rootID uint64, key []byte) (bool, error) {
	_, found, err := Get(pr, cfg, rootID, key)
	if err != nil && errors.Is(err, ErrOverflowValueUnsupported) {
		// Membership is determinable even when value assembly
		// isn't yet implemented.
		return true, nil
	}
	return found, err
}

package btree

// SetKeyspace subpage → nested-B+tree promotion per
// set-keyspace.md §Subpage Promotion Threshold + §Promotion. Fires
// when the SetKeyspace surface detects that inserting a
// new value would push the existing subpage past the
// `page.SubpagePromotionThreshold` byte budget; this file's
// PromoteSubpageToNestedTree implements the 4-step atomic algorithm
// the spec describes:
//
//  1. Build a nested B+tree containing every subpage entry as a
//     regular cell (keys = the set's values, values empty) — packing
//     the leading entries into one freshly-allocated leaf and growing
//     the tree through the ordinary insert path for the remainder, so
//     the result spans as many leaves as the entries' leaf encoding
//     requires.
//  2. Replace the subpage cell with a nested B+tree reference cell
//     (the CALLER's responsibility, post-return — this function only
//     yields (rootID, count) for the new cell).
//  3. Insert the new value into the nested B+tree.
//
// Atomicity (set-keyspace.md entailed invariant E3): on any error,
// returns (0, 0, err) and the caller's tx-abort path retires every
// page allocated by this call via the pager's loose / retired
// bookkeeping. No partial nested tree leaks; the caller is free to
// retry in a fresh tx.

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/page"
)

// PromoteSubpageToNestedTree implements the promotion algorithm. Inputs:
//
//   - subpageBuf: the raw subpage bytes (header + entries) being
//     promoted, as produced by `internal/page.EncodeSubpage` or
//     `SubpageReader.Insert`. Decoded internally; caller does not
//     need to pre-validate (Validate is called).
//   - fixedValueSize: the SetKeyspace's per-descriptor value-stride
//     (0 = variable). Used to decode the subpage; the nested tree
//     itself stores subpage values as full keys regardless of
//     fixedValueSize (the fixed-stride optimization is for in-
//     subpage storage only; the nested B+tree uses ordinary leaf
//     entries with prefix compression per set-keyspace.md §Nested
//     B+tree Reference Cell).
//   - newValue: the value whose insertion triggered promotion. Must
//     not already be present in the subpage (the caller's pre-check
//     should have observed the duplicate via SubpageReader.Search
//     and returned added=false without invoking promotion).
//
// Returns (rootID, count) for the new nested-tree-reference cell the
// caller writes via `LeafBuilder.AddNestedTreeRef`. count = N+1
// where N is the subpage's pre-promotion entry count.
//
// Errors:
//   - ErrCorrupted (wrapped) on a malformed input subpage.
//   - The newValue-already-present case is treated as ErrCorrupted
//     because it indicates the caller bypassed the pre-promotion
//     duplicate check (set-keyspace.md §Invariants sorted-order on the subpage + a Search hit would
//     have short-circuited the SetKeyspace.Put before reaching
//     here).
//   - Any error from `pw.AllocPage` / `pw.AllocSlab` / `Put`
//     bubbles up; the caller's tx-abort frees pages allocated so far.
func PromoteSubpageToNestedTree(
	pw PageWriter,
	cfg page.Config,
	subpageBuf []byte,
	fixedValueSize uint16,
	newValue []byte,
) (rootID uint64, count uint64, err error) {
	sp := page.NewSubpageReader(subpageBuf, fixedValueSize)
	if err := sp.Validate(); err != nil {
		return 0, 0, fmt.Errorf("%w: PromoteSubpageToNestedTree: %w", ErrCorrupted, err)
	}
	// Defense-in-depth: surface the caller-skipped-dedup case before
	// any allocation so a misbehaving SetKeyspace surface produces a
	// deterministic error rather than a tree with a duplicate value.
	if _, found := sp.Search(newValue); found {
		return 0, 0, fmt.Errorf("%w: PromoteSubpageToNestedTree: newValue already present in subpage (caller should have short-circuited Put with added=false)",
			ErrCorrupted)
	}

	// Step 1+2: allocate a new leaf and build it with the subpage's
	// existing entries. We use LeafBuilder directly (rather than N
	// separate Put calls) so the promotion costs a single page alloc
	// instead of N CoWs. The subpage entries are already sorted
	// (set-keyspace.md §Invariants) and deduped, so the builder's sort-order assertion
	// holds by construction.
	newLeafID, err := pw.AllocPage()
	if err != nil {
		return 0, 0, fmt.Errorf("PromoteSubpageToNestedTree: alloc nested-root leaf: %w", err)
	}
	leafBuf, err := pw.ZeroPage(newLeafID)
	if err != nil {
		// AbortTx restores the bitmap snapshot and is sufficient for
		// in-tx cleanup, but the explicit FreePage keeps the failure
		// paths that occur BEFORE any Put has run symmetric: those
		// paths still own newLeafID. Once a Put succeeds, ownership
		// transfers to the tree (Put frees the old root internally on
		// CoW) and the error paths deliberately free nothing — see the
		// ownership-handoff comment below.
		_ = pw.FreePage(newLeafID)
		return 0, 0, fmt.Errorf("PromoteSubpageToNestedTree: alloc nested-root slab: %w", err)
	}
	b := page.NewLeafBuilder(leafBuf, cfg)
	t := cfg.InlineThreshold()
	var copied uint64
	var spill [][]byte
	sp.AllValues(func(v []byte) bool {
		// AllValues yields slices borrowed from subpageBuf;
		// LeafBuilder.AddInline copies the key bytes into the new
		// leaf, so we don't need to copy v here. The builder's
		// next-call sort-order assertion borrows the key slice
		// briefly until the next AddInline, but subpageBuf outlives
		// the loop so the borrow is safe.
		//
		// A threshold-sized subpage does NOT generally fit one leaf:
		// a leaf cell costs >= 7 bytes + member where a subpage entry
		// costs 2 + member (or just the fixed stride), so small
		// members overflow the first leaf long before the subpage
		// budget is exhausted. Entries past the first leaf's capacity
		// spill to the ordinary insert path below, which grows the
		// tree with proper splits — the promotion result is a
		// multi-leaf nested tree whenever the members' leaf encoding
		// requires one (set-keyspace.md §Subpage Promotion Threshold).
		//
		// Over-threshold members (legal in a subpage — limits.md
		// §Maximum Value Size (Set Keyspaces) — up to the promotion
		// budget) MUST take the overflow-key form as nested-tree
		// keys (page-formats.md §Overflow-Key Cells); AddInline
		// would encode an invalid over-T inline key, so they spill
		// to btree.Put, which writes the key extent.
		if len(spill) == 0 && len(v) <= t && b.AddInline(v, nil) {
			copied++
			return true
		}
		spill = append(spill, v)
		return true
	})
	b.Finish()

	// Spill + step 3: grow the tree through btree.Put — the same
	// machinery every SetKeyspace nested insert uses, splits included.
	// Members are unique and sorted (subpage invariants), so each Put
	// is a fresh append-most insert; newValue's position is arbitrary.
	// Ownership handoff: every successful Put below CoWs the current
	// root and FREES it internally — so after the FIRST successful Put,
	// newLeafID is no longer this function's to free (freeing it again
	// would double-free a loose page, or mark a reallocated live page
	// loose). The explicit error-path free applies only while no Put
	// has run; past that point cleanup is wholly the caller's
	// savepoint/tx-abort (set-keyspace.md E3 atomicity).
	root := newLeafID
	putRan := false
	for _, v := range spill {
		nr, existed, perr := PutReportExisting(pw, cfg, root, v, nil)
		if perr != nil {
			if !putRan {
				_ = pw.FreePage(newLeafID)
			}
			return 0, 0, fmt.Errorf("PromoteSubpageToNestedTree: spill subpage entry into nested tree: %w", perr)
		}
		if existed {
			// Structurally impossible — sp.Validate() enforced strict
			// sorted-unique on the whole subpage — but a silent replace
			// here would desync NestedCount from the tree (E-class
			// Count equality), so encode the check like the newValue
			// pre-check above.
			return 0, 0, fmt.Errorf("%w: PromoteSubpageToNestedTree: duplicate subpage entry reached the spill path", ErrCorrupted)
		}
		putRan = true
		root = nr
		copied++
	}
	newRoot, err := Put(pw, cfg, root, newValue, nil)
	if err != nil {
		if !putRan {
			_ = pw.FreePage(newLeafID)
		}
		return 0, 0, fmt.Errorf("PromoteSubpageToNestedTree: insert new value into nested tree: %w", err)
	}
	return newRoot, copied + 1, nil
}

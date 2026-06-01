package btree

// SetKeyspace subpage → nested-B+tree promotion per
// set-keyspace.md §Subpage Promotion Threshold + §Promotion. Fires
// when the SetKeyspace surface (chunk 6.6) detects that inserting a
// new value would push the existing subpage past the
// `page.SubpagePromotionThreshold` byte budget; this file's
// PromoteSubpageToNestedTree implements the 4-step atomic algorithm
// the spec describes:
//
//  1. Allocate a new leaf page for the nested B+tree.
//  2. Copy all subpage entries into the new leaf page as regular
//     cells (where "keys" are the values from the set and "values"
//     are empty).
//  3. Replace the subpage cell with a nested B+tree reference cell
//     (the CALLER's responsibility, post-return — this function only
//     yields (rootID, count) for the new cell).
//  4. Insert the new value into the nested B+tree.
//
// Atomicity (set-keyspace.md entailed invariant E3): on any error,
// returns (0, 0, err) and the caller's tx-abort path retires every
// page allocated by this call via the pager's loose / retired
// bookkeeping. No partial nested tree leaks; the caller is free to
// retry in a fresh tx.

import (
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// PromoteSubpageToNestedTree implements the 4-step promotion. Inputs:
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
//     duplicate check (Inv-2 on the subpage + a Search hit would
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
	// (Inv-2) and deduped, so the builder's sort-order assertion
	// holds by construction.
	newLeafID, err := pw.AllocPage()
	if err != nil {
		return 0, 0, fmt.Errorf("PromoteSubpageToNestedTree: alloc nested-root leaf: %w", err)
	}
	leafBuf, err := pw.ZeroPage(newLeafID)
	if err != nil {
		// AbortTx restores the bitmap snapshot and is sufficient for
		// in-tx cleanup, but the explicit FreePage keeps the
		// promotion function's per-failure-path contract symmetric
		// with the AddInline / Put error branches below: every
		// failure path that occurs AFTER AllocPage explicitly frees
		// newLeafID before returning. This makes a future refactor
		// that swaps PageWriter implementations (or adds a test
		// double without AbortTx semantics) safe by construction.
		_ = pw.FreePage(newLeafID)
		return 0, 0, fmt.Errorf("PromoteSubpageToNestedTree: alloc nested-root slab: %w", err)
	}
	b := page.NewLeafBuilder(leafBuf, cfg)
	var copied uint64
	var addErr error
	sp.AllValues(func(v []byte) bool {
		// AllValues yields slices borrowed from subpageBuf;
		// LeafBuilder.AddInline copies the key bytes into the new
		// leaf, so we don't need to copy v here. The builder's
		// next-call sort-order assertion borrows the key slice
		// briefly until the next AddInline, but subpageBuf outlives
		// the loop so the borrow is safe.
		if !b.AddInline(v, nil) {
			addErr = fmt.Errorf("PromoteSubpageToNestedTree: nested-root leaf overflowed adding entry %d (subpage size %d exceeds 50%% threshold of a single nested leaf)",
				copied, sp.SizeBytes())
			return false
		}
		copied++
		return true
	})
	if addErr != nil {
		_ = pw.FreePage(newLeafID)
		return 0, 0, addErr
	}
	b.Finish()

	// Step 4: insert newValue into the nested tree. btree.Put handles
	// the typical case (in-place splice into the single leaf) and
	// the edge case (leaf overflow → split into branch + two leaves)
	// uniformly. The returned newRoot is the nested tree's new root
	// — either the single leaf we just built (if in-place) or a
	// fresh branch page (if Put triggered a split).
	newRoot, err := Put(pw, cfg, newLeafID, newValue, nil)
	if err != nil {
		// Free the leaf we allocated; any pages Put allocated before
		// failing are managed by its own rollback path. The pager's
		// tx-abort releases the rest.
		_ = pw.FreePage(newLeafID)
		return 0, 0, fmt.Errorf("PromoteSubpageToNestedTree: insert new value into nested tree: %w", err)
	}
	return newRoot, copied + 1, nil
}

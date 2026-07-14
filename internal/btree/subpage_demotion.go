package btree

// SetKeyspace nested-tree → subpage demotion per set-keyspace.md
// §Demotion. Fires when a `SetKeyspace.DeleteValue` reduces a
// nested B+tree to a single leaf page whose values would fit as a
// subpage (below SubpagePromotionThreshold). The
// SetKeyspace surface invokes DemoteNestedTreeIfFits after each
// successful nested-tree btree.Delete; on demoted=true the surface
// replaces the parent cell with a subpage cell and the caller's
// stats reflect a freed nested-root leaf.
//
// Mirror-image of PromoteSubpageToNestedTree: same E3
// atomicity contract — on any error returns (nil, false, err) with
// no observable post-tx-commit state change (pages allocated by
// this call's EncodeSubpage path are heap-only; the freed leaf is
// only Freed on the successful path).

import (
	"fmt"

	"github.com/greatliontech/gmdb/internal/page"
)

// DemoteNestedTreeIfFits inspects the nested tree rooted at rootID
// and demotes it to a subpage if the tree is a single leaf whose
// values fit as a subpage (encoded subpage size ≤
// `page.SubpagePromotionThreshold(cfg)`). Returns:
//
//   - (subpageBytes, true, nil) — demoted. The caller writes
//     subpageBytes via `LeafBuilder.AddSubpage` in the parent leaf;
//     the nested-root leaf has already been FreePage'd. **The
//     caller MUST treat the parent-leaf rewrite as part of the
//     same atomic step**: a return-to-app between this call and
//     the parent-leaf rewrite that does not abort the tx would
//     leave the parent cell dangling at a freed page. Demote +
//     parent-rewrite is a single SetKeyspace.DeleteValue /
//     Cursor.Delete / DeleteRange operation per spec §Invariants
//     E3; any error after this point requires Tx.Rollback to
//     restore the pre-demote state.
//   - (nil, false, nil) — not demoted. Either the nested tree has
//     more than one leaf (root is a branch), OR the single leaf's
//     content exceeds the subpage threshold. Caller keeps the
//     nested-tree-ref cell unchanged (with refreshed NestedCount
//     from the post-delete state).
//   - (nil, false, err) — corruption, or pager-side failure. The
//     caller's tx-abort rolls back any partial state; this function
//     does no allocation, so on error the only side effect is what
//     btree.Delete already did before returning.
//
// fixedValueSize: the SetKeyspace's per-descriptor value-stride
// (0 = variable). Used to compute the subpage encoding for the
// demoted state (the on-disk subpage carries entries in the keyspace's
// declared mode regardless of how the nested tree stored them — the
// nested tree stores members as plain inline or compact empty-value
// cells (set-keyspace.md §Nested B+tree Reference Cell +
// page-formats.md empty-value cell), but the demoted-subpage entries
// must match the keyspace's fixed/variable declaration).
//
// Empty / null root (rootID == 0): the caller's post-delete state
// should never carry a null root for a nested tree that this
// function is asked to inspect. Returns ErrCorrupted defensively.
func DemoteNestedTreeIfFits(
	pw PageWriter,
	cfg page.Config,
	fixedValueSize uint16,
	rootID uint64,
) (subpageBytes []byte, demoted bool, err error) {
	if rootID == 0 {
		return nil, false, fmt.Errorf("%w: DemoteNestedTreeIfFits called with rootID=0", ErrCorrupted)
	}

	buf, err := pw.Page(rootID)
	if err != nil {
		return nil, false, err
	}
	typ, _, _, _ := page.ReadHeader(buf)
	if !page.IsLeafType(typ) {
		// Multi-leaf nested tree (root is a branch) — never a
		// demotion candidate. Return (nil, false, nil) without
		// inspecting further; the caller keeps the nested-tree-ref
		// cell unchanged.
		if !page.IsBranchType(typ) {
			return nil, false, fmt.Errorf("%w: nested-tree root page %d has unexpected type %d (expected branch or leaf)",
				ErrCorrupted, rootID, typ)
		}
		return nil, false, nil
	}

	// Single-leaf nested tree. Decode entries; each entry's KEY is
	// one SetKeyspace value (the nested tree stores values as keys
	// with empty values per set-keyspace.md §Nested B+tree Reference
	// Cell). Build a candidate subpage from those values and compare
	// its size against the threshold.
	r := page.NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		return nil, false, fmt.Errorf("%w: nested-tree leaf %d: %w", ErrCorrupted, rootID, err)
	}

	// Defensive: a zero-entry nested leaf violates set-keyspace.md §Invariants (empty-set ban)
	// ("empty value sets do not exist on disk"). The caller invokes
	// demote only after a successful btree.Delete that left ≥1
	// value; reaching here with Count=0 means the on-disk state is
	// corrupt (or a future caller bypassed the post-delete
	// non-empty pre-condition). Reject before EncodeSubpage
	// silently builds a 4-byte Count=0 subpage and the caller
	// installs an empty cell.
	if r.Count() == 0 {
		return nil, false, fmt.Errorf("%w: nested-tree leaf %d has zero entries (empty sets must not persist)",
			ErrCorrupted, rootID)
	}

	values := make([][]byte, 0, r.Count())
	it := r.IterForReuse(nil, nil, nil)
	for {
		e, ok := it.Next()
		if !ok {
			break
		}
		// Defensive: nested-tree leaves hold members as plain inline
		// or compact empty-value cells (§Nested B+tree Reference Cell
		// + page-formats.md empty-value cell). A nested-tree leaf
		// containing MultiValue or Overflow cells is structural
		// corruption.
		if e.Flags&^page.CellFlagEmptyValue != 0 {
			return nil, false, fmt.Errorf("%w: nested-tree leaf %d entry has unexpected CellFlags 0x%x (expected a plain inline or empty-value cell)",
				ErrCorrupted, rootID, e.Flags)
		}
		// Copy the key bytes — they're borrowed from the leaf's
		// buf, which we're about to FreePage on the successful
		// demotion path; the returned subpage must be independent.
		v := make([]byte, len(e.Key))
		copy(v, e.Key)
		values = append(values, v)
	}

	candidate, err := page.EncodeSubpage(values, fixedValueSize)
	if err != nil {
		// EncodeSubpage's pre-conditions (sorted, deduped,
		// fixedValueSize-uniform) are entailed by the nested tree's
		// own structural invariants — out-of-order or duplicate
		// keys never escape the btree write path; a mismatched-size
		// key on a Kind=1 fixed-size keyspace is fixed-size-stride corruption (set-keyspace.md §Invariants).
		// So any EncodeSubpage error here indicates a corrupt
		// nested leaf — re-wrap as ErrCorrupted so the
		// SetKeyspace surface can distinguish on-disk corruption
		// from user-input errors (ErrSubpageValueSize would
		// otherwise map to the public ErrValueSizeMismatch
		// "user supplied wrong-size value", which is misleading).
		return nil, false, fmt.Errorf("%w: DemoteNestedTreeIfFits: EncodeSubpage for leaf %d (%d values): %w",
			ErrCorrupted, rootID, len(values), err)
	}
	if len(candidate) > page.SubpagePromotionThreshold(cfg) {
		// Single leaf but doesn't fit as subpage — keep as nested
		// tree. No state change.
		return nil, false, nil
	}

	// Demote: free the nested-root leaf, return the subpage bytes
	// for the caller to install in the parent cell.
	if err := pw.FreePage(rootID); err != nil {
		return nil, false, fmt.Errorf("DemoteNestedTreeIfFits: free nested-root leaf %d: %w", rootID, err)
	}
	return candidate, true, nil
}

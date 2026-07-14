package page

// PatchRefs rewrites the page-reference field of every overflow and
// nested-tree cell in place — OverflowPage for overflow cells,
// NestedRoot for nested-tree cells — without re-encoding the leaf.
// Both wire forms end with the same two fixed-width u64s
// ([Ref u64][TotalLen/Count u64], see decodeFullKeyEntry /
// decodeDeltaEntry), so the rewrite is size-identical by construction:
// keys, values, group structure, and the variable-length fields are
// untouched. This is the relocation primitive — moving a cell's
// overflow chain or nested-tree root changes only where the u64 points,
// never how the cell encodes — and it works on any leaf variant,
// including pages whose on-disk group structure predates the current
// RestartGroupTarget config (a canonical re-encode of such a page can
// GROW; a ref patch cannot).
//
// refAt is called once per overflow/nested cell in entry order with the
// decoded entry (Key/Value borrow per the reader's usual rules, valid
// only during the call) and must return the cell's page reference —
// e.OverflowPage / e.NestedRoot unchanged, or the relocated value. The
// second u64 (TotalLen / NestedCount) is immutable: relocation moves
// pages, never sizes.
//
// The receiver's buffer must be writable (a CoW copy) and structurally
// valid — PatchRefs uses the unchecked hot-path decoders under the same
// Validate trust boundary as every other read path.
func (r LeafReader) PatchRefs(refAt func(idx int, e LeafEntry) uint64) {
	r.patchEntryRefs(refAt, nil)
}

// PatchKeyExtRefs rewrites the KeyExtPage field of every overflow-key
// cell in place — the key-half analog of PatchRefs, with the same
// size-identical guarantee (the reference is a fixed u64 at a
// header-computable offset after the resident key bytes; KeyTotalLen
// is immutable — relocation moves pages, never sizes). keyExtAt is
// called once per overflow-key cell in entry order and returns the
// cell's key-extent first page — e.KeyExtPage unchanged, or the
// relocated value. May be combined with PatchRefs on the same page
// (the two patch disjoint fields).
func (r LeafReader) PatchKeyExtRefs(keyExtAt func(idx int, e LeafEntry) uint64) {
	r.patchEntryRefs(nil, keyExtAt)
}

func (r LeafReader) patchEntryRefs(refAt, keyExtAt func(idx int, e LeafEntry) uint64) {
	patch := func(idx int, e LeafEntry, nextOff int) {
		if refAt != nil && (e.Flags&CellFlagOverflow != 0 || e.IsNestedTree()) {
			le.PutUint64(r.buf[nextOff-16:], refAt(idx, e))
		}
		if keyExtAt != nil && e.IsOverflowKey() {
			// The key-extent reference sits immediately after the
			// resident key bytes: [KeyExtPage u64][KeyTotalLen u32],
			// followed by the value half (16-byte trailer for
			// overflow/nested forms; value bytes for inline; nothing
			// for empty-value). Compute its offset back from nextOff.
			valueHalf := 0
			switch {
			case cellHasTrailerOnly(e.Flags):
				valueHalf = 16
			case e.Flags&CellFlagEmptyValue != 0:
				valueHalf = 0
			default:
				valueHalf = len(e.Value)
			}
			le.PutUint64(r.buf[nextOff-valueHalf-12:], keyExtAt(idx, e))
		}
	}

	if r.seg() {
		// Segregated: references live in the value region at fixed
		// offsets from each entry's VOff; dedicated walk.
		r.segPatchRefs(refAt, keyExtAt)
		return
	}
	if r.uc() {
		for i := 0; i < r.count; i++ {
			e, next := r.decodeFullKeyEntry(r.ucOffset(i))
			patch(i, e, next)
		}
		return
	}
	var keyBuf []byte
	idx := 0
	for g := 0; g < r.rt.RestartCount(); g++ {
		off := r.rt.Offset(g)
		e, next := r.decodeFullKeyEntry(off)
		patch(idx, e, next)
		prevKey := e.Key
		idx++
		off = next
		for k := 1; k < r.rt.GroupEntryCount(g); k++ {
			var de LeafEntry
			de, next, keyBuf = r.decodeDeltaEntry(off, prevKey, keyBuf)
			patch(idx, de, next)
			prevKey = keyBuf
			idx++
			off = next
		}
	}
}

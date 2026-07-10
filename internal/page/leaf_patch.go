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
	patch := func(idx int, e LeafEntry, nextOff int) {
		if e.Flags&CellFlagOverflow == 0 && !e.IsNestedTree() {
			return
		}
		le.PutUint64(r.buf[nextOff-16:], refAt(idx, e))
	}

	if !r.compressed {
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

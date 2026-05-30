package page

// In-place leaf splice helpers — the no-decode fast path for leaf
// insert / delete / split (page-formats.md §Insert and Delete). Each helper
// mutates a CoW'd page buffer directly when the resulting layout fits,
// shifting bytes and patching the restart table instead of decoding and
// re-encoding the whole leaf via LeafBuilder.
//
// **Fast-path + fallback.** Every helper is a strict fast path: it returns
// false (leaving the page byte-unchanged) on any case it does not handle,
// and the caller falls back to the decode/re-encode path. Correctness never
// depends on the splice.
//
// **Determinism (load-bearing).** A successful splice produces bytes
// IDENTICAL to a LeafBuilder rebuild of the same logical entries under the
// config the page was built with — the page-formats.md §Leaf Split
// deterministic-encoding invariant (which is itself stated per fixed
// RestartGroupTarget). In the common case the keyspace's RGT is unchanged
// since the page was built, so this is byte-identity to a rebuild under the
// current cfg; that is what lets Check() repair and recovery re-encode a
// spliced page and get the same bytes the original writer produced.
//
// The one wrinkle is a mid-life RestartGroupTarget change (Tx.SetKeyspaceConfig):
//   - Across the compressed↔uncompressed boundary (RGT 1 ↔ ≥2): TryAppend
//     declines, so the leaf migrates via the rebuild fallback (see TryAppend).
//   - Within the compressed range (e.g. 16→8): the splice keeps the page's
//     existing group structure and encodes the new entry per the new target.
//     Per keyspaces.md, existing leaves keep their stored structure and the
//     RGT is only a builder hint for entries written after the change, so a
//     mix is expected during a transition. Such a page is valid and
//     read-correct but is not byte-reproduced by a fresh full rebuild under
//     either RGT until the leaf is next split / merged / rebuilt. Nothing
//     relies on fresh-rebuild reproducibility across an RGT change (Check()
//     validates structurally rather than re-encoding leaves).
//
// Two mechanisms the helpers rely on for the byte-identity that does hold:
//
//   - Entry bytes are written via the shared writeCompressed{Restart,Delta}-
//     Entry encoders (leaf_builder.go), so the splice and the builder never
//     diverge on per-entry layout.
//   - The free-space region [DataEnd, restart-table-start) of an input page
//     is all-zero (LeafBuilder.Finish zeroes it). A splice only consumes
//     zeroed free space and only shifts the restart table into bytes that
//     were already zero, so the region stays zero with no explicit pass —
//     matching the builder, which zeroes it. (Holds across repeated splices:
//     each splice leaves a zeroed free-space region for the next.)
//
// The helpers assume the page was produced by the current LeafBuilder (true
// for every leaf in a gmdb tree — a single-encoder, pre-v1 deployment). They
// do not bound-check structurally; the caller validates the page before the
// mutation phase, exactly as the decode path does.

// TryAppend appends e to an existing leaf in place when e.Key sorts after
// every key already on the page (the caller MUST establish this — it is the
// leaf's insertion point at index Count()). prevKey is the leaf's current
// last key, used for prefix-delta encoding on a compressed leaf.
//
// Returns true on a successful in-place append; false (page byte-unchanged)
// when the page is empty, the entry does not fit, or the variant has no
// append splice yet, in which case the caller falls back to decode +
// re-encode.
func TryAppend(buf []byte, cfg Config, e LeafEntry, prevKey []byte) bool {
	typ, _, count, _ := ReadHeader(buf)
	if count == 0 {
		// No last entry to append after / no last group to extend; the
		// genesis and empty-leaf cases build via LeafBuilder, not the splice.
		return false
	}
	// Splice only a compressed leaf whose variant still matches the keyspace's
	// configured variant. Two declines route to the decode/re-encode fallback:
	//
	//   - typ != TypeLeaf (uncompressed page): the uncompressed append splice
	//     lands in a later chunk (port order: compressed ops first).
	//   - compressed page but RGT==1 now selects the uncompressed variant:
	//     RestartGroupTarget was changed across the compressed↔uncompressed
	//     boundary since this leaf was built. The leaf must migrate to the new
	//     variant through the rebuild fallback — keyspaces.md: existing leaves
	//     migrate "when they next split, merge, or are rebuilt" (the slow-path
	//     Put is a rebuild). Splicing the stale variant would defeat the
	//     configured variant and build degenerate single-entry restart groups
	//     (every appended entry its own group, since target==1).
	//
	// A within-compressed RGT change (e.g. 16→8, still compressed) is NOT
	// declined: the splice keeps the existing groups and encodes the new entry
	// per the new target — spec-consistent ("existing leaves keep their stored
	// group structure; the new value is a builder hint for leaves written
	// after the change"). The resulting page is valid and read-correct but is
	// not what a fresh full rebuild would produce until the leaf is next
	// rebuilt; see the determinism note in the file header.
	if typ != TypeLeaf || cfg.EffectiveRestartGroupTarget() == 1 {
		return false
	}
	return tryAppendCompressed(buf, cfg, e, prevKey)
}

// tryAppendCompressed appends e to a compressed leaf in place. e.Key must
// sort after every existing key; prevKey is the leaf's last key. Returns
// false (page byte-unchanged) when the entry does not fit.
//
// The append either extends the last restart group with a delta entry or
// opens a new group with a full-key restart entry. The group decision
// mirrors LeafBuilder.addCompressedEntry exactly: a new group opens when the
// last group is at the RestartGroupTarget cap OR the new key shares no prefix
// with prevKey (the "natural break" heuristic, page-formats.md §Compressed
// Leaf). Matching that decision is what keeps the spliced page byte-identical
// to a builder rebuild — a verbatim port without the natural-break clause
// would diverge whenever a zero-shared-prefix key lands in a non-full group.
func tryAppendCompressed(buf []byte, cfg Config, e LeafEntry, prevKey []byte) bool {
	_, _, count, _ := ReadHeader(buf)
	rc := int(le.Uint16(buf[leafOffRestartCount:]))
	contentEnd := cfg.ContentEnd()
	dataEnd := int(le.Uint16(buf[leafOffDataEnd:]))
	restartTableOff := contentEnd - rc*restartTableEntrySize

	lastGroupOff := restartTableOff + (rc-1)*restartTableEntrySize
	lastGroupCount := int(buf[lastGroupOff+2])
	target := int(cfg.EffectiveRestartGroupTarget())

	// Group decision — identical to the builder. shared doubles as the
	// delta's SharedLen when we extend the last group.
	shared := sharedPrefixLen(prevKey, e.Key)
	isRestart := lastGroupCount >= target || shared == 0

	// Entry size (header + value part). valuePartSize handles the inline vs
	// 16-byte-trailer (overflow / nested-tree) split.
	var entrySize int
	if isRestart {
		entrySize = 1 + 2 + len(e.Key) // Flags + KeyLen + Key
	} else {
		entrySize = 1 + 2 + 2 + (len(e.Key) - shared) // Flags + SharedLen + UnsharedLen + UnsharedKey
	}
	entrySize += valuePartSize(e.Flags, e.Value)

	newRC := rc
	if isRestart {
		newRC++
	}
	// Fit check — identical to LeafBuilder.addCompressedEntry's. Must fire
	// before any write so a decline leaves the page byte-unchanged.
	if dataEnd+entrySize+newRC*restartTableEntrySize > contentEnd {
		return false
	}

	// Map e's trailer fields the same way LeafBuilder.AddEntry does:
	// (OverflowPage, TotalLen) for overflow cells, (NestedRoot, NestedCount)
	// for nested-tree cells. Unused for inline / subpage cells.
	ovflPage, totalLen := e.OverflowPage, e.TotalLen
	if e.IsNestedTree() {
		ovflPage, totalLen = e.NestedRoot, e.NestedCount
	}

	// Write the new entry at the old DataEnd (the start of free space).
	var wp int
	if isRestart {
		wp = writeCompressedRestartEntry(buf, dataEnd, e.Flags, e.Key, e.Value, ovflPage, totalLen)
	} else {
		wp = writeCompressedDeltaEntry(buf, dataEnd, e.Flags, shared, e.Key[shared:], e.Value, ovflPage, totalLen)
	}

	// Patch the restart table.
	if isRestart {
		// Grow the table by one entry: shift the existing rc entries 4 bytes
		// toward DataEnd (into formerly-zero free space) preserving order,
		// then write the new group's slot at the high end. The new group's
		// first entry is the restart we just wrote, at the old DataEnd.
		newRestartTableOff := contentEnd - newRC*restartTableEntrySize
		copy(buf[newRestartTableOff:], buf[restartTableOff:restartTableOff+rc*restartTableEntrySize])
		newSlot := newRestartTableOff + rc*restartTableEntrySize
		le.PutUint16(buf[newSlot:], uint16(dataEnd)) // Offset of the new group's first entry
		buf[newSlot+2] = 1                           // Count
		buf[newSlot+3] = 0                           // Reserved
	} else {
		// Extend the last group: bump its Count (a uint8). This branch only
		// runs when !isRestart, i.e. lastGroupCount < target, and target ≤
		// MaxRestartGroupTarget (255) by Config.Validate — so lastGroupCount+1
		// ≤ target ≤ 255, no overflow.
		buf[lastGroupOff+2] = uint8(lastGroupCount + 1)
	}

	// Header: bump Count, update RestartCount + DataEnd. Mirrors the writes
	// in LeafBuilder.Finish so the page matches a rebuild.
	WriteHeader(buf, TypeLeaf, count+1, 0)
	le.PutUint16(buf[leafOffRestartCount:], uint16(newRC))
	le.PutUint16(buf[leafOffDataEnd:], uint16(wp))

	return true
}

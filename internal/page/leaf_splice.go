package page

import "bytes"

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
// **Determinism / correctness of a successful splice** — what it guarantees
// differs by op:
//
//   - TryAppend yields bytes IDENTICAL to a LeafBuilder rebuild of the same
//     entries under the config the page was built with: an append matches the
//     builder's own last-entry behavior, so the page stays a builder
//     fixed-point. This is the page-formats.md §Leaf Split deterministic-
//     encoding invariant (stated per fixed RestartGroupTarget). The one wrinkle
//     is a mid-life RGT change (Tx.SetKeyspaceConfig): a compressed↔uncompressed
//     boundary (RGT 1↔≥2) makes TryAppend decline so the leaf migrates via
//     rebuild; a within-compressed change leaves an append non-fixed-point
//     until the leaf is next rebuilt — both spec-allowed (keyspaces.md:
//     existing leaves keep their structure; RGT is a builder hint).
//
//   - TryInsertAt is LOCALIZED: it grows the containing group in place (up to
//     min(2*target, 255)) and re-encodes only the displaced successor. The
//     builder fills groups to `target` with no balancing, so an "insert ==
//     rebuild" splice could almost never fire (every full group would decline);
//     the splice therefore produces a valid compressed leaf that a fresh
//     rebuild would group differently — sanctioned by §Insert and Delete (an
//     insert "may shift the containing group's boundaries"; RGT is a hint,
//     Count ∈ [1,255]). Its guarantee is SEMANTIC + STRUCTURAL (decodes to the
//     correct entry sequence; passes Validate), not byte-identity.
//
//   - TryDeleteAt is likewise LOCALIZED: it shrinks the containing group in
//     place, re-encoding at most the displaced successor (promoted to the
//     group's restart when the deleted entry was at p==0, or re-deltaed against
//     the new predecessor). A fresh rebuild would re-pack the survivors into
//     full groups, so — like TryInsertAt — its guarantee is SEMANTIC +
//     STRUCTURAL, not byte-identity. A delete ALWAYS shrinks the page (the
//     removed entry outweighs the successor's re-encode growth, by the
//     shared-prefix triangle inequality on sorted keys), so it never declines
//     for lack of space.
//
// Either way Check() validates structurally rather than re-encoding leaves, so
// it accepts localized / post-RGT-change pages; the determinism invariant
// governs builder output (BulkLoad / split).
//
// Two mechanisms both ops rely on:
//
//   - Entry bytes are written via the shared writeCompressed{Restart,Delta}-
//     Entry encoders (leaf_builder.go), so splice and builder never diverge on
//     per-entry layout.
//   - Free space [DataEnd, restart-table-start) stays zeroed: an input page has
//     it zeroed (LeafBuilder.Finish); a splice consumes only zeroed bytes /
//     shifts into them, and re-zeroes any tail freed by a net shrink — so the
//     region stays zero with no full pass, across repeated splices.
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

// TryInsertAt inserts e at absolute index insertIdx of an existing leaf in
// place — the mid-page insert fast path. The caller MUST establish that
// insertIdx is e.Key's sorted position and the key is absent (an append is
// insertIdx == Count(), handled by TryAppend; a replace is a different path).
//
// Returns true on a successful in-place insert; false (page byte-unchanged)
// when the page is empty, the variant doesn't match (see TryAppend), the
// target group is at its growth cap, or the entry doesn't fit — the caller
// then falls back to decode + re-encode.
//
// Unlike TryAppend this is a LOCALIZED splice: it grows the containing group
// past RestartGroupTarget (up to min(2*target, 255)). The builder fills groups
// to the target with no balancing, so a canonical "insert == rebuild" splice
// could almost never fire (every full group would decline). The result is a
// valid compressed leaf that a fresh rebuild would group differently — see the
// file-header determinism note; its guarantee is semantic + structural, not
// byte-identity.
func TryInsertAt(buf []byte, cfg Config, insertIdx int, e LeafEntry) bool {
	typ, _, count, _ := ReadHeader(buf)
	if count == 0 {
		return false
	}
	// Same variant gate as TryAppend: only splice a compressed leaf whose
	// variant still matches the configured one; otherwise migrate via rebuild.
	if typ != TypeLeaf || cfg.EffectiveRestartGroupTarget() == 1 {
		return false
	}
	return tryInsertAtCompressed(buf, cfg, insertIdx, e)
}

// tryInsertAtCompressed inserts e at insertIdx in a compressed leaf by growing
// the containing restart group: it writes the new entry and re-encodes only
// the displaced successor (now a delta against the new key); every other entry
// keeps its bytes. Returns false (page unchanged) on the growth cap or fit.
//
// Three insert positions within the target group (p = position in group,
// gc = group count), via the >= group-find rule that routes a group-boundary
// insert into the EARLIER group as p == gc:
//   - I-B: p == 0 (only when insertIdx == 0) → new entry becomes the group's
//     restart; the old restart E[0] is re-encoded as a delta against it.
//   - I-C: 0 < p < gc → new entry is a delta vs E[p-1]; the successor E[p] is
//     re-encoded as a delta vs the new key.
//   - I-D: p == gc → new entry appended at the group's tail as a delta vs
//     E[gc-1]; no successor to re-encode.
func tryInsertAtCompressed(buf []byte, cfg Config, insertIdx int, e LeafEntry) bool {
	r := NewLeafReader(buf, cfg)
	rc := r.rt.RestartCount()
	contentEnd := cfg.ContentEnd()
	dataEnd := r.DataEnd()
	restartTableOff := contentEnd - rc*restartTableEntrySize
	target := int(cfg.EffectiveRestartGroupTarget())

	// Find the group containing insertIdx. The >= rule routes an insert at a
	// group boundary into the earlier group (p == gc, the I-D case), so p == 0
	// (I-B) occurs only for insertIdx == 0.
	accum, targetGroup, p, gc := 0, rc-1, 0, 0
	for g := range rc {
		c := r.rt.GroupEntryCount(g)
		if accum+c >= insertIdx {
			targetGroup, p, gc = g, insertIdx-accum, c
			break
		}
		accum += c
	}

	// Group-growth cap: a group may grow to min(2*target, 255) via inserts
	// before a decline forces the rebuild to rebalance it. 255 is the hard
	// uint8 Count limit (page-formats.md §Compressed Leaf); 2*target is the
	// rebalancing policy (matches grove). Beyond it, decline → rebuild.
	maxGroup := min(2*target, 255)
	if gc+1 > maxGroup {
		return false
	}

	// Walk the target group from its restart, reconstructing keys, to capture
	// newShared (E[p-1] vs new key; unused at p == 0) and the successor E[p]
	// (its flags, full key, value/trailer, byte extent) so it can be re-encoded
	// as a delta vs the new key. succKey / succVal are cloned because the
	// running keyBuf is reused across delta decodes.
	var (
		newShared, succShared int
		succKey, succVal      []byte
		succFlags             uint8
		succT0, succT1        uint64
		spliceOff, succEnd    int
		hasSucc               bool
	)
	keyBuf := make([]byte, 0, 64)
	walkOff := r.rt.Offset(targetGroup)
	var prevKey []byte
	// Walk to the successor at i==p; the i<gc bound caps the walk at gc-1 in
	// the I-D case (p==gc), where there is no successor entry to decode.
	for i := 0; i <= p && i < gc; i++ {
		entryStart := walkOff
		var ent LeafEntry
		if i == 0 {
			ent, walkOff = r.decodeRestartEntry(walkOff)
		} else {
			ent, walkOff, keyBuf = r.decodeDeltaEntry(walkOff, prevKey, keyBuf)
		}
		if i == p-1 {
			newShared = sharedPrefixLen(ent.Key, e.Key)
		}
		if i == p {
			spliceOff, succEnd, hasSucc = entryStart, walkOff, true
			succFlags = ent.Flags
			succKey = bytes.Clone(ent.Key)
			succShared = sharedPrefixLen(e.Key, ent.Key)
			if cellHasTrailerOnly(ent.Flags) {
				succT0, succT1 = entryTrailer(ent)
			} else {
				succVal = bytes.Clone(ent.Value)
			}
		}
		prevKey = ent.Key
	}
	if p == gc {
		// I-D: splice just past the group's last entry; no successor.
		spliceOff, succEnd = walkOff, walkOff
	}

	// Sizes. The new entry is a restart (full key) at p == 0, else a delta vs
	// E[p-1]; the successor (if any) becomes a delta vs the new key.
	var newEntrySize int
	if p == 0 {
		newEntrySize = 1 + 2 + len(e.Key) + valuePartSize(e.Flags, e.Value)
	} else {
		newEntrySize = 1 + 2 + 2 + (len(e.Key) - newShared) + valuePartSize(e.Flags, e.Value)
	}
	newSuccSize := 0
	if hasSucc {
		newSuccSize = 1 + 2 + 2 + (len(succKey) - succShared) + valuePartSize(succFlags, succVal)
	}
	oldSuccSize := succEnd - spliceOff // 0 for I-D
	byteDelta := newEntrySize + newSuccSize - oldSuccSize
	newDataEnd := dataEnd + byteDelta
	if newDataEnd+rc*restartTableEntrySize > contentEnd {
		return false
	}

	// Shift everything after the old successor by byteDelta in ONE contiguous
	// move: the trailing in-group entries E[p+1..] and all later groups shift by
	// the same delta (their bytes are unchanged — E[p+1]'s predecessor E[p]'s
	// full key is unchanged, so its delta is too). copy is memmove (handles
	// overlap and either sign of byteDelta). Must run BEFORE the writes below,
	// which would otherwise clobber the source tail when growing.
	if byteDelta != 0 {
		copy(buf[succEnd+byteDelta:newDataEnd], buf[succEnd:dataEnd])
	}

	// Write the new entry at spliceOff, then the re-encoded successor right
	// after, via the shared entry-writers (gmdb's ValueLen-before-key order).
	newT0, newT1 := entryTrailer(e)
	var off int
	if p == 0 {
		off = writeCompressedRestartEntry(buf, spliceOff, e.Flags, e.Key, e.Value, newT0, newT1)
	} else {
		off = writeCompressedDeltaEntry(buf, spliceOff, e.Flags, newShared, e.Key[newShared:], e.Value, newT0, newT1)
	}
	if hasSucc {
		writeCompressedDeltaEntry(buf, off, succFlags, succShared, succKey[succShared:], succVal, succT0, succT1)
	}

	// A net shrink (byteDelta < 0: the successor's re-delta saved more than the
	// new entry cost) frees tail bytes — zero them so free space stays zeroed.
	if newDataEnd < dataEnd {
		clear(buf[newDataEnd:dataEnd])
	}

	// Patch the restart table: bump the target group's Count, shift every later
	// group's Offset by byteDelta. RestartCount is unchanged (an insert grows a
	// group, never adds one), so the table itself does not move.
	buf[restartTableOff+targetGroup*restartTableEntrySize+2] = uint8(gc + 1)
	for g := targetGroup + 1; g < rc; g++ {
		off := restartTableOff + g*restartTableEntrySize
		le.PutUint16(buf[off:], uint16(int(le.Uint16(buf[off:]))+byteDelta))
	}

	_, _, count, _ := ReadHeader(buf)
	WriteHeader(buf, TypeLeaf, count+1, 0)
	le.PutUint16(buf[leafOffDataEnd:], uint16(newDataEnd))
	return true
}

// TryDeleteAt removes the entry at absolute index deleteIdx from an existing
// leaf in place — the delete fast path. The caller MUST establish that
// deleteIdx is a present entry's index (e.g. via SearchLeaf).
//
// Returns true on a successful in-place delete; false (page byte-unchanged)
// when the page would become empty (count <= 1) or the variant doesn't match
// (uncompressed, or a compressed page reconfigured to RGT==1 — see TryAppend),
// in which case the caller falls back to decode + re-encode.
//
// gmdb signals a decline with a plain bool, where grove's tryDeleteAtCompressed
// returns an int (freed space, or -1 = "page would empty, caller frees it").
// grove needs the int because it has NO decode-rebuild fallback for delete — its
// caller must learn the empty case. gmdb's delete caller (deleteFromLeaf) DOES
// have that fallback: it already frees an emptied leaf and migrates a stale
// variant on rebuild, and recomputes underflow from the page (leafUnderflow), so
// neither the freed-space value nor an explicit empty code is needed here. This
// keeps TryDeleteAt's signature uniform with TryAppend / TryInsertAt.
//
// Like TryInsertAt this is a LOCALIZED splice (it shrinks one group; a fresh
// rebuild would re-pack the survivors), so a successful splice's guarantee is
// semantic + structural, not byte-identity — see the file-header note.
func TryDeleteAt(buf []byte, cfg Config, deleteIdx int) bool {
	typ, _, count, _ := ReadHeader(buf)
	if count <= 1 {
		// The page would become empty. The caller's decode-rebuild fallback
		// retires the leaf (rebuildLeafAfterDelete with an empty entry set); the
		// splice has no emptied-page form to produce, so decline.
		return false
	}
	// Same variant gate as TryAppend / TryInsertAt: only splice a compressed leaf
	// whose variant still matches the configured one; otherwise the leaf migrates
	// to the configured variant through the rebuild fallback.
	if typ != TypeLeaf || cfg.EffectiveRestartGroupTarget() == 1 {
		return false
	}
	return tryDeleteAtCompressed(buf, cfg, deleteIdx)
}

// tryDeleteAtCompressed removes the entry at deleteIdx from a compressed leaf by
// shrinking its containing restart group: it re-encodes at most the displaced
// successor and shifts the trailing bytes left; every other entry keeps its
// bytes. The page ALWAYS shrinks, so there is no fit-check and it always returns
// true: the removed entry's bytes (≥ 5 header bytes + value part) outweigh the
// successor's re-encode growth, because for sorted keys a<b<c the shared-prefix
// triangle inequality sharedPrefixLen(a,c) ≥ min(sharedPrefixLen(a,b),
// sharedPrefixLen(b,c)) bounds how much longer the successor's unshared part can
// get when its delta base moves from E[p] to E[p-1] — net byteDelta < 0 on every
// path. TestTryDeleteAtCompressed_AlwaysShrinks / _ReencodingGrowthIsBounded
// enforce that bound.
//
// Four delete positions within the target group (p = position in group, gc =
// group count), found via the strict > rule (a delete always targets a present
// entry, never a boundary):
//   - D-A: gc == 1        → remove the whole group (RestartCount decreases — the
//     one case that changes the restart-table size).
//   - D-B: p == 0, gc > 1 → the old E[1] delta becomes the new restart entry
//     (full-key encoded).
//   - D-C: 0 < p < gc-1   → the successor E[p+1] (was a delta vs E[p]) is
//     re-encoded as a delta vs E[p-1].
//   - D-D: p == gc-1      → drop the group's last entry; no successor.
func tryDeleteAtCompressed(buf []byte, cfg Config, deleteIdx int) bool {
	r := NewLeafReader(buf, cfg)
	count := r.Count()
	rc := r.rt.RestartCount()
	contentEnd := cfg.ContentEnd()
	dataEnd := r.DataEnd()
	restartTableOff := contentEnd - rc*restartTableEntrySize

	// Find the group containing deleteIdx. The strict > rule selects the group
	// whose accumulated count first exceeds deleteIdx (unlike the insert >= rule
	// — a delete targets an entry that is unambiguously inside one group).
	accum, targetGroup, p, gc := 0, rc-1, 0, 0
	for g := range rc {
		c := r.rt.GroupEntryCount(g)
		if accum+c > deleteIdx {
			targetGroup, p, gc = g, deleteIdx-accum, c
			break
		}
		accum += c
	}

	groupStart := r.rt.Offset(targetGroup)
	groupEnd := dataEnd
	if targetGroup+1 < rc {
		groupEnd = r.rt.Offset(targetGroup + 1)
	}

	// Case D-A: single-entry group — remove it entirely. RestartCount drops by
	// one, so the restart table (which grows backward from ContentEnd) shifts one
	// slot toward ContentEnd: later groups keep their slot's byte position and
	// only adjust their stored data offset, while earlier groups' slots move one
	// entry right.
	if gc == 1 {
		oldGroupBytes := groupEnd - groupStart
		if groupEnd < dataEnd {
			copy(buf[groupStart:], buf[groupEnd:dataEnd]) // shift later groups' data left
		}
		newDataEnd := dataEnd - oldGroupBytes
		newRC := rc - 1
		newRestartTableOff := contentEnd - newRC*restartTableEntrySize

		// Later groups (g > targetGroup): data shifted left by oldGroupBytes;
		// their slot's byte position is unchanged (new index g-1 at the new base
		// == old index g at the old base, since the table moved one slot toward
		// ContentEnd). Adjust the stored data offset in place.
		for g := targetGroup + 1; g < rc; g++ {
			off := restartTableOff + g*restartTableEntrySize
			le.PutUint16(buf[off:], uint16(int(le.Uint16(buf[off:]))-oldGroupBytes))
		}
		// Earlier groups (g < targetGroup): unchanged data, but each slot moves
		// one entry toward ContentEnd. Copy high→low so the rightward move never
		// clobbers a not-yet-copied source slot.
		for g := targetGroup - 1; g >= 0; g-- {
			src := restartTableOff + g*restartTableEntrySize
			dst := newRestartTableOff + g*restartTableEntrySize
			copy(buf[dst:dst+restartTableEntrySize], buf[src:src+restartTableEntrySize])
		}
		// Zero [newDataEnd, newRestartTableOff): the stale data tail freed by the
		// shift, the old free space, and the one restart slot vacated at the low
		// end of the table. Must run AFTER the earlier-groups copy, whose source
		// slots live in this range. The relocated table at [newRestartTableOff,
		// ContentEnd) is disjoint.
		clear(buf[newDataEnd:newRestartTableOff])

		WriteHeader(buf, TypeLeaf, uint16(count-1), 0)
		le.PutUint16(buf[leafOffRestartCount:], uint16(newRC))
		le.PutUint16(buf[leafOffDataEnd:], uint16(newDataEnd))
		return true
	}

	// gc > 1: D-B / D-C / D-D. Walk the group to capture the predecessor key
	// (D-C's new delta base) and the displaced successor (D-B/D-C), cloning keys
	// and values because the walk's keyBuf is reused across delta decodes.
	// spliceOff/spliceEnd bracket the bytes being replaced: just E[p] for D-D,
	// E[p]..E[p+1] for D-B/D-C.
	var (
		predKey              []byte
		succFlags            uint8
		succKey, succVal     []byte
		succT0, succT1       uint64
		newShared            int
		spliceOff, spliceEnd int
		hasSucc              bool
	)
	keyBuf := make([]byte, 0, 64)
	walkOff := groupStart
	var prevKey []byte
	for i := 0; i < gc; i++ {
		entryStart := walkOff
		var ent LeafEntry
		if i == 0 {
			ent, walkOff = r.decodeRestartEntry(walkOff)
		} else {
			ent, walkOff, keyBuf = r.decodeDeltaEntry(walkOff, prevKey, keyBuf)
		}
		if i == p-1 {
			predKey = bytes.Clone(ent.Key)
		}
		if i == p {
			spliceOff = entryStart
			if p == gc-1 { // D-D: deleted entry is the group's last; no successor.
				spliceEnd = walkOff
				break
			}
		} else if i == p+1 { // D-B (p==0) or D-C: capture the successor, then stop.
			spliceEnd = walkOff
			hasSucc = true
			succFlags = ent.Flags
			succKey = bytes.Clone(ent.Key)
			if cellHasTrailerOnly(ent.Flags) {
				succT0, succT1 = entryTrailer(ent)
			} else {
				succVal = bytes.Clone(ent.Value)
			}
			if p > 0 { // D-C: successor re-encodes as a delta vs E[p-1].
				newShared = sharedPrefixLen(predKey, succKey)
			}
			break
		}
		prevKey = ent.Key
	}

	// Size of the replaced region after the splice: the re-encoded successor, or
	// 0 for D-D (entry simply removed).
	newReplaceSize := 0
	if hasSucc {
		if p == 0 { // D-B: successor promoted to a full-key restart.
			newReplaceSize = 1 + 2 + len(succKey) + valuePartSize(succFlags, succVal)
		} else { // D-C: successor as a delta vs E[p-1].
			newReplaceSize = 1 + 2 + 2 + (len(succKey) - newShared) + valuePartSize(succFlags, succVal)
		}
	}
	oldReplaceSize := spliceEnd - spliceOff
	byteDelta := newReplaceSize - oldReplaceSize // always < 0 (see doc)
	newDataEnd := dataEnd + byteDelta

	// Shift everything after the replaced region left by |byteDelta| in ONE
	// contiguous move: the trailing in-group entries E[p+2..] and all later
	// groups are adjacent (a group ends exactly where the next begins), so they
	// shift by the same delta. Their bytes are unchanged — E[p+2]'s predecessor
	// E[p+1]'s full key is unchanged by the re-encode, so its delta still holds.
	// copy is memmove (handles the overlapping left shift).
	if byteDelta != 0 {
		copy(buf[spliceEnd+byteDelta:newDataEnd], buf[spliceEnd:dataEnd])
	}

	// Write the re-encoded successor at spliceOff (nothing for D-D), via the
	// shared entry-writers (gmdb's ValueLen-before-key order). Its source is
	// Go-cloned (succKey / succVal), independent of the bytes just shifted.
	if hasSucc {
		if p == 0 {
			writeCompressedRestartEntry(buf, spliceOff, succFlags, succKey, succVal, succT0, succT1)
		} else {
			writeCompressedDeltaEntry(buf, spliceOff, succFlags, newShared, succKey[newShared:], succVal, succT0, succT1)
		}
	}

	// A delete always shrinks the page — zero the freed tail so free space stays
	// zeroed. The restart table does not move (RestartCount unchanged); only its
	// entries' offsets/count change.
	clear(buf[newDataEnd:dataEnd])

	// Patch the restart table: drop the target group's Count by one, shift every
	// later group's Offset by byteDelta. RestartCount is unchanged (gc > 1 → the
	// group survives), so the table itself stays in place.
	buf[restartTableOff+targetGroup*restartTableEntrySize+2] = uint8(gc - 1)
	for g := targetGroup + 1; g < rc; g++ {
		off := restartTableOff + g*restartTableEntrySize
		le.PutUint16(buf[off:], uint16(int(le.Uint16(buf[off:]))+byteDelta))
	}

	WriteHeader(buf, TypeLeaf, uint16(count-1), 0)
	le.PutUint16(buf[leafOffDataEnd:], uint16(newDataEnd))
	return true
}

// entryTrailer returns an entry's two trailer u64s for the trailer-only cell
// forms — (OverflowPage, TotalLen) for overflow, (NestedRoot, NestedCount) for
// nested-tree. Returns (0, 0) for inline / subpage cells (whose value is inline,
// not a trailer); callers gate on cellHasTrailerOnly before using it.
func entryTrailer(e LeafEntry) (uint64, uint64) {
	if e.IsNestedTree() {
		return e.NestedRoot, e.NestedCount
	}
	return e.OverflowPage, e.TotalLen
}

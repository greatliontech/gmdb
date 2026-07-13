package page

// Shared restart-group infrastructure for compressed leaf pages (and any
// future page kind that adopts variable-size restart groups).
//
// Per page-formats.md §Compressed Leaf, the restart table sits at the end
// of the page's content area, immediately before the optional XXH3-64
// footer, growing backward from ContentEnd. Each entry is 4 bytes:
//
//	+----------+--------+-----------+
//	| Offset   | Count  | Reserved  |
//	| uint16   | uint8  | uint8     |
//	+----------+--------+-----------+
//
// Offset is the byte offset (within the page) of the group's first entry.
// Count is the number of entries in this group (1..255). Reserved is zero
// on write; ignored on read per file-layout.md §Reserved-byte policy.

// restartTableEntrySize is the per-group restart-table entry width.
const restartTableEntrySize = 4

// restartTable provides read access to a leaf page's restart table. Built
// once at LeafReader construction; cheap to construct, holds only a slice
// header into the page buffer plus precomputed extents.
type restartTable struct {
	buf          []byte // page buffer
	restartCount int    // number of groups
	tableOff     int    // byte offset of the first restart-table entry
}

// newRestartTable returns a restartTable view over buf. restartCount is the
// group count (read by the caller from the per-page header field at offset
// HeaderSize+0). contentEnd is cfg.ContentEnd().
func newRestartTable(buf []byte, restartCount int, contentEnd int) restartTable {
	return restartTable{
		buf:          buf,
		restartCount: restartCount,
		tableOff:     contentEnd - restartCount*restartTableEntrySize,
	}
}

// RestartCount returns the number of restart groups.
func (rt restartTable) RestartCount() int { return rt.restartCount }

// TableOffset returns the byte offset within the page where the restart
// table begins (the first byte of entry 0's Offset field). Exposed so the
// builder + splice helpers know where the free-space tail ends.
func (rt restartTable) TableOffset() int { return rt.tableOff }

// Offset returns the byte offset of the i-th group's first entry. Panics
// on out-of-range i — callers iterate [0, RestartCount()).
func (rt restartTable) Offset(i int) int {
	off := rt.tableOff + i*restartTableEntrySize
	return int(le.Uint16(rt.buf[off:]))
}

// GroupEntryCount returns the entry count of the i-th group, in [1, 255].
// A Count of 0 in any group is structural corruption per page-formats.md
// §Compressed Leaf invariant; the decoder returns ErrCorrupted on
// encounter.
func (rt restartTable) GroupEntryCount(i int) int {
	off := rt.tableOff + i*restartTableEntrySize + 2
	return int(rt.buf[off])
}

// GroupStartIndex returns the absolute entry index of the first entry in
// group groupIdx by summing the Counts of preceding groups. O(groupIdx);
// hot callers cache the result or use the offset directly.
func (rt restartTable) GroupStartIndex(groupIdx int) int {
	n := 0
	for g := range groupIdx {
		n += rt.GroupEntryCount(g)
	}
	return n
}

// FindGroupContaining returns the group index whose entry range contains
// the given absolute entry index. Precondition: 0 <= entryIdx < total
// entry count. Caller-side responsibility to ensure entryIdx is in range;
// returns the last group on out-of-range as a defensive fallback.
func (rt restartTable) FindGroupContaining(entryIdx int) int {
	accum := 0
	for g := 0; g < rt.restartCount; g++ {
		gc := rt.GroupEntryCount(g)
		if accum+gc > entryIdx {
			return g
		}
		accum += gc
	}
	return rt.restartCount - 1
}

// restartGroupTracker manages restart-group state during page building.
// Maintains an in-progress list of (offset, accumulated count) for the
// groups emitted so far, plus the current group's running entry count.
// Used by LeafBuilder; would also be used by any future builder for a
// variable-restart-group page kind.
//
// The inline restartsBuf array avoids heap allocation for pages with up to
// 128 restart groups — covers the worst-case ~32-byte keys at K=16 (≈ 250
// entries → ~16 groups) by an order of magnitude.
type restartGroupTracker struct {
	// restarts packs (offset uint16, count uint8) per group into a uint32:
	// the low 16 bits are the offset, bits 16-23 are the count (set when
	// the group is finalized; 0 while in-progress). The packed form lets
	// us push a new group atomically and finalize it later without two
	// parallel slices.
	restarts      []uint32
	curGroupCount int
	restartsBuf   [128]uint32
}

// init resets the tracker to empty. Reuses the inline backing array so a
// pooled LeafBuilder doesn't reallocate.
func (t *restartGroupTracker) init() {
	t.restarts = t.restartsBuf[:0]
	t.curGroupCount = 0
}

// IsRestart reports whether the next entry should start a new restart
// group. True when no entries have been added yet (totalCount==0) or the
// current group has reached the target cap.
func (t *restartGroupTracker) IsRestart(totalCount, target int) bool {
	return totalCount == 0 || t.curGroupCount >= target
}

// StartGroup records the start of a new restart group at the given byte
// offset. Caller must call FinalizeCurrentGroup BEFORE StartGroup when
// transitioning between groups (so the previous group's count is sealed).
func (t *restartGroupTracker) StartGroup(offset int) {
	t.restarts = append(t.restarts, uint32(offset))
	t.curGroupCount = 0
}

// FinalizeCurrentGroup writes the current group's count into the packed
// restarts entry. No-op when the current group is empty (start of page).
//
// **Idempotence requirement** (load-bearing for LeafBuilder fit-fail-
// retry safety): repeated calls with the same `curGroupCount` must
// produce the same `restarts[last]` value. The OR-with-(count<<16)
// satisfies this only because (a) the count-bits were zeroed at
// StartGroup time, and (b) curGroupCount only increases (via IncrCount)
// between StartGroup and the next StartGroup. Any future change that
// stores something other than the current count via OR-merge (a max, a
// sum, etc.) must preserve repeat-call idempotence or be paired with a
// curGroupCount reset.
func (t *restartGroupTracker) FinalizeCurrentGroup() {
	if t.curGroupCount > 0 && len(t.restarts) > 0 {
		t.restarts[len(t.restarts)-1] |= uint32(t.curGroupCount) << 16
	}
}

// IncrCount records one more entry in the current group.
func (t *restartGroupTracker) IncrCount() { t.curGroupCount++ }

// CurGroupCount returns the in-progress group's entry count. Used by the
// builder's fit-ahead check when deciding whether the next entry would
// fit before opening a new group.
func (t *restartGroupTracker) CurGroupCount() int { return t.curGroupCount }

// GroupCount returns the number of groups recorded so far (including the
// in-progress one, if any).
func (t *restartGroupTracker) GroupCount() int { return len(t.restarts) }

// TableSize returns the byte size the restart table would occupy if we
// added `extra` more groups (typically 0 or 1) beyond the current count.
// Used by the builder to fit-check before appending an entry that would
// open a new group.
func (t *restartGroupTracker) TableSize(extra int) int {
	return (len(t.restarts) + extra) * restartTableEntrySize
}

// WriteTable serializes the restart table into buf at the page's content
// tail (ending at contentEnd). Finalizes the in-progress group first.
// Returns the restart-group count written; the caller stores this in the
// per-page RestartCount header field.
func (t *restartGroupTracker) WriteTable(buf []byte, contentEnd int) int {
	t.FinalizeCurrentGroup()

	rc := len(t.restarts)
	tableStart := contentEnd - rc*restartTableEntrySize
	for i, packed := range t.restarts {
		off := tableStart + i*restartTableEntrySize
		le.PutUint16(buf[off:], uint16(packed&0xFFFF))
		buf[off+2] = uint8(packed >> 16)
		buf[off+3] = 0 // reserved
	}
	return rc
}

// sharedPrefixLen returns the length of the common prefix between a and b.
// Used in delta-encoding to compute SharedLen at builder time and to
// compute the kcpl optimization at lookup time.
func sharedPrefixLen(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

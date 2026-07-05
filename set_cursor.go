package gmdb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/thegrumpylion/gmdb/internal/btree"
	"github.com/thegrumpylion/gmdb/internal/page"
)

// SetCursor is a bidirectional iterator over a SetKeyspace's
// (key, value) pairs, with intra-key value navigation per
// api-surface.md §SetCursor API and the iteration semantics in
// keyspaces.md §Iteration Semantics.
//
// Two levels of position:
//
//   - OUTER (key): tracked by an inner btree.Cursor over the parent
//     SetKeyspace tree. Outer moves are First / Last / Next / Prev /
//     Seek / SeekGE / NextKey / PrevKey.
//   - INNER (value within current key's set): subpage cells
//     materialize their (page-bounded) value slice and track an int
//     index; nested-tree cells STREAM members through a lazy inner
//     btree cursor, so a position costs O(1) memory on
//     arbitrarily large sets (set-keyspace.md §Cursor value
//     streaming). Inner moves are FirstValue / LastValue /
//     NextValue / PrevValue / SeekValue, uniform across both modes.
//
// Entailed invariant E4 (set-keyspace.md §Invariants): `NextValue`
// from the last value of a key transitions the cursor to
// "value-EOF for this key" (next NextValue returns nil); only Next
// / NextKey advance across keys. Symmetric for PrevValue / value-
// BOF / Prev / PrevKey.
//
// Value storage strategy (set-keyspace.md §Cursor Value Streaming):
// subpage cells materialize their value slice on each outer-key
// transition — bounded by one page by construction; nested-tree
// cells stream members through a lazy inner cursor, so a position
// costs O(tree depth) regardless of set size. Returned member
// slices are fresh copies (tx-lifetime ownership, api-surface.md
// §Byte Slice Ownership); empty members surface as []byte{}.
//
// Sibling-mutation contract: SetKeyspace tracks every open
// SetCursor in `openSetCursors`. SetKeyspace.Put / Delete /
// DeleteValue / SetCursor.Delete MarkStale every OTHER cursor on
// the same keyspace; the cursor's outer btree.Cursor surfaces
// ErrCursorStale on the next non-repositioning op. The caller
// re-positions via First / Last / Seek / SeekGE.
//
// Tx lifecycle: every navigation/mutation method first checks
// that the parent tx is still open and the keyspace handle is not
// dead (DeleteKeyspace-invalidated). After tx.Commit() or
// tx.Rollback() the cursor returns (nil, nil) and Err() reports
// ErrTxClosed.
type SetCursor struct {
	ks       *SetKeyspace
	tx       *Tx
	closeErr error

	// outerCursor traverses the parent SetKeyspace's B+tree. Its
	// Next/Prev/Seek/SeekGE operations are the source-of-truth for
	// outer-key positioning; we read the LeafEntry via outerCursor.
	outerCursor *btree.Cursor

	// positioned is true iff the cursor holds a valid current key +
	// value-position state (either mode). False on Unpositioned
	// (pre-First) or End-of-iteration.
	positioned bool

	// currentKey is a heap-copy of the current outer-key (the
	// SetKeyspace key). Heap copy so the outerCursor's stale-mark
	// invalidation cannot leave us with a dangling alias.
	currentKey []byte

	// values is the materialized sorted value-set for currentKey.
	// Each entry is a heap copy (independent of leaf-buffer borrow
	// lifetimes). Always sorted (set-keyspace.md §Invariants + nested-tree btree-order).
	values [][]byte

	// innerIdx is the position within values:
	//   [0, len(values)) — Positioned at values[innerIdx].
	//   len(values)      — Value-EOF (NextValue past last value).
	//   -1               — Value-BOF (PrevValue past first value).
	innerIdx int

	// Nested-mode value navigation (set-keyspace.md §Cursor Value
	// Streaming): when the current key's cell is a nested tree, the
	// member set is NOT materialized — values stays nil and the
	// fields below stream members through a lazy inner cursor, so a
	// cursor position costs O(1) memory on million-member sets.
	// Subpage cells keep the materialized slice (bounded by one
	// page). innerIdx is meaningful only in subpage mode; the
	// BOF/EOF sentinels map to innerState in nested mode.
	nestedActive bool
	nestedRoot   uint64
	nestedCount  uint64
	// inner is the lazy member cursor; nil until a value op
	// positions it, and discarded on BOF/EOF transitions (a btree
	// cursor that has walked off an end re-descends on recovery).
	inner *btree.Cursor
	// innerState: 0 = at curVal, -1 = value-BOF, +1 = value-EOF.
	innerState int8
	// curVal is a heap copy of the current member (innerState == 0).
	curVal []byte

	// stale is set by markSetCursorsStale (called from
	// SetKeyspace.Put / Delete / DeleteValue) and cleared by
	// re-positioning ops (First / Last / Seek / SeekGE). It is
	// independent of outerCursor's own gen/posGen stale tracking
	// because SetCursor's value-bounded ops (NextValue / PrevValue
	// / Current) short-circuit on the materialized `values` slice
	// without touching outerCursor — so outerCursor's stale flag
	// alone is insufficient to gate them.
	stale bool
}

// Cursor returns a new SetCursor for iterating over this
// SetKeyspace's (key, value) pairs. Starts Unpositioned — call
// First / Last / Seek / SeekGE before reading.
//
// Calling Cursor() on a handle invalidated by a same-tx
// DeleteKeyspace is permitted; the returned cursor's methods all
// surface ErrKeyspaceClosed (every cursor op probes ks.dead).
//
// Sibling-mutation contract: SetKeyspace.Put / Delete /
// DeleteValue / sibling SetCursor.Delete MarkStale every cursor
// returned by this method that is still reachable; subsequent
// non-repositioning ops surface ErrCursorStale until re-Seeked.
// newInternalSetCursor returns a *SetCursor on this SetKeyspace
// WITHOUT registering in ks.openSetCursors. Used by
// internal helpers (RebuildIndex's per-(setKey, setValue) walk
// on the cached path) where the cursor's lifetime is scoped to a
// single helper call. Registration would leak entries into the
// per-tx openSetCursors slice across repeated calls. The non-
// registered cursor doesn't receive markStale from sibling
// mutations — fine when no concurrent ops fire during the
// internal loop.
func newInternalSetCursor(ks *SetKeyspace) *SetCursor {
	return &SetCursor{
		ks:          ks,
		tx:          ks.tx,
		outerCursor: ks.newRootCursor(),
	}
}

func (ks *SetKeyspace) Cursor() *SetCursor {
	c := &SetCursor{
		ks:          ks,
		tx:          ks.tx,
		outerCursor: ks.newRootCursor(),
	}
	// Only register cursors on live handles. A dead keyspace's
	// cursors are rejected by requireOpen anyway (closeErr →
	// ErrKeyspaceClosed); appending them would let a pathological
	// caller (`for { sks.Cursor() }` after DeleteKeyspace) grow
	// openSetCursors unboundedly, since markSetCursorsStale walks
	// every entry on every sibling mutation.
	if !ks.dead {
		ks.openSetCursors = append(ks.openSetCursors, c)
	}
	return c
}

// unregisterSetCursor removes c from ks.openSetCursors — the
// SetCursor analogue of Keyspace.unregisterCursor; see its doc for
// the pairing contract.
func (ks *SetKeyspace) unregisterSetCursor(c *SetCursor) {
	for i, x := range ks.openSetCursors {
		if x == c {
			last := len(ks.openSetCursors) - 1
			ks.openSetCursors[i] = ks.openSetCursors[last]
			ks.openSetCursors[last] = nil
			ks.openSetCursors = ks.openSetCursors[:last]
			return
		}
	}
}

// requireOpen short-circuits closed-state and dead-keyspace checks.
// Used by RE-POSITIONING ops (First / Last / Seek / SeekGE) which
// recover from a stale state and so should be permitted to run
// even when c.stale is true. Sets closeErr (permanent) on
// tx-closed / keyspace-closed; never sets it for stale (transient).
func (c *SetCursor) requireOpen(needsWrite bool) bool {
	if c.closeErr != nil {
		return false
	}
	if err := c.tx.requireOpen(needsWrite); err != nil {
		// ErrChildActive is transient — the parent-freeze lifts when the
		// active child resolves (transactions.md §Nested Transactions).
		// Do NOT stick it; terminal errors (ErrTxClosed / ErrReadOnly /
		// ErrClosed) still stick.
		if !errors.Is(err, ErrChildActive) {
			c.closeErr = err
		}
		return false
	}
	if c.ks.dead {
		c.closeErr = ErrKeyspaceClosed
		return false
	}
	if needsWrite && c.ks.readOnly {
		c.closeErr = ErrReadOnly
		return false
	}
	return true
}

// requireFresh = requireOpen + stale check. Used by
// NON-REPOSITIONING ops (Next / Prev / NextKey / PrevKey /
// NextValue / PrevValue / FirstValue / LastValue / SeekValue /
// Current / CountValues / Delete). On stale, returns false without
// touching closeErr — Err() surfaces ErrCursorStale via the
// separate c.stale check, and a subsequent re-positioning op
// (First/Last/Seek/SeekGE) clears c.stale via clearPosition.
//
// This split is critical: SetCursor's value-bounded ops
// short-circuit on the materialized values slice without touching
// outerCursor, so outerCursor's own gen/posGen stale flag alone
// is insufficient to gate them. c.stale (set by markSetCursorsStale
// from every keyspace mutation) is the canonical stale source.
func (c *SetCursor) requireFresh(needsWrite bool) bool {
	if !c.requireOpen(needsWrite) {
		return false
	}
	if c.stale {
		return false
	}
	return true
}

// First positions the cursor at the (firstKey, firstValue) pair of
// the SetKeyspace. Returns (nil, nil) on an empty keyspace.
func (c *SetCursor) First() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	c.clearPosition()
	k, _ := c.outerCursor.First()
	if k == nil {
		return nil, nil
	}
	if err := c.materializeAtOuter(); err != nil {
		c.closeErr = err
		return nil, nil
	}
	v := c.valSetFirst()
	if v == nil {
		return nil, nil // nested descend failed — Err() carries the cause
	}
	return c.currentKey, v
}

// Last positions at the (lastKey, lastValueOfLastKey) pair.
func (c *SetCursor) Last() (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	c.clearPosition()
	k, _ := c.outerCursor.Last()
	if k == nil {
		return nil, nil
	}
	if err := c.materializeAtOuter(); err != nil {
		c.closeErr = err
		return nil, nil
	}
	v := c.valSetLast()
	if v == nil {
		return nil, nil
	}
	return c.currentKey, v
}

// Next advances by one (key, value) pair. Walks values within the
// current key first; on value-exhaustion advances to the next
// key's first value. Returns (nil, nil) at end-of-iteration.
//
// From Unpositioned, Next is equivalent to First.
func (c *SetCursor) Next() (key, value []byte) {
	if !c.requireFresh(false) {
		return nil, nil
	}
	if !c.positioned {
		return c.First()
	}
	// In-key advance (E4 machinery, but Next DOES cross keys on
	// value exhaustion). Dispatch on ok, never on slice nil-ness —
	// an empty member is a real position.
	if v, ok := c.valNext(); ok {
		return c.currentKey, v
	}
	if c.closeErr != nil {
		return nil, nil // read error mid-key — do not cross keys past it
	}
	// Cross to next key.
	return c.advanceOuterForward()
}

// Prev steps backward by one (key, value) pair. From Unpositioned,
// Prev is equivalent to Last.
func (c *SetCursor) Prev() (key, value []byte) {
	if !c.requireFresh(false) {
		return nil, nil
	}
	if !c.positioned {
		return c.Last()
	}
	if v, ok := c.valPrev(); ok {
		return c.currentKey, v
	}
	if c.closeErr != nil {
		return nil, nil
	}
	return c.advanceOuterBackward()
}

// Seek positions at the (key, firstValueOf key) pair where key
// matches target exactly. Returns (nil, nil) on miss with
// End-of-iteration state.
func (c *SetCursor) Seek(target []byte) (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	c.clearPosition()
	k, _ := c.outerCursor.Seek(target)
	if k == nil {
		return nil, nil
	}
	if err := c.materializeAtOuter(); err != nil {
		c.closeErr = err
		return nil, nil
	}
	v := c.valSetFirst()
	if v == nil {
		return nil, nil // nested descend failed — Err() carries the cause
	}
	return c.currentKey, v
}

// SeekGE positions at the (smallest-key-≥-target, firstValueOf
// thatKey) pair. Returns (nil, nil) when no key ≥ target exists.
func (c *SetCursor) SeekGE(target []byte) (key, value []byte) {
	if !c.requireOpen(false) {
		return nil, nil
	}
	c.clearPosition()
	k, _ := c.outerCursor.SeekGE(target)
	if k == nil {
		return nil, nil
	}
	if err := c.materializeAtOuter(); err != nil {
		c.closeErr = err
		return nil, nil
	}
	v := c.valSetFirst()
	if v == nil {
		return nil, nil // nested descend failed — Err() carries the cause
	}
	return c.currentKey, v
}

// Current returns the current (key, value) without advancing.
// (nil, nil) at Unpositioned or End-of-iteration; also (nil, nil)
// at value-EOF or value-BOF (per E4, the cursor is conceptually
// off-the-key in those states).
func (c *SetCursor) Current() (key, value []byte) {
	if !c.requireFresh(false) {
		return nil, nil
	}
	if !c.positioned {
		return nil, nil
	}
	if v, ok := c.valCurrent(); ok {
		return c.currentKey, v
	}
	return nil, nil
}

// FirstValue rewinds to the first value of the current key. Returns
// (nil) at Unpositioned (the cursor has no current key to bound to).
func (c *SetCursor) FirstValue() (value []byte) {
	if !c.requireFresh(false) {
		return nil
	}
	if !c.positioned {
		return nil
	}
	return c.valSetFirst()
}

// LastValue forwards to the last value of the current key.
func (c *SetCursor) LastValue() (value []byte) {
	if !c.requireFresh(false) {
		return nil
	}
	if !c.positioned {
		return nil
	}
	return c.valSetLast()
}

// NextValue advances by one VALUE within the current key's set.
// Per entailed invariant E4: does NOT cross key boundaries. From
// the last value, returns nil and transitions to "value-EOF for
// this key" — only Next / NextKey advance across keys.
func (c *SetCursor) NextValue() (value []byte) {
	if !c.requireFresh(false) {
		return nil
	}
	if !c.positioned {
		return nil
	}
	v, _ := c.valNext()
	return v
}

// PrevValue steps back by one value within the current key's set.
// Symmetric with NextValue per E4: does NOT cross key boundaries.
func (c *SetCursor) PrevValue() (value []byte) {
	if !c.requireFresh(false) {
		return nil
	}
	if !c.positioned {
		return nil
	}
	v, _ := c.valPrev()
	return v
}

// NextKey skips the remainder of the current key's value set and
// positions at the first value of the next key. Returns
// (nil, nil) at end-of-iteration.
func (c *SetCursor) NextKey() (key, value []byte) {
	if !c.requireFresh(false) {
		return nil, nil
	}
	if !c.positioned {
		return c.First()
	}
	return c.advanceOuterForward()
}

// PrevKey skips to the first value of the previous key. (Note: NOT
// the last value of the previous key — symmetric with NextKey,
// both land at the FIRST value of the target key for consistency
// with the api-surface "position at the start of a key's set"
// convention.)
//
// From Unpositioned, PrevKey lands at the FIRST value of the last
// key (NOT the last value, which would be the Last()-equivalent —
// PrevKey is "skip to a key" and the api-surface contract says
// PrevKey always positions at the first value of its target).
//
// Returns (nil, nil) at begin-of-iteration.
func (c *SetCursor) PrevKey() (key, value []byte) {
	if !c.requireFresh(false) {
		return nil, nil
	}
	if !c.positioned {
		// Position at the last key's FIRST value (NOT last value).
		// Last() lands at the last key's last value; reset innerIdx
		// to 0 for the "PrevKey lands at first value" semantic.
		k, _ := c.Last()
		if k == nil {
			return nil, nil
		}
		v := c.valSetFirst()
		if v == nil {
			return nil, nil // nested descend failed — Err() carries the cause
		}
		return c.currentKey, v
	}
	// Need to go to PREVIOUS key. outerCursor.Prev() steps back
	// one outer-key, then materialize and position innerIdx=0.
	return c.prevOuterFirstValue()
}

// SeekValue locates target within the current key's value set.
// On hit: sets innerIdx to the matching index and returns the
// value. On miss: returns nil and leaves innerIdx UNCHANGED — per
// E4, SeekValue is bounded to the current key and never crosses
// to a neighboring key, nor does it speculatively reposition.
//
// (target is returned verbatim on hit so the caller can chain
// idiomatically with NextValue / PrevValue. The returned slice
// aliases the cursor's internal values slice — caller MUST copy
// before mutating.)
func (c *SetCursor) SeekValue(target []byte) (value []byte) {
	if !c.requireFresh(false) {
		return nil
	}
	if !c.positioned {
		return nil
	}
	v, _ := c.valSeek(target)
	return v
}

// CountValues returns the number of values in the current key's
// set. Returns (0, nil) at Unpositioned. Materialized in O(1).
func (c *SetCursor) CountValues() (uint64, error) {
	if !c.requireFresh(false) {
		if c.closeErr != nil {
			return 0, c.closeErr
		}
		return 0, ErrCursorStale
	}
	if !c.positioned {
		return 0, nil
	}
	return c.valCount(), nil
}

// Delete removes the current (key, value) pair. Cursor must be
// Positioned at a real value (not value-EOF / value-BOF);
// otherwise returns ErrCursorUnpositioned. After delete, advances
// to the next (key, value) pair OR transitions to end-of-iteration
// — matching the Cursor.Delete post-state contract.
//
// Last-value-of-key delete drops the parent cell per set-keyspace.md §Invariants (empty
// sets must not persist); the cursor's subsequent Next would land
// on the next key's first value.
//
// Errors: ErrCursorUnpositioned, ErrReadOnly (read-only tx),
// ErrTxClosed, ErrKeyspaceClosed, ErrCorrupted (wrapped on
// structural fault during the re-seek).
func (c *SetCursor) Delete() error {
	if !c.requireFresh(true) {
		if c.closeErr != nil {
			return c.closeErr
		}
		return ErrCursorStale
	}
	if !c.positioned || !c.valAtReal() {
		return ErrCursorUnpositioned
	}
	k := bytes.Clone(c.currentKey)
	cur, _ := c.valCurrent()
	v := bytes.Clone(cur)

	// Determine the successor pre-delete:
	//   - in-key: next value within current set.
	//   - cross-key: first value of next outer key (we'll re-seek
	//     using "any value ≥ (k+sentinel)" via outerCursor's next).
	// valPeekSuccessor may move the nested inner cursor; the whole
	// position is discarded by the re-seek below, so that is never
	// observed.
	var sucKey, sucValue []byte
	if sv := c.valPeekSuccessor(); sv != nil {
		sucKey = k
		sucValue = sv
	}

	// Mutate the keyspace. SetKeyspace.DeleteValue runs its full
	// dispatch (subpage in-place / nested-tree delete / demotion /
	// cell-removal) AND invokes markSetCursorsStale which marks
	// every open SetCursor — including this one — stale +
	// refreshes their outer cursor's tracked rootID. The re-seek
	// below clears OUR stale state via the standard re-position
	// path; siblings recover on their next First/Last/Seek/SeekGE.
	// On error, propagate without changing cursor state (caller
	// can retry).
	if err := c.ks.DeleteValue(k, v); err != nil {
		return err
	}

	// Re-seek to the successor.
	c.clearPosition()
	if sucKey == nil {
		// Cross-key: walk outer cursor past k. SeekGE on
		// (k + 0x00) — the smallest possible key greater than k —
		// lands at the next key (or end-of-iteration).
		next := append(append([]byte(nil), k...), 0x00)
		ck, _ := c.outerCursor.SeekGE(next)
		if ck == nil {
			return nil
		}
		if err := c.materializeAtOuter(); err != nil {
			c.closeErr = err
			return err
		}
		if c.valSetFirst() == nil && c.closeErr != nil {
			return c.closeErr
		}
		return nil
	}
	// In-key: re-Seek to sucKey, then SeekValue to sucValue.
	ck, _ := c.outerCursor.Seek(sucKey)
	if ck == nil {
		// Defensive: the key vanished between delete and re-seek
		// (shouldn't happen — we deleted a value, not the key).
		// Fall back to SeekGE past k.
		next := append(append([]byte(nil), k...), 0x00)
		ck2, _ := c.outerCursor.SeekGE(next)
		if ck2 == nil {
			return nil
		}
		if err := c.materializeAtOuter(); err != nil {
			c.closeErr = err
			return err
		}
		if c.valSetFirst() == nil && c.closeErr != nil {
			return c.closeErr
		}
		return nil
	}
	if err := c.materializeAtOuter(); err != nil {
		c.closeErr = err
		return err
	}
	if _, ok := c.valSeek(sucValue); !ok {
		if c.closeErr != nil {
			return c.closeErr // read error during the re-seek, not a miss
		}
		// Defensive: the successor value vanished (post-demote
		// merge of values? shouldn't happen). Fall back to the
		// first value.
		if c.valSetFirst() == nil && c.closeErr != nil {
			return c.closeErr
		}
	}
	return nil
}

// Err returns the cursor's current error state. Closed-state
// errors (ErrTxClosed / ErrKeyspaceClosed / ErrClosed) are sticky;
// ErrCursorStale is transient and clears when the caller
// re-positions via First / Last / Seek / SeekGE.
func (c *SetCursor) Err() error {
	if c.closeErr != nil {
		return c.closeErr
	}
	if c.ks.dead {
		return ErrKeyspaceClosed
	}
	// Transient: report the parent-freeze without sticking it, so Err
	// clears once the active child resolves.
	if c.ks.tx.activeChild != nil {
		return ErrChildActive
	}
	if c.stale {
		return ErrCursorStale
	}
	if err := c.outerCursor.Err(); err != nil {
		if errors.Is(err, btree.ErrCursorStale) {
			return ErrCursorStale
		}
		// Translate the internal Unpositioned sentinel to the public one
		// (same reason ErrCursorStale is translated just above). The source
		// of truth is outerCursor.Err(), NOT the SetCursor.positioned bool:
		// positioned is false for BOTH Unpositioned and End-of-iteration, so
		// it cannot make the distinction transactions.md §Cursor State
		// Machine requires — but outerCursor's 3-state machine returns
		// btree.ErrCursorUnpositioned only when Unpositioned and nil at
		// End-of-iteration, so EOI correctly stays nil here.
		if errors.Is(err, btree.ErrCursorUnpositioned) {
			return ErrCursorUnpositioned
		}
		// mapBtreeErr covers btree.ErrCorrupted AND the pager sentinels
		// (ErrBadPageChecksum / ErrCorrupted) now reachable through a
		// cursor read via the verifying Page (checksums.md §Verification + checksums.md §Structural and Allocation Bounds); other errors
		// pass through unwrapped, preserving the prior behaviour.
		return mapBtreeErr(err)
	}
	return nil
}

// --- internal helpers ---

// --- value navigation (mode-dispatching) ---
//
// The helpers below are the ONLY code that touches values/innerIdx
// (subpage mode) or inner/innerState/curVal (nested mode). Public
// ops express the E4 value state machine through them.

// nonNilValue normalizes an empty member to []byte{} at the helper
// boundary: api-surface.md §Nil and Empty Semantics — cursors return
// (key, []byte{}) for empty values, and the public ops dispatch on
// the ok flag, never on slice nil-ness (an empty member is a real
// position — a nil-sentinel dispatch skipped it, demonstrated in
// review).
func nonNilValue(v []byte) []byte {
	if v == nil {
		return []byte{}
	}
	return v
}

// valSetFirst positions at the current key's first value.
func (c *SetCursor) valSetFirst() []byte {
	if !c.nestedActive {
		c.innerIdx = 0
		return nonNilValue(c.values[0])
	}
	return c.nestedDescend(true)
}

// valSetLast positions at the current key's last value.
func (c *SetCursor) valSetLast() []byte {
	if !c.nestedActive {
		c.innerIdx = len(c.values) - 1
		return nonNilValue(c.values[c.innerIdx])
	}
	return c.nestedDescend(false)
}

// nestedDescend opens a fresh inner cursor at the first/last member.
// A nil walk result (empty nested tree — unreachable in-spec, empty
// sets never persist — or a read error, reported via Err) degrades
// to value-EOF.
func (c *SetCursor) nestedDescend(first bool) []byte {
	c.inner = btree.NewReadCursor(c.tx.pgr, c.ks.builderCfg(), c.nestedRoot)
	var nk []byte
	if first {
		nk, _ = c.inner.First()
	} else {
		nk, _ = c.inner.Last()
	}
	if nk == nil {
		if err := c.inner.Err(); err != nil {
			c.closeErr = mapBtreeErr(err)
		}
		c.inner = nil
		c.innerState = 1
		return nil
	}
	// Fresh copy per step: returned member slices keep the
	// tx-lifetime ownership contract (api-surface.md §Byte Slice
	// Ownership) — no reuse buffer that a later cursor op would
	// overwrite under the caller.
	c.curVal = nonNilValue(bytes.Clone(nk))
	c.innerState = 0
	return c.curVal
}

// valNext advances one value within the key; E4: never crosses keys.
// ok=false ONLY on the value-EOF transition/state (an empty member
// is ok=true with an empty slice).
func (c *SetCursor) valNext() ([]byte, bool) {
	if !c.nestedActive {
		if c.innerIdx < 0 {
			c.innerIdx = 0
			return nonNilValue(c.values[0]), true
		}
		if c.innerIdx+1 >= len(c.values) {
			c.innerIdx = len(c.values)
			return nil, false
		}
		c.innerIdx++
		return nonNilValue(c.values[c.innerIdx]), true
	}
	switch c.innerState {
	case -1:
		v := c.nestedDescend(true)
		return v, c.innerState == 0
	case 1:
		return nil, false
	}
	nk, _ := c.inner.Next()
	if nk == nil {
		if err := c.inner.Err(); err != nil {
			c.closeErr = mapBtreeErr(err)
		}
		c.inner = nil
		c.innerState = 1
		return nil, false
	}
	c.curVal = nonNilValue(bytes.Clone(nk))
	return c.curVal, true
}

// valPrev steps back one value within the key; E4 symmetric.
func (c *SetCursor) valPrev() ([]byte, bool) {
	if !c.nestedActive {
		if c.innerIdx >= len(c.values) {
			c.innerIdx = len(c.values) - 1
			return nonNilValue(c.values[c.innerIdx]), true
		}
		if c.innerIdx <= 0 {
			c.innerIdx = -1
			return nil, false
		}
		c.innerIdx--
		return nonNilValue(c.values[c.innerIdx]), true
	}
	switch c.innerState {
	case 1:
		v := c.nestedDescend(false)
		return v, c.innerState == 0
	case -1:
		return nil, false
	}
	nk, _ := c.inner.Prev()
	if nk == nil {
		if err := c.inner.Err(); err != nil {
			c.closeErr = mapBtreeErr(err)
		}
		c.inner = nil
		c.innerState = -1
		return nil, false
	}
	c.curVal = nonNilValue(bytes.Clone(nk))
	return c.curVal, true
}

// valSeek locates target within the current key's set. On miss it
// returns ok=false and leaves the position UNCHANGED (E4) — nested
// mode probes with a point Get before moving anything.
func (c *SetCursor) valSeek(target []byte) ([]byte, bool) {
	if !c.nestedActive {
		idx, found := binarySearchValues(c.values, target)
		if !found {
			return nil, false
		}
		c.innerIdx = idx
		return nonNilValue(c.values[idx]), true
	}
	_, found, err := btree.Get(c.tx.pgr, c.ks.builderCfg(), c.nestedRoot, target)
	if err != nil {
		c.closeErr = mapBtreeErr(err)
		return nil, false
	}
	if !found {
		return nil, false
	}
	c.inner = btree.NewReadCursor(c.tx.pgr, c.ks.builderCfg(), c.nestedRoot)
	nk, _ := c.inner.Seek(target)
	if nk == nil {
		// The member vanished between probe and descent — same-tx
		// impossible (no mutation between), defensive only.
		c.inner = nil
		c.innerState = 1
		return nil, false
	}
	c.curVal = nonNilValue(bytes.Clone(nk))
	c.innerState = 0
	return c.curVal, true
}

// valCurrent returns the current value; ok=false at BOF/EOF.
func (c *SetCursor) valCurrent() ([]byte, bool) {
	if !c.nestedActive {
		if c.innerIdx < 0 || c.innerIdx >= len(c.values) {
			return nil, false
		}
		return nonNilValue(c.values[c.innerIdx]), true
	}
	if c.innerState != 0 {
		return nil, false
	}
	return c.curVal, true
}

// valAtReal reports whether the cursor is at a real value (not
// BOF/EOF) — Delete's precondition.
func (c *SetCursor) valAtReal() bool {
	if !c.nestedActive {
		return c.innerIdx >= 0 && c.innerIdx < len(c.values)
	}
	return c.innerState == 0
}

// valCount is the current key's member count — O(1) in both modes
// (subpage slice length; the nested cell's persisted NestedCount).
func (c *SetCursor) valCount() uint64 {
	if !c.nestedActive {
		return uint64(len(c.values))
	}
	return c.nestedCount
}

// valPeekSuccessor returns a copy of the value after the current
// one, or nil at the key's end — Delete's pre-delete successor
// probe. Nested mode probes with a THROWAWAY cursor: the live inner
// cursor must not move, because a failed DeleteValue leaves this
// cursor un-staled and the documented retry contract promises its
// position unchanged (a moved inner cursor would skip a member —
// demonstrated in review).
func (c *SetCursor) valPeekSuccessor() []byte {
	if !c.nestedActive {
		if c.innerIdx+1 < len(c.values) {
			return bytes.Clone(c.values[c.innerIdx+1])
		}
		return nil
	}
	tmp := btree.NewReadCursor(c.tx.pgr, c.ks.builderCfg(), c.nestedRoot)
	if nk, _ := tmp.Seek(c.curVal); nk == nil {
		return nil // current member unreadable — Delete's own path will surface it
	}
	nk, _ := tmp.Next()
	if nk == nil {
		return nil
	}
	return bytes.Clone(nk)
}

// clearPosition resets the position state to Unpositioned and
// clears the stale flag — re-positioning ops use this to recover
// from a sibling-mutation-induced stale state. (The underlying
// outerCursor's own stale flag is cleared by btree.Cursor's
// re-positioning machinery: First/Last/Seek/SeekGE descend
// freshly and re-set posGen=gen.)
func (c *SetCursor) clearPosition() {
	c.positioned = false
	c.currentKey = nil
	c.values = nil
	c.innerIdx = 0
	c.nestedActive = false
	c.nestedRoot = 0
	c.nestedCount = 0
	c.inner = nil
	c.innerState = 0
	c.curVal = nil
	c.stale = false
}

// materializeAtOuter reads the current outerCursor's (key, cell)
// and establishes the value-navigation mode: subpage cells
// materialize their (page-bounded) value slice; nested-tree cells
// record root + persisted count and stream members lazily through
// the val* helpers. Caller positions via valSetFirst/valSetLast.
func (c *SetCursor) materializeAtOuter() error {
	// outerCursor's Current returns (key, opaqueValue) but we need
	// the full cell flags. Re-fetch via the keyspace's GetEntry
	// path which gives us the LeafEntry.
	k, _ := c.outerCursor.Current()
	if k == nil {
		return fmt.Errorf("%w: SetCursor.materializeAtOuter: outerCursor unpositioned", ErrCorrupted)
	}
	cfg := c.ks.builderCfg()
	e, found, err := btree.GetEntry(c.tx.pgr, cfg, c.ks.desc.Root, k)
	if err != nil {
		return mapBtreeErr(err)
	}
	if !found {
		return fmt.Errorf("%w: SetCursor: outerCursor key %q not found via GetEntry", ErrCorrupted, k)
	}
	c.currentKey = append([]byte(nil), k...)
	// Reset BOTH mode states — the previous position may have been
	// the other cell kind.
	c.values = c.values[:0]
	c.nestedActive = false
	c.nestedRoot = 0
	c.nestedCount = 0
	c.inner = nil
	c.innerState = 0
	c.curVal = nil
	switch {
	case e.IsSubpage():
		sp := page.NewSubpageReader(e.Value, c.ks.desc.FixedValueSize)
		sp.AllValues(func(v []byte) bool {
			c.values = append(c.values, append([]byte(nil), v...))
			return true
		})
	case e.IsNestedTree():
		// Nested-tree cells stream: record the root + persisted
		// count; the value helpers walk lazily (O(1) memory per
		// position on arbitrarily large sets).
		c.nestedActive = true
		c.nestedRoot = e.NestedRoot
		c.nestedCount = e.NestedCount
		c.inner = nil
		c.innerState = 0
		c.curVal = nil
	default:
		return fmt.Errorf("%w: SetCursor: unexpected cell flags 0x%x at key %q",
			ErrCorrupted, e.Flags, k)
	}
	empty := len(c.values) == 0
	if c.nestedActive {
		empty = c.nestedRoot == 0 || c.nestedCount == 0
	}
	if empty {
		return fmt.Errorf("%w: SetCursor: zero-value cell at key %q (empty-set invariant violation)",
			ErrCorrupted, k)
	}
	c.positioned = true
	return nil
}

// advanceOuterForward moves outerCursor.Next() and materializes
// the new key's values. innerIdx=0 (first value of the new key).
// Returns (nil, nil) at end-of-iteration.
func (c *SetCursor) advanceOuterForward() (key, value []byte) {
	k, _ := c.outerCursor.Next()
	if k == nil {
		c.clearPosition()
		return nil, nil
	}
	if err := c.materializeAtOuter(); err != nil {
		c.closeErr = err
		return nil, nil
	}
	v := c.valSetFirst()
	if v == nil {
		return nil, nil // nested descend failed — Err() carries the cause
	}
	return c.currentKey, v
}

// advanceOuterBackward moves outerCursor.Prev() and materializes
// the new key's values. innerIdx=len-1 (last value of the new key).
// Returns (nil, nil) at begin-of-iteration.
func (c *SetCursor) advanceOuterBackward() (key, value []byte) {
	k, _ := c.outerCursor.Prev()
	if k == nil {
		c.clearPosition()
		return nil, nil
	}
	if err := c.materializeAtOuter(); err != nil {
		c.closeErr = err
		return nil, nil
	}
	v := c.valSetLast()
	if v == nil {
		return nil, nil
	}
	return c.currentKey, v
}

// prevOuterFirstValue moves outerCursor.Prev() and positions at
// innerIdx=0 (first value of the prev key) — the PrevKey semantic.
func (c *SetCursor) prevOuterFirstValue() (key, value []byte) {
	k, _ := c.outerCursor.Prev()
	if k == nil {
		c.clearPosition()
		return nil, nil
	}
	if err := c.materializeAtOuter(); err != nil {
		c.closeErr = err
		return nil, nil
	}
	v := c.valSetFirst()
	if v == nil {
		return nil, nil // nested descend failed — Err() carries the cause
	}
	return c.currentKey, v
}

// binarySearchValues finds target in a sorted [][]byte slice.
// Returns (insertion-index, found) — same semantics as
// sort.Search but adapted for byte slices.
func binarySearchValues(values [][]byte, target []byte) (int, bool) {
	lo, hi := 0, len(values)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		cmp := bytes.Compare(values[mid], target)
		switch {
		case cmp == 0:
			return mid, true
		case cmp < 0:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return lo, false
}

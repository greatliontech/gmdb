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
//   - INNER (value within current key's set): tracked by an int
//     index into a `values [][]byte` slice materialized when the
//     outer cursor advances to a new key. Inner moves are
//     FirstValue / LastValue / NextValue / PrevValue / SeekValue.
//
// Entailed invariant E4 (set-keyspace.md §Invariants): `NextValue`
// from the last value of a key transitions the cursor to
// "value-EOF for this key" (next NextValue returns nil); only Next
// / NextKey advance across keys. Symmetric for PrevValue / value-
// BOF / Prev / PrevKey.
//
// Materialization strategy (v1 simplification): on every outer-key
// transition the cursor decodes the cell and materialises ALL
// values for that key into a [][]byte. Cost is O(N) per key
// transition; for very large nested-tree cells this allocates a
// matching-size slice. Acceptable for v1 — see
// `docs/issues/setcursor-lazy-value-iteration.md` (if/when filed
// for the perf-driven follow-up).
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

	// positioned is true iff the cursor holds a valid (currentKey,
	// values, innerIdx) triple representing a (key, value) pair.
	// False on Unpositioned (pre-First) or End-of-iteration.
	positioned bool

	// currentKey is a heap-copy of the current outer-key (the
	// SetKeyspace key). Heap copy so the outerCursor's stale-mark
	// invalidation cannot leave us with a dangling alias.
	currentKey []byte

	// values is the materialized sorted value-set for currentKey.
	// Each entry is a heap copy (independent of leaf-buffer borrow
	// lifetimes). Always sorted (Inv-2 + nested-tree btree-order).
	values [][]byte

	// innerIdx is the position within values:
	//   [0, len(values)) — Positioned at values[innerIdx].
	//   len(values)      — Value-EOF (NextValue past last value).
	//   -1               — Value-BOF (PrevValue past first value).
	innerIdx int

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
func (ks *SetKeyspace) Cursor() *SetCursor {
	cfg := ks.builderCfg()
	var outer *btree.Cursor
	if ks.tx.writable {
		outer = btree.NewCursor(ks.tx.pgr, cfg, ks.desc.Root, ks.tx.db.opts.MergeThreshold)
	} else {
		outer = btree.NewReadCursor(ks.tx.pgr, cfg, ks.desc.Root)
	}
	c := &SetCursor{
		ks:          ks,
		tx:          ks.tx,
		outerCursor: outer,
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
		c.closeErr = err
		return false
	}
	if c.ks.dead {
		c.closeErr = ErrKeyspaceClosed
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
	c.innerIdx = 0
	return c.currentKey, c.values[0]
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
	c.innerIdx = len(c.values) - 1
	return c.currentKey, c.values[c.innerIdx]
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
	// In-key advance.
	if c.innerIdx+1 < len(c.values) {
		c.innerIdx++
		return c.currentKey, c.values[c.innerIdx]
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
	if c.innerIdx > 0 {
		c.innerIdx--
		return c.currentKey, c.values[c.innerIdx]
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
	c.innerIdx = 0
	return c.currentKey, c.values[0]
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
	c.innerIdx = 0
	return c.currentKey, c.values[0]
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
	if c.innerIdx < 0 || c.innerIdx >= len(c.values) {
		return nil, nil
	}
	return c.currentKey, c.values[c.innerIdx]
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
	c.innerIdx = 0
	return c.values[0]
}

// LastValue forwards to the last value of the current key.
func (c *SetCursor) LastValue() (value []byte) {
	if !c.requireFresh(false) {
		return nil
	}
	if !c.positioned {
		return nil
	}
	c.innerIdx = len(c.values) - 1
	return c.values[c.innerIdx]
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
	if c.innerIdx < 0 {
		// Value-BOF → first value.
		c.innerIdx = 0
		return c.values[0]
	}
	if c.innerIdx+1 >= len(c.values) {
		// Already at last value (or past it). Transition to
		// value-EOF and return nil. Does NOT advance outer.
		c.innerIdx = len(c.values)
		return nil
	}
	c.innerIdx++
	return c.values[c.innerIdx]
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
	if c.innerIdx >= len(c.values) {
		// Value-EOF → last value.
		c.innerIdx = len(c.values) - 1
		return c.values[c.innerIdx]
	}
	if c.innerIdx <= 0 {
		// At first value (or before). Transition to value-BOF.
		c.innerIdx = -1
		return nil
	}
	c.innerIdx--
	return c.values[c.innerIdx]
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
		c.innerIdx = 0
		return c.currentKey, c.values[0]
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
	idx, found := binarySearchValues(c.values, target)
	if !found {
		return nil
	}
	c.innerIdx = idx
	return c.values[idx]
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
	return uint64(len(c.values)), nil
}

// Delete removes the current (key, value) pair. Cursor must be
// Positioned at a real value (not value-EOF / value-BOF);
// otherwise returns ErrCursorUnpositioned. After delete, advances
// to the next (key, value) pair OR transitions to end-of-iteration
// — matching the chunk-4 Cursor.Delete post-state contract.
//
// Last-value-of-key delete drops the parent cell per Inv-1 (empty
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
	if !c.positioned || c.innerIdx < 0 || c.innerIdx >= len(c.values) {
		return ErrCursorUnpositioned
	}
	k := bytes.Clone(c.currentKey)
	v := bytes.Clone(c.values[c.innerIdx])

	// Determine the successor pre-delete:
	//   - in-key: next value within current set.
	//   - cross-key: first value of next outer key (we'll re-seek
	//     using "any value ≥ (k+sentinel)" via outerCursor's next).
	var sucKey, sucValue []byte
	if c.innerIdx+1 < len(c.values) {
		sucKey = k
		sucValue = bytes.Clone(c.values[c.innerIdx+1])
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
		c.innerIdx = 0
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
		c.innerIdx = 0
		return nil
	}
	if err := c.materializeAtOuter(); err != nil {
		c.closeErr = err
		return err
	}
	idx, found := binarySearchValues(c.values, sucValue)
	if !found {
		// Defensive: the successor value vanished (post-demote
		// merge of values? shouldn't happen). Fall back to
		// innerIdx=0.
		c.innerIdx = 0
		return nil
	}
	c.innerIdx = idx
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
	if c.stale {
		return ErrCursorStale
	}
	if err := c.outerCursor.Err(); err != nil {
		if errors.Is(err, btree.ErrCursorStale) {
			return ErrCursorStale
		}
		if errors.Is(err, btree.ErrCorrupted) {
			return fmt.Errorf("%w: %v", ErrCorrupted, err)
		}
		return err
	}
	return nil
}

// --- internal helpers ---

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
	c.stale = false
}

// materializeAtOuter reads the current outerCursor's (key, cell)
// and populates currentKey + values. Caller is responsible for
// setting innerIdx after this call.
//
// Allocates new []byte for currentKey and each value (independent
// of leaf-buffer borrow). For nested-tree cells, walks the entire
// nested tree to materialize.
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
	c.values = c.values[:0]
	switch {
	case e.IsSubpage():
		sp := page.NewSubpageReader(e.Value, c.ks.desc.FixedValueSize)
		sp.AllValues(func(v []byte) bool {
			c.values = append(c.values, append([]byte(nil), v...))
			return true
		})
	case e.IsNestedTree():
		// Walk the nested tree's leaves and copy each key.
		nestedCfg := cfg
		inner := btree.NewReadCursor(c.tx.pgr, nestedCfg, e.NestedRoot)
		for nk, _ := inner.First(); nk != nil; nk, _ = inner.Next() {
			c.values = append(c.values, append([]byte(nil), nk...))
		}
		if err := inner.Err(); err != nil {
			return mapBtreeErr(err)
		}
	default:
		return fmt.Errorf("%w: SetCursor: unexpected cell flags 0x%x at key %q",
			ErrCorrupted, e.Flags, k)
	}
	if len(c.values) == 0 {
		return fmt.Errorf("%w: SetCursor: zero-value cell at key %q (Inv-1 violation)",
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
	c.innerIdx = 0
	return c.currentKey, c.values[0]
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
	c.innerIdx = len(c.values) - 1
	return c.currentKey, c.values[c.innerIdx]
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
	c.innerIdx = 0
	return c.currentKey, c.values[0]
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

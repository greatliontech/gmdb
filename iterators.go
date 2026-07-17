package gmdb

import (
	"bytes"
	"fmt"
	"iter"
)

// Byte-level read-only range iterators for Keyspace and SetKeyspace
// (api-surface.md §Range Iterators). Each returns an iter.Seq2 driven by
// a fresh read cursor, so it composes with `for k, v := range ks.All()`.
//
// Lifetime: the yielded key/value slices are borrowed from the snapshot
// (valid only for the duration of the yield call, like Cursor's
// returns) — copy anything retained past the loop body, per
// api-surface.md §Byte Slice Ownership.
//
// Errors: the iter.Seq2 has no error channel by design. A cursor read
// error ENDS the sequence, and the handle's Err() method reports it
// post-loop (api-surface.md §Range Iterators) — reset at the start of
// each sequence's iteration, so it describes the LAST sequence, the
// IndexHandle.Err contract. A clean end (exhaustion, a Range/Prefix
// bound, a caller break) leaves Err() nil. The typed layer
// (typed.KeyspaceHandle / typed.SetKeyspaceHandle) delegates to these
// and layers its decode/encode errors on top.
//
// Guard errors PANIC at construction (api-surface.md §Range
// Iterators): calling All/Range/Prefix on a handle whose transaction
// is frozen by an active child (ErrChildActive), closed
// (ErrTxClosed), or whose DB is closed (ErrClosed) is a programmer
// error — a silently empty sequence is indistinguishable from no
// data, and the Seq2 shape has no error channel to report through.
// Mid-loop state changes still end the sequence (the stale
// contract); the post-loop Err() check reports what ended it.
//
// Registration lifecycle: each closure registers its cursor while the
// loop is live (a loop-body mutation on the same keyspace must reach
// it via the staleness broadcast — which then ENDS the sequence; a
// fresh iterator resumes) and unregisters at loop exit, completed or
// broken, so iteration in a long transaction does not grow the
// per-mutation stale walk. An iter.Pull2 consumer that abandons next
// without calling stop leaks the registration for the tx lifetime —
// the same cost as an explicit Cursor(), and the iter.Pull contract
// already requires stop.

// All yields every (key, value) pair in ascending key order. Check
// Err() post-loop to distinguish exhaustion from an error-truncated
// sequence.
func (ks *Keyspace) All() iter.Seq2[[]byte, []byte] {
	guardIterConstruction(ks.tx, ks.dead)
	return func(yield func([]byte, []byte) bool) {
		ks.iterErr = nil // per-sequence reset: Err() reports the LAST sequence
		c := ks.Cursor()
		defer ks.unregisterCursor(c) // registered while live: the loop body may mutate
		for kb, vb := c.First(); kb != nil; kb, vb = c.Next() {
			if vb == nil && c.Err() != nil {
				// Overflow-value assembly failure: the cursor stays
				// positioned on the key with a nil value and the error
				// on Err() (btree adoptEntry's miss channel). End the
				// sequence with the error recorded instead of yielding
				// the phantom (key, nil) pair.
				ks.iterErr = c.Err()
				return
			}
			if !yield(kb, vb) {
				return
			}
		}
		ks.iterErr = c.Err()
	}
}

// Range yields (key, value) pairs whose key is in [start, end), in
// ascending key order. A nil start begins at the first key; a nil end
// runs to the last key. Check Err() post-loop to distinguish a clean
// end from an error-truncated sequence.
func (ks *Keyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte] {
	guardIterConstruction(ks.tx, ks.dead)
	return func(yield func([]byte, []byte) bool) {
		ks.iterErr = nil // per-sequence reset: Err() reports the LAST sequence
		c := ks.Cursor()
		defer ks.unregisterCursor(c)
		var kb, vb []byte
		if start != nil {
			kb, vb = c.SeekGE(start)
		} else {
			kb, vb = c.First()
		}
		for ; kb != nil; kb, vb = c.Next() {
			if end != nil && bytes.Compare(kb, end) >= 0 {
				return
			}
			if vb == nil && c.Err() != nil {
				ks.iterErr = c.Err() // see All: overflow-value miss channel
				return
			}
			if !yield(kb, vb) {
				return
			}
		}
		ks.iterErr = c.Err()
	}
}

// Prefix yields (key, value) pairs whose key begins with prefix, in
// ascending key order. A nil/empty prefix yields every pair. Check
// Err() post-loop to distinguish a clean end from an error-truncated
// sequence.
func (ks *Keyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte] {
	guardIterConstruction(ks.tx, ks.dead)
	return func(yield func([]byte, []byte) bool) {
		ks.iterErr = nil // per-sequence reset: Err() reports the LAST sequence
		c := ks.Cursor()
		defer ks.unregisterCursor(c)
		for kb, vb := c.SeekGE(prefix); kb != nil; kb, vb = c.Next() {
			if !bytes.HasPrefix(kb, prefix) {
				return
			}
			if vb == nil && c.Err() != nil {
				ks.iterErr = c.Err() // see All: overflow-value miss channel
				return
			}
			if !yield(kb, vb) {
				return
			}
		}
		ks.iterErr = c.Err()
	}
}

// All yields every (key, value) member pair across all keys' value sets,
// in ascending (key, value) order — each pair yields separately. Check
// Err() post-loop to distinguish exhaustion from an error-truncated
// sequence.
func (ks *SetKeyspace) All() iter.Seq2[[]byte, []byte] {
	guardIterConstruction(ks.tx, ks.dead)
	return func(yield func([]byte, []byte) bool) {
		ks.iterErr = nil // per-sequence reset: Err() reports the LAST sequence
		c := ks.Cursor()
		defer ks.unregisterSetCursor(c)
		for kb, vb := c.First(); kb != nil; kb, vb = c.Next() {
			if vb == nil && c.Err() != nil {
				ks.iterErr = c.Err() // see Keyspace.All: value miss channel
				return
			}
			if !yield(kb, vb) {
				return
			}
		}
		ks.iterErr = c.Err()
	}
}

// Range yields (key, value) member pairs whose key is in [start, end).
// A nil start begins at the first key; a nil end runs to the last key.
// Check Err() post-loop to distinguish a clean end from an
// error-truncated sequence.
func (ks *SetKeyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte] {
	guardIterConstruction(ks.tx, ks.dead)
	return func(yield func([]byte, []byte) bool) {
		ks.iterErr = nil // per-sequence reset: Err() reports the LAST sequence
		c := ks.Cursor()
		defer ks.unregisterSetCursor(c)
		var kb, vb []byte
		if start != nil {
			kb, vb = c.SeekGE(start)
		} else {
			kb, vb = c.First()
		}
		for ; kb != nil; kb, vb = c.Next() {
			if end != nil && bytes.Compare(kb, end) >= 0 {
				return
			}
			if vb == nil && c.Err() != nil {
				ks.iterErr = c.Err() // see Keyspace.All: value miss channel
				return
			}
			if !yield(kb, vb) {
				return
			}
		}
		ks.iterErr = c.Err()
	}
}

// Prefix yields (key, value) member pairs whose key begins with prefix.
// A nil/empty prefix yields every pair. Check Err() post-loop to
// distinguish a clean end from an error-truncated sequence.
func (ks *SetKeyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte] {
	guardIterConstruction(ks.tx, ks.dead)
	return func(yield func([]byte, []byte) bool) {
		ks.iterErr = nil // per-sequence reset: Err() reports the LAST sequence
		c := ks.Cursor()
		defer ks.unregisterSetCursor(c)
		for kb, vb := c.SeekGE(prefix); kb != nil; kb, vb = c.Next() {
			if !bytes.HasPrefix(kb, prefix) {
				return
			}
			if vb == nil && c.Err() != nil {
				ks.iterErr = c.Err() // see Keyspace.All: value miss channel
				return
			}
			if !yield(kb, vb) {
				return
			}
		}
		ks.iterErr = c.Err()
	}
}

// guardIterConstruction panics when an iterator is constructed on a
// handle whose state forbids every operation — a frozen parent
// (ErrChildActive), a closed transaction (ErrTxClosed), a closed DB
// (ErrClosed), or a DEAD keyspace handle (ErrKeyspaceClosed, the
// keyspace deleted in this transaction). See the package doc: the Seq2 shape
// has no error channel, and a silently empty sequence would be
// indistinguishable from no data (api-surface.md §Range Iterators).
func guardIterConstruction(tx *Tx, ksDead bool) {
	if err := tx.requireOpen(false); err != nil {
		panic(fmt.Sprintf("gmdb: iterator constructed on an unusable transaction: %v", err))
	}
	if ksDead {
		panic(fmt.Sprintf("gmdb: iterator constructed on a dead keyspace handle: %v", ErrKeyspaceClosed))
	}
}

// GuardIterConstruction fires the same construction-time panic the
// keyspace's own iterators fire when the transaction is unusable or
// the keyspace handle is dead. It exists for tiers that wrap this
// keyspace's iteration surfaces (gmdb/typed) and must fail their
// error short-circuit paths identically; callers rarely need it.
func (ks *Keyspace) GuardIterConstruction() {
	guardIterConstruction(ks.tx, ks.dead)
}

// GuardIterConstruction is the SetKeyspace form of
// (*Keyspace).GuardIterConstruction.
func (sks *SetKeyspace) GuardIterConstruction() {
	guardIterConstruction(sks.tx, sks.dead)
}

// iterHandleErr is the shared Err() cascade for both keyspace kinds.
// Broader handle truths win over the sticky per-sequence error,
// mirroring IndexHandle.Err: a dead handle reports ErrKeyspaceClosed,
// an unusable transaction its guard error (ErrTxClosed; ErrChildActive
// is transient and clears when the active child resolves).
func (kc *keyspaceCore) iterHandleErr() error {
	if kc.dead {
		return ErrKeyspaceClosed
	}
	if err := kc.tx.requireOpen(false); err != nil {
		return err
	}
	return kc.iterErr
}

// Err reports how the most recent All / Range / Prefix sequence on
// this handle ended: nil for a clean end (exhaustion, a Range end
// bound, a Prefix mismatch, or a caller break), otherwise the cursor
// error that truncated it — ErrCursorStale after a loop-body mutation
// invalidated the sequence, or an ErrCorrupted / ErrBadPageChecksum
// wrap on a structural read fault (api-surface.md §Range Iterators).
// The error is reset when the next sequence's iteration starts, so
// Err() always describes the LAST sequence — the IndexHandle.Err
// contract. Broader handle truths win: a dead handle reports
// ErrKeyspaceClosed, a closed transaction ErrTxClosed, a parent frozen
// by an active child ErrChildActive (transient). Like every handle
// surface, concurrent iteration on one handle races on Err.
func (ks *Keyspace) Err() error {
	return ks.iterHandleErr()
}

// Err is the SetKeyspace form of (*Keyspace).Err.
func (sks *SetKeyspace) Err() error {
	return sks.iterHandleErr()
}

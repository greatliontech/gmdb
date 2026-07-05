package gmdb

import (
	"bytes"
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
// Errors: the iter.Seq2 has no error channel by design. A cursor I/O
// error simply ends the sequence (the cursor returns a nil key); callers
// that must observe such errors should iterate with Cursor() and check
// Err(). The typed layer (TypedKeyspaceHandle / TypedSetKeyspaceHandle) delegates to these.
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

// All yields every (key, value) pair in ascending key order.
func (ks *Keyspace) All() iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		c := ks.Cursor()
		defer ks.unregisterCursor(c) // registered while live: the loop body may mutate
		for kb, vb := c.First(); kb != nil; kb, vb = c.Next() {
			if !yield(kb, vb) {
				return
			}
		}
	}
}

// Range yields (key, value) pairs whose key is in [start, end), in
// ascending key order. A nil start begins at the first key; a nil end
// runs to the last key.
func (ks *Keyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
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
			if !yield(kb, vb) {
				return
			}
		}
	}
}

// Prefix yields (key, value) pairs whose key begins with prefix, in
// ascending key order. A nil/empty prefix yields every pair.
func (ks *Keyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		c := ks.Cursor()
		defer ks.unregisterCursor(c)
		for kb, vb := c.SeekGE(prefix); kb != nil; kb, vb = c.Next() {
			if !bytes.HasPrefix(kb, prefix) {
				return
			}
			if !yield(kb, vb) {
				return
			}
		}
	}
}

// All yields every (key, value) member pair across all keys' value sets,
// in ascending (key, value) order — each pair yields separately.
func (ks *SetKeyspace) All() iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		c := ks.Cursor()
		defer ks.unregisterSetCursor(c)
		for kb, vb := c.First(); kb != nil; kb, vb = c.Next() {
			if !yield(kb, vb) {
				return
			}
		}
	}
}

// Range yields (key, value) member pairs whose key is in [start, end).
// A nil start begins at the first key; a nil end runs to the last key.
func (ks *SetKeyspace) Range(start, end []byte) iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
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
			if !yield(kb, vb) {
				return
			}
		}
	}
}

// Prefix yields (key, value) member pairs whose key begins with prefix.
// A nil/empty prefix yields every pair.
func (ks *SetKeyspace) Prefix(prefix []byte) iter.Seq2[[]byte, []byte] {
	return func(yield func([]byte, []byte) bool) {
		c := ks.Cursor()
		defer ks.unregisterSetCursor(c)
		for kb, vb := c.SeekGE(prefix); kb != nil; kb, vb = c.Next() {
			if !bytes.HasPrefix(kb, prefix) {
				return
			}
			if !yield(kb, vb) {
				return
			}
		}
	}
}

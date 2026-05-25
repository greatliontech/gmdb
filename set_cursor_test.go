package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// Chunk-6.7 SetCursor tests. Promotes the entailed invariant E4
// (NextValue does not cross key boundaries) and pins the
// core/value navigation contract per api-surface.md §SetCursor API
// + keyspaces.md §Iteration Semantics.

// helper: open a SetKeyspace + populate it with a deterministic set
// of (key, value) pairs.
func newSetKeyspaceWithData(t *testing.T, opts *SetKeyspaceOptions, pairs map[string][]string) (*SetKeyspace, *Tx, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx, true)
	if err != nil {
		db.Close()
		t.Fatalf("Begin: %v", err)
	}
	sks, err := tx.CreateSetKeyspace("k", opts)
	if err != nil {
		tx.Rollback()
		db.Close()
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	for k, vs := range pairs {
		for _, v := range vs {
			if _, err := sks.Put([]byte(k), []byte(v)); err != nil {
				tx.Rollback()
				db.Close()
				t.Fatalf("Put(%q,%q): %v", k, v, err)
			}
		}
	}
	cleanup := func() {
		tx.Rollback()
		db.Close()
	}
	return sks, tx, cleanup
}

// --- Core navigation ---

func TestSetCursorFirstOnEmpty(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, nil)
	defer cleanup()
	c := sks.Cursor()
	k, v := c.First()
	if k != nil || v != nil {
		t.Errorf("First on empty=(%q,%q), want (nil,nil)", k, v)
	}
}

func TestSetCursorFirstNextWalksAllPairs(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a", "b"},
		"k2": {"x"},
		"k3": {"p", "q", "r"},
	})
	defer cleanup()
	c := sks.Cursor()
	want := []string{
		"k1:a", "k1:b",
		"k2:x",
		"k3:p", "k3:q", "k3:r",
	}
	var got []string
	for k, v := c.First(); k != nil; k, v = c.Next() {
		got = append(got, fmt.Sprintf("%s:%s", k, v))
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestSetCursorPrevFromLast(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a", "b"},
		"k2": {"x", "y"},
	})
	defer cleanup()
	c := sks.Cursor()
	want := []string{"k2:y", "k2:x", "k1:b", "k1:a"}
	var got []string
	for k, v := c.Last(); k != nil; k, v = c.Prev() {
		got = append(got, fmt.Sprintf("%s:%s", k, v))
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestSetCursorSeekExact(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a"}, "k2": {"x", "y"}, "k3": {"p"},
	})
	defer cleanup()
	c := sks.Cursor()
	k, v := c.Seek([]byte("k2"))
	if string(k) != "k2" || string(v) != "x" {
		t.Errorf("Seek(k2)=(%q,%q), want (k2,x)", k, v)
	}
	// Seek miss returns nil,nil.
	c2 := sks.Cursor()
	k2, v2 := c2.Seek([]byte("missing"))
	if k2 != nil || v2 != nil {
		t.Errorf("Seek miss=(%q,%q), want (nil,nil)", k2, v2)
	}
}

func TestSetCursorSeekGE(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"alpha": {"1"}, "delta": {"2"}, "kilo": {"3", "4"},
	})
	defer cleanup()
	c := sks.Cursor()
	k, v := c.SeekGE([]byte("d"))
	if string(k) != "delta" || string(v) != "2" {
		t.Errorf("SeekGE(d)=(%q,%q), want (delta,2)", k, v)
	}
	// past end.
	c2 := sks.Cursor()
	k2, _ := c2.SeekGE([]byte("zulu"))
	if k2 != nil {
		t.Errorf("SeekGE past end: got %q", k2)
	}
}

// --- Value navigation (E4 enforcement) ---

func TestSetCursorNextValueDoesNotCrossKeys(t *testing.T) {
	// E4 regression: NextValue from the last value of a key must
	// transition to value-EOF (return nil), NOT advance to the
	// next key's first value.
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a", "b", "c"},
		"k2": {"x", "y"},
	})
	defer cleanup()
	c := sks.Cursor()
	// Walk k1's values via NextValue.
	c.Seek([]byte("k1"))
	got := []string{string(c.values[0])}
	for v := c.NextValue(); v != nil; v = c.NextValue() {
		got = append(got, string(v))
	}
	want := []string{"a", "b", "c"}
	if !slices.Equal(got, want) {
		t.Errorf("NextValue walk: got %v, want %v (must not cross to k2)", got, want)
	}
	// After value-EOF, the cursor is still positioned on k1.
	// CountValues should still report k1's count.
	count, _ := c.CountValues()
	if count != 3 {
		t.Errorf("CountValues post-EOF=%d, want 3", count)
	}
	// Now call Next() — SHOULD cross to k2's first value per the
	// "Next walks across keys" contract.
	k, v := c.Next()
	if string(k) != "k2" || string(v) != "x" {
		t.Errorf("Next post-NextValue-EOF=(%q,%q), want (k2,x)", k, v)
	}
}

func TestSetCursorPrevValueDoesNotCrossKeys(t *testing.T) {
	// E4 symmetric: PrevValue from the first value of a key
	// transitions to value-BOF (return nil), NOT to the previous
	// key.
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a", "b"},
		"k2": {"x", "y", "z"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.Seek([]byte("k2"))
	c.LastValue() // position at z
	got := []string{"z"}
	for v := c.PrevValue(); v != nil; v = c.PrevValue() {
		got = append(got, string(v))
	}
	want := []string{"z", "y", "x"}
	if !slices.Equal(got, want) {
		t.Errorf("PrevValue walk: got %v, want %v (must not cross to k1)", got, want)
	}
	// After BOF, Prev() crosses to k1's LAST value (Prev's contract).
	k, v := c.Prev()
	if string(k) != "k1" || string(v) != "b" {
		t.Errorf("Prev post-PrevValue-BOF=(%q,%q), want (k1,b)", k, v)
	}
}

func TestSetCursorNextKeySkipsValues(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a", "b", "c"},
		"k2": {"x", "y", "z"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.First() // (k1, a)
	c.NextValue() // (k1, b)
	// NextKey skips c, advances to k2's first value.
	k, v := c.NextKey()
	if string(k) != "k2" || string(v) != "x" {
		t.Errorf("NextKey=(%q,%q), want (k2,x)", k, v)
	}
}

func TestSetCursorPrevKeyGoesToFirstValueOfPrevKey(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a", "b"},
		"k2": {"x"},
		"k3": {"p", "q"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.Seek([]byte("k3"))
	c.NextValue() // (k3, q)
	k, v := c.PrevKey()
	if string(k) != "k2" || string(v) != "x" {
		t.Errorf("PrevKey=(%q,%q), want (k2,x)", k, v)
	}
	// PrevKey again → (k1, a).
	k2, v2 := c.PrevKey()
	if string(k2) != "k1" || string(v2) != "a" {
		t.Errorf("PrevKey 2nd=(%q,%q), want (k1,a)", k2, v2)
	}
}

func TestSetCursorSeekValueWithinKey(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"apple", "banana", "cherry", "date"},
		"k2": {"x"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.Seek([]byte("k1"))
	v := c.SeekValue([]byte("cherry"))
	if string(v) != "cherry" {
		t.Errorf("SeekValue(cherry)=%q, want cherry", v)
	}
	// SeekValue miss returns nil (and doesn't cross keys).
	v2 := c.SeekValue([]byte("zulu"))
	if v2 != nil {
		t.Errorf("SeekValue miss=%q, want nil", v2)
	}
	// Next from cherry → date.
	_, v3 := c.Next()
	if string(v3) != "date" {
		// Note: after a missed SeekValue, the cursor's innerIdx
		// is unchanged (we still pointed at cherry). Next then
		// moves to next value (date).
		t.Errorf("Next post-Seek=%q, want date", v3)
	}
}

func TestSetCursorFirstValueLastValue(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"apple", "banana", "cherry"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.First()
	v := c.LastValue()
	if string(v) != "cherry" {
		t.Errorf("LastValue=%q, want cherry", v)
	}
	v2 := c.FirstValue()
	if string(v2) != "apple" {
		t.Errorf("FirstValue=%q, want apple", v2)
	}
}

func TestSetCursorCountValues(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a", "b"},
		"k2": {"x", "y", "z", "w"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.Seek([]byte("k2"))
	count, err := c.CountValues()
	if err != nil || count != 4 {
		t.Errorf("CountValues(k2)=(%d,%v), want (4,nil)", count, err)
	}
}

// --- Nested-tree iteration ---

func TestSetCursorIteratesAcrossNestedTreeCells(t *testing.T) {
	// Build a SetKeyspace with one key promoted to nested tree
	// (~200 values) and verify Cursor walks all values then crosses
	// to the next key.
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx, true)
	defer tx.Rollback()

	sks, _ := tx.CreateSetKeyspace("k", nil)
	N := 200
	for i := range N {
		v := make([]byte, 30)
		v[0] = byte(i / 256)
		v[1] = byte(i % 256)
		sks.Put([]byte("aaa-bigkey"), v)
	}
	sks.Put([]byte("zzz-smallkey"), []byte("only"))

	c := sks.Cursor()
	var bigCount, smallCount int
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		switch string(k) {
		case "aaa-bigkey":
			bigCount++
		case "zzz-smallkey":
			smallCount++
		}
	}
	if bigCount != N {
		t.Errorf("bigkey count=%d, want %d", bigCount, N)
	}
	if smallCount != 1 {
		t.Errorf("smallkey count=%d, want 1", smallCount)
	}
}

// --- Delete ---

func TestSetCursorDeleteUnpositionedReturnsErr(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a"},
	})
	defer cleanup()
	c := sks.Cursor()
	if err := c.Delete(); !errors.Is(err, ErrCursorUnpositioned) {
		t.Errorf("Delete unpositioned: err=%v, want ErrCursorUnpositioned", err)
	}
}

func TestSetCursorDeleteAdvancesToNextValue(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a", "b", "c"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.First() // (k1, a)
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Cursor should now be at (k1, b).
	k, v := c.Current()
	if string(k) != "k1" || string(v) != "b" {
		t.Errorf("post-Delete Current=(%q,%q), want (k1,b)", k, v)
	}
	count, _ := sks.CountValues([]byte("k1"))
	if count != 2 {
		t.Errorf("CountValues post-Delete=%d, want 2", count)
	}
}

func TestSetCursorDeleteLastValueAdvancesToNextKey(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"only"},
		"k2": {"x"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.First() // (k1, only)
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// k1 is now gone (Inv-1); cursor advances to k2.
	k, v := c.Current()
	if string(k) != "k2" || string(v) != "x" {
		t.Errorf("post-Delete-last Current=(%q,%q), want (k2,x)", k, v)
	}
	has, _ := sks.Has([]byte("k1"))
	if has {
		t.Errorf("Has(k1) post-Delete-last: want false")
	}
}

func TestSetCursorDeleteEndOfIteration(t *testing.T) {
	// Delete the last (key, value) → cursor transitions to EOI.
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.First()
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	k, v := c.Current()
	if k != nil || v != nil {
		t.Errorf("post-Delete-last-of-tree Current=(%q,%q), want (nil,nil)", k, v)
	}
}

// --- Sibling-mutation invalidation ---

func TestSetCursorSiblingPutMarksOtherStale(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a"},
	})
	defer cleanup()
	c1 := sks.Cursor()
	c1.First()
	c2 := sks.Cursor()
	c2.First()
	// Mutate via c2.Delete (or just via sks.Put — same effect on
	// siblings).
	sks.Put([]byte("k2"), []byte("new"))
	// c1's Next should surface ErrCursorStale.
	_, _ = c1.Next()
	if err := c1.Err(); !errors.Is(err, ErrCursorStale) {
		t.Errorf("c1.Err post-sibling-Put=%v, want ErrCursorStale", err)
	}
}

// --- Commit-reopen iteration consistency ---

func TestSetCursorCommitReopenIteration(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})

	tx, _ := db.Begin(ctx, true)
	sks, _ := tx.CreateSetKeyspace("k", nil)
	sks.Put([]byte("a"), []byte("1"))
	sks.Put([]byte("a"), []byte("2"))
	sks.Put([]byte("b"), []byte("x"))
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	db.Close()

	db2, _ := Open(ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db2.Close()
	tx2, _ := db2.Begin(ctx, true)
	defer tx2.Rollback()
	sks2, _ := tx2.OpenSetKeyspace("k")
	c := sks2.Cursor()
	var got []string
	for k, v := c.First(); k != nil; k, v = c.Next() {
		got = append(got, fmt.Sprintf("%s:%s", k, v))
	}
	want := []string{"a:1", "a:2", "b:x"}
	if !slices.Equal(got, want) {
		t.Errorf("post-reopen iter: got %v, want %v", got, want)
	}
}

// --- Empty-iter / unpositioned edges ---

func TestSetCursorCurrentUnpositioned(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, nil)
	defer cleanup()
	c := sks.Cursor()
	k, v := c.Current()
	if k != nil || v != nil {
		t.Errorf("Current unpositioned=(%q,%q), want (nil,nil)", k, v)
	}
}

func TestSetCursorNextValueUnpositioned(t *testing.T) {
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, nil)
	defer cleanup()
	c := sks.Cursor()
	v := c.NextValue()
	if v != nil {
		t.Errorf("NextValue unpositioned=%q, want nil", v)
	}
}

// --- DeleteKeyspace marks SetCursor stale ---

func TestSetCursorDeadOnDeleteKeyspace(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx, true)
	defer tx.Rollback()

	sks, _ := tx.CreateSetKeyspace("k", nil)
	sks.Put([]byte("a"), []byte("1"))
	c := sks.Cursor()
	c.First()
	if err := tx.DeleteKeyspace("k"); err != nil {
		t.Fatalf("DeleteKeyspace: %v", err)
	}
	// Cursor ops should surface ErrKeyspaceClosed.
	if k, v := c.Next(); k != nil || v != nil {
		t.Errorf("Next post-DeleteKeyspace=(%q,%q), want (nil,nil)", k, v)
	}
	if err := c.Err(); !errors.Is(err, ErrKeyspaceClosed) {
		t.Errorf("Err post-DeleteKeyspace=%v, want ErrKeyspaceClosed", err)
	}
}

// --- Fixed-size SetKeyspace cursor (L-2 coverage) ---

func TestSetCursorFixedSizeIteration(t *testing.T) {
	// Materialization path uses ks.desc.FixedValueSize when
	// constructing the SubpageReader; verify fixed-size subpage
	// decoding works correctly through the cursor.
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx, true)
	defer tx.Rollback()

	sks, _ := tx.CreateSetKeyspace("k", &SetKeyspaceOptions{FixedValueSize: 4})
	values := [][]byte{
		{0, 0, 0, 1}, {0, 0, 0, 2}, {0, 0, 0, 3},
	}
	for _, v := range values {
		if _, err := sks.Put([]byte("topic"), v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	c := sks.Cursor()
	var got [][]byte
	for k, v := c.First(); k != nil; k, v = c.Next() {
		dup := append([]byte(nil), v...)
		got = append(got, dup)
	}
	if len(got) != 3 {
		t.Fatalf("iter count=%d, want 3", len(got))
	}
	for i, want := range values {
		if !bytes.Equal(got[i], want) {
			t.Errorf("iter[%d]=%x, want %x", i, got[i], want)
		}
	}
}

// --- SetCursor.Delete on a nested-tree cell (L-2 coverage) ---

func TestSetCursorDeleteOnNestedTreeAdvances(t *testing.T) {
	// Exercise SetCursor.Delete on a promoted (nested-tree) cell —
	// the post-delete cursor must position at the next value of
	// the same key (the nested-tree delete-then-demote path is
	// distinct from the subpage-in-place delete).
	ctx := context.Background()
	db, _ := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	defer db.Close()
	tx, _ := db.Begin(ctx, true)
	defer tx.Rollback()

	sks, _ := tx.CreateSetKeyspace("k", nil)
	N := 200
	for i := range N {
		v := make([]byte, 30)
		v[0] = byte(i / 256)
		v[1] = byte(i % 256)
		sks.Put([]byte("topic"), v)
	}
	c := sks.Cursor()
	c.First()
	// Delete first value of the nested tree.
	if err := c.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Cursor should now be at the SECOND-smallest value (the
	// post-delete first value).
	k, v := c.Current()
	if string(k) != "topic" {
		t.Errorf("post-delete Current key=%q, want topic", k)
	}
	if len(v) != 30 {
		t.Errorf("post-delete value len=%d, want 30", len(v))
	}
	count, _ := sks.CountValues([]byte("topic"))
	if count != uint64(N-1) {
		t.Errorf("CountValues post-delete=%d, want %d", count, N-1)
	}
}

// --- Stale-then-recover (L-2 coverage) ---

func TestSetCursorStaleClearsOnReposition(t *testing.T) {
	// After a sibling-Put marks the cursor stale, a re-position
	// (Seek / First / Last / SeekGE) clears the stale flag and
	// returns valid (key, value).
	sks, _, cleanup := newSetKeyspaceWithData(t, nil, map[string][]string{
		"k1": {"a", "b"},
	})
	defer cleanup()
	c := sks.Cursor()
	c.First() // positioned at (k1, a)
	sks.Put([]byte("k2"), []byte("x"))
	// Cursor is now stale; Next returns nil + ErrCursorStale.
	if _, _ = c.Next(); c.Err() == nil {
		t.Fatalf("expected stale Err post-sibling-Put")
	}
	// Re-position via First — should succeed and clear stale.
	k, v := c.First()
	if k == nil {
		t.Fatalf("First post-stale: got nil")
	}
	if string(k) != "k1" || string(v) != "a" {
		t.Errorf("First post-stale=(%q,%q), want (k1,a)", k, v)
	}
	if err := c.Err(); err != nil {
		t.Errorf("Err post-recover=%v, want nil (stale should be cleared)", err)
	}
}


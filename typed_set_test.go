package gmdb

import (
	"context"
	"errors"
	"testing"
)

// newTypedSetTx opens a fresh DB + write tx for typed-set tests.
func newTypedSetTx(t *testing.T) (*Tx, func()) {
	t.Helper()
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 4096})
	tx, err := db.Begin(ctx)
	if err != nil {
		_ = db.Close()
		t.Fatalf("Begin: %v", err)
	}
	return tx, func() {
		_ = tx.Rollback()
		_ = db.Close()
	}
}

func TestTypedSetKSRoundTrip(t *testing.T) {
	tx, cleanup := newTypedSetTx(t)
	defer cleanup()

	tsk := NewTypedSetKeyspace[uint64, string]("subs", Uint64Encoder{}, StringEncoder{}, nil)
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Put members.
	if added, err := ks.Put(1, "a"); err != nil || !added {
		t.Errorf("Put(1,a) = (%v, %v), want (true, nil)", added, err)
	}
	if added, err := ks.Put(1, "b"); err != nil || !added {
		t.Errorf("Put(1,b) = (%v, %v), want (true, nil)", added, err)
	}
	if added, err := ks.Put(2, "c"); err != nil || !added {
		t.Errorf("Put(2,c) = (%v, %v), want (true, nil)", added, err)
	}
	// Duplicate member: added=false.
	if added, err := ks.Put(1, "a"); err != nil || added {
		t.Errorf("Put(1,a) again = (%v, %v), want (false, nil)", added, err)
	}
	// Has / HasValue / CountValues.
	if has, err := ks.Has(1); err != nil || !has {
		t.Errorf("Has(1) = (%v, %v), want (true, nil)", has, err)
	}
	if has, err := ks.HasValue(1, "a"); err != nil || !has {
		t.Errorf("HasValue(1,a) = (%v, %v), want (true, nil)", has, err)
	}
	if has, err := ks.HasValue(1, "z"); err != nil || has {
		t.Errorf("HasValue(1,z) = (%v, %v), want (false, nil)", has, err)
	}
	if n, err := ks.CountValues(1); err != nil || n != 2 {
		t.Errorf("CountValues(1) = (%d, %v), want (2, nil)", n, err)
	}
	// DeleteValue + Delete.
	if err := ks.DeleteValue(1, "a"); err != nil {
		t.Fatalf("DeleteValue(1,a): %v", err)
	}
	if n, _ := ks.CountValues(1); n != 1 {
		t.Errorf("CountValues(1) after DeleteValue = %d, want 1", n)
	}
	if err := ks.DeleteValue(1, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteValue(1,a) again = %v, want ErrNotFound", err)
	}
	if err := ks.Delete(2); err != nil {
		t.Fatalf("Delete(2): %v", err)
	}
	if has, _ := ks.Has(2); has {
		t.Error("Has(2) after Delete = true, want false")
	}
	if err := ks.Delete(2); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete(2) again = %v, want ErrNotFound", err)
	}
}

// collectSetMembers returns "k/v" strings for every member via All().
func collectSetMembers(ks *TypedSetKeyspaceHandle[uint64, string]) []string {
	var out []string
	for k, v := range ks.All() {
		out = append(out, itoa(k)+"/"+v)
	}
	return out
}

func itoa(u uint64) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}

func TestTypedSetKSCursorAndIterators(t *testing.T) {
	tx, cleanup := newTypedSetTx(t)
	defer cleanup()

	tsk := NewTypedSetKeyspace[uint64, string]("subs", Uint64Encoder{}, StringEncoder{}, nil)
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	members := []struct {
		k uint64
		v string
	}{{1, "a"}, {1, "b"}, {2, "c"}, {3, "x"}, {3, "y"}, {3, "z"}}
	for _, m := range members {
		if _, err := ks.Put(m.k, m.v); err != nil {
			t.Fatalf("Put(%d,%s): %v", m.k, m.v, err)
		}
	}

	// Member-level cursor First/Next yields (key,value) lex order.
	c := ks.Cursor()
	var got []string
	for k, v, ok := c.First(); ok; k, v, ok = c.Next() {
		got = append(got, itoa(k)+"/"+v)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("cursor Err: %v", err)
	}
	want := []string{"1/a", "1/b", "2/c", "3/x", "3/y", "3/z"}
	if !equalStr(got, want) {
		t.Errorf("cursor members = %v, want %v", got, want)
	}

	// All matches.
	if all := collectSetMembers(ks); !equalStr(all, want) {
		t.Errorf("All = %v, want %v", all, want)
	}

	// Range [2,3) = key-2 members only.
	var rng []string
	for k, v := range ks.Range(ptr(uint64(2)), ptr(uint64(3))) {
		rng = append(rng, itoa(k)+"/"+v)
	}
	if want := []string{"2/c"}; !equalStr(rng, want) {
		t.Errorf("Range(2,3) = %v, want %v", rng, want)
	}

	// Range [3, nil) = all key-3 members.
	var tail []string
	for k, v := range ks.Range(ptr(uint64(3)), nil) {
		tail = append(tail, itoa(k)+"/"+v)
	}
	if want := []string{"3/x", "3/y", "3/z"}; !equalStr(tail, want) {
		t.Errorf("Range(3,nil) = %v, want %v", tail, want)
	}

	// Cursor Delete removes the current member.
	c2 := ks.Cursor()
	if _, _, ok := c2.SeekGE(3); !ok {
		t.Fatal("SeekGE(3) not ok")
	}
	if err := c2.Delete(); err != nil {
		t.Fatalf("cursor Delete: %v", err)
	}
	if n, _ := ks.CountValues(3); n != 2 {
		t.Errorf("CountValues(3) after cursor delete = %d, want 2", n)
	}
}

// TestTypedSetKSEmptyValueMember pins the load-bearing edge of the
// member-only cursor design: an empty-bytes set value must round-trip
// (the member cursor keys its end sentinel on the never-empty KEY, so an
// empty VALUE member is decoded, not mistaken for end-of-iteration).
func TestTypedSetKSEmptyValueMember(t *testing.T) {
	tx, cleanup := newTypedSetTx(t)
	defer cleanup()

	tsk := NewTypedSetKeyspace[uint64, string]("subs", Uint64Encoder{}, StringEncoder{}, nil)
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := ks.Put(1, ""); err != nil {
		t.Fatalf("Put(1,\"\"): %v", err)
	}
	if _, err := ks.Put(1, "b"); err != nil {
		t.Fatalf("Put(1,b): %v", err)
	}
	if has, err := ks.HasValue(1, ""); err != nil || !has {
		t.Errorf("HasValue(1,\"\") = (%v, %v), want (true, nil)", has, err)
	}
	if n, _ := ks.CountValues(1); n != 2 {
		t.Errorf("CountValues(1) = %d, want 2 (empty value counts as a member)", n)
	}
	// Cursor First must decode the empty-value member (not treat it as end).
	c := ks.Cursor()
	k, v, ok := c.First()
	if !ok || k != 1 || v != "" {
		t.Errorf("First() = (%d, %q, %v), want (1, \"\", true)", k, v, ok)
	}
	// All yields both members ("" sorts before "b").
	var got []string
	for k, v := range ks.All() {
		got = append(got, itoa(k)+"/"+v)
	}
	if want := []string{"1/", "1/b"}; !equalStr(got, want) {
		t.Errorf("All = %v, want %v", got, want)
	}
}

// TestTypedSetKSPrefix exercises variable-width prefix matching over a set
// keyspace (members of every key whose encoding has the prefix).
func TestTypedSetKSPrefix(t *testing.T) {
	tx, cleanup := newTypedSetTx(t)
	defer cleanup()

	tsk := NewTypedSetKeyspace[string, string]("words", StringEncoder{}, StringEncoder{}, nil)
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, m := range []struct{ k, v string }{
		{"ap", "1"}, {"app", "2"}, {"app", "3"}, {"banana", "x"},
	} {
		if _, err := ks.Put(m.k, m.v); err != nil {
			t.Fatalf("Put(%s,%s): %v", m.k, m.v, err)
		}
	}
	var got []string
	for k, v := range ks.Prefix("app") {
		got = append(got, k+"/"+v)
	}
	if want := []string{"app/2", "app/3"}; !equalStr(got, want) {
		t.Errorf("Prefix(app) = %v, want %v", got, want)
	}
}

// TestTypedSetCursorLastPrevSeek covers the member-cursor navigation the
// other tests don't (Last / Prev / exact Seek).
func TestTypedSetCursorLastPrevSeek(t *testing.T) {
	tx, cleanup := newTypedSetTx(t)
	defer cleanup()

	tsk := NewTypedSetKeyspace[uint64, string]("subs", Uint64Encoder{}, StringEncoder{}, nil)
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, m := range []struct {
		k uint64
		v string
	}{{1, "a"}, {2, "b"}, {2, "c"}, {3, "d"}} {
		if _, err := ks.Put(m.k, m.v); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	c := ks.Cursor()
	// Last → (3, d).
	if k, v, ok := c.Last(); !ok || k != 3 || v != "d" {
		t.Errorf("Last() = (%d, %q, %v), want (3, d, true)", k, v, ok)
	}
	// Prev from last walks members backward: (2,c).
	if k, v, ok := c.Prev(); !ok || k != 2 || v != "c" {
		t.Errorf("Prev() = (%d, %q, %v), want (2, c, true)", k, v, ok)
	}
	// Seek exact to key 2 lands on its first member (2, b).
	c2 := ks.Cursor()
	if k, v, ok := c2.Seek(2); !ok || k != 2 || v != "b" {
		t.Errorf("Seek(2) = (%d, %q, %v), want (2, b, true)", k, v, ok)
	}
}

func TestTypedSetKSReadOnly(t *testing.T) {
	ctx := context.Background()
	path := tmpPath(t)
	db := openWith(t, ctx, path, Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tsk := NewTypedSetKeyspace[uint64, string]("subs", Uint64Encoder{}, StringEncoder{}, nil)

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := ks.Put(1, "a"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rtx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer rtx.Rollback()
	rks, err := tsk.OpenReadOnly(rtx)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if has, err := rks.HasValue(1, "a"); err != nil || !has {
		t.Errorf("HasValue(1,a) = (%v, %v), want (true, nil)", has, err)
	}
	if _, err := rks.Put(1, "b"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Put on read-only = %v, want ErrReadOnly", err)
	}
}

// TestTypedSetKSFixedValueSize exercises the opts plumbing: a
// FixedValueSize set with a fixed-width (8-byte) value encoder.
func TestTypedSetKSFixedValueSize(t *testing.T) {
	tx, cleanup := newTypedSetTx(t)
	defer cleanup()

	tsk := NewTypedSetKeyspace[uint64, uint64]("fixed", Uint64Encoder{}, Uint64Encoder{},
		&SetKeyspaceOptions{FixedValueSize: 8})
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, v := range []uint64{30, 10, 20} {
		if _, err := ks.Put(1, v); err != nil {
			t.Fatalf("Put(1,%d): %v", v, err)
		}
	}
	if n, _ := ks.CountValues(1); n != 3 {
		t.Errorf("CountValues(1) = %d, want 3", n)
	}
	// Values come back in ascending uint64 order (BE encoding lex order).
	var got []uint64
	c := ks.Cursor()
	for k, v, ok := c.First(); ok; k, v, ok = c.Next() {
		if k != 1 {
			t.Fatalf("unexpected key %d", k)
		}
		got = append(got, v)
	}
	if want := []uint64{10, 20, 30}; !equalU64(got, want) {
		t.Errorf("fixed-size set values = %v, want %v (sorted)", got, want)
	}
}

// TestTypedSetCursorSignedOrder verifies signed key lex order through the
// set cursor (Inv-T1).
func TestTypedSetCursorSignedOrder(t *testing.T) {
	ctx := context.Background()
	db := openWith(t, ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	defer db.Close()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	tsk := NewTypedSetKeyspace[int64, string]("signed", Int64Encoder{}, StringEncoder{}, nil)
	ks, err := tsk.Create(tx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, k := range []int64{5, -3, 0, -100, 1} {
		if _, err := ks.Put(k, "v"); err != nil {
			t.Fatalf("Put(%d): %v", k, err)
		}
	}
	var got []int64
	c := ks.Cursor()
	for k, _, ok := c.First(); ok; k, _, ok = c.Next() {
		got = append(got, k)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []int64{-100, -3, 0, 1, 5}
	if len(got) != len(want) {
		t.Fatalf("signed set order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("signed set order = %v, want %v", got, want)
		}
	}
}

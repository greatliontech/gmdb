package gmdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

// DeletePrefix (api-surface.md §Keyspace API / §SetKeyspace API):
// deletes [prefix, prefixSuccessor(prefix)) with nil/empty = every
// key — the Prefix iterator's convention. The load-bearing property
// is BOUNDARY EXACTNESS: keys adjacent to the prefix range on either
// side survive.

func deletePrefixFixture(t *testing.T) (*Tx, *Keyspace) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("k")
	if err != nil {
		t.Fatalf("CreateKeyspace: %v", err)
	}
	return tx, ks
}

func TestKeyspaceDeletePrefixBoundaryExactness(t *testing.T) {
	_, ks := deletePrefixFixture(t)
	// Neighbors on both sides of the "ab" prefix range, including the
	// tightest adjacents: "ab" itself is IN range; "aa\xff" and "ac"
	// are out; "ab\xff\xff" is in.
	keys := []string{"aa", "aa\xff", "ab", "ab0", "ab\xff\xff", "abc", "ac", "b"}
	for _, k := range keys {
		if err := ks.Put([]byte(k), []byte("v")); err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
	}
	n, err := ks.DeletePrefix([]byte("ab"))
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if n != 4 { // ab, ab0, ab\xff\xff, abc
		t.Fatalf("deleted %d, want 4", n)
	}
	for _, k := range []string{"aa", "aa\xff", "ac", "b"} {
		if _, err := ks.Get([]byte(k)); err != nil {
			t.Errorf("survivor %q gone: %v", k, err)
		}
	}
	for _, k := range []string{"ab", "ab0", "abc"} {
		if _, err := ks.Get([]byte(k)); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", k, err)
		}
	}
}

func TestKeyspaceDeletePrefixAllFFPrefix(t *testing.T) {
	// An all-0xFF prefix has no successor — the range is upper-open.
	_, ks := deletePrefixFixture(t)
	for _, k := range []string{"\xfe", "\xff", "\xff\x00", "\xffz"} {
		if err := ks.Put([]byte(k), []byte("v")); err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
	}
	n, err := ks.DeletePrefix([]byte{0xFF})
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if n != 3 {
		t.Fatalf("deleted %d, want 3", n)
	}
	if _, err := ks.Get([]byte("\xfe")); err != nil {
		t.Errorf("survivor \\xfe gone: %v", err)
	}
}

func TestKeyspaceDeletePrefixNilDeletesEverything(t *testing.T) {
	_, ks := deletePrefixFixture(t)
	for i := range 10 {
		if err := ks.Put(fmt.Appendf(nil, "k%02d", i), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	n, err := ks.DeletePrefix(nil)
	if err != nil || n != 10 {
		t.Fatalf("DeletePrefix(nil) = (%d, %v), want (10, nil)", n, err)
	}
	// Empty (non-nil) prefix: same convention as the Prefix iterator.
	if err := ks.Put([]byte("x"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	n, err = ks.DeletePrefix([]byte{})
	if err != nil || n != 1 {
		t.Fatalf("DeletePrefix(empty) = (%d, %v), want (1, nil)", n, err)
	}
}

func TestKeyspaceDeletePrefixIndexedFallback(t *testing.T) {
	// The indexed keyspace routes through DeleteRange's per-row
	// fallback — index rows for deleted keys must go too.
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	decl := &IndexDecl{Name: "byv", Columns: []IndexColumn{{Name: "v"}},
		Extract: func(_, v []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{bytes.Clone(v)}}}
		}}
	tx, _ := db.Begin(ctx)
	ks, err := tx.CreateKeyspace("k", decl)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		prefix := "keep"
		if i%2 == 0 {
			prefix = "drop"
		}
		if err := ks.Put(fmt.Appendf(nil, "%s%02d", prefix, i), fmt.Appendf(nil, "val%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := ks.DeletePrefix([]byte("drop"))
	if err != nil || n != 4 {
		t.Fatalf("DeletePrefix(drop) = (%d, %v), want (4, nil)", n, err)
	}
	idx, err := ks.Index("byv")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		found := false
		for range idx.LookupKeys([][]byte{fmt.Appendf(nil, "val%02d", i)}) {
			found = true
		}
		if err := idx.Err(); err != nil {
			t.Fatal(err)
		}
		wantFound := i%2 == 1
		if found != wantFound {
			t.Errorf("index row val%02d: found=%v, want %v", i, found, wantFound)
		}
	}
}

func TestSetKeyspaceDeletePrefix(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256,
		Maintenance: MaintenanceOptions{Disable: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, _ := db.Begin(ctx)
	sks, err := tx.CreateSetKeyspace("s", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two members per key; count is MEMBER-value count.
	for _, k := range []string{"aa", "ab", "ab0", "ac"} {
		for _, m := range []string{"m1", "m2"} {
			if _, err := sks.Put([]byte(k), []byte(m)); err != nil {
				t.Fatalf("Put(%q,%q): %v", k, m, err)
			}
		}
	}
	n, err := sks.DeletePrefix([]byte("ab"))
	if err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	if n != 4 { // 2 keys x 2 members
		t.Fatalf("deleted %d member values, want 4", n)
	}
	for _, k := range []string{"aa", "ac"} {
		has, err := sks.HasValue([]byte(k), []byte("m1"))
		if err != nil || !has {
			t.Errorf("survivor %q member gone: has=%v err=%v", k, has, err)
		}
	}
	has, err := sks.HasValue([]byte("ab"), []byte("m1"))
	if err != nil || has {
		t.Errorf("deleted key ab still has members: has=%v err=%v", has, err)
	}
	// nil prefix = everything.
	n, err = sks.DeletePrefix(nil)
	if err != nil || n != 4 {
		t.Fatalf("DeletePrefix(nil) = (%d, %v), want (4, nil)", n, err)
	}
}

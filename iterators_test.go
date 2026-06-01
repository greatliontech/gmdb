package gmdb

import (
	"context"
	"slices"
	"testing"
)

// TestKeyspaceByteIterators covers Keyspace.All/Range/Prefix: ordering,
// values, the [start,end) bounds (nil = unbounded), prefix matching, and
// early break.
func TestKeyspaceByteIterators(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Update(ctx, func(tx *Tx) error {
		ks, err := tx.CreateKeyspace("ks")
		if err != nil {
			return err
		}
		for _, k := range []string{"a", "b", "c", "d", "e", "foo1", "foo2"} {
			if err := ks.Put([]byte(k), []byte("V"+k)); err != nil {
				return err
			}
		}

		// All — ascending order, correct values.
		var all []string
		for kb, vb := range ks.All() {
			if string(vb) != "V"+string(kb) {
				t.Errorf("All value for %q = %q, want %q", kb, vb, "V"+string(kb))
			}
			all = append(all, string(kb))
		}
		want := []string{"a", "b", "c", "d", "e", "foo1", "foo2"}
		if !slices.Equal(all, want) {
			t.Errorf("All keys = %v, want %v", all, want)
		}

		// Range [b, d) → b, c (start inclusive, end exclusive).
		if got := collectKeys(ks.Range([]byte("b"), []byte("d"))); !slices.Equal(got, []string{"b", "c"}) {
			t.Errorf("Range(b,d) = %v, want [b c]", got)
		}
		// Range(nil, c) → a, b. Range(c, nil) → c, d, e, foo1, foo2.
		if got := collectKeys(ks.Range(nil, []byte("c"))); !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("Range(nil,c) = %v, want [a b]", got)
		}
		if got := collectKeys(ks.Range([]byte("c"), nil)); !slices.Equal(got, []string{"c", "d", "e", "foo1", "foo2"}) {
			t.Errorf("Range(c,nil) = %v, want [c d e foo1 foo2]", got)
		}

		// Prefix("foo") → foo1, foo2.
		if got := collectKeys(ks.Prefix([]byte("foo"))); !slices.Equal(got, []string{"foo1", "foo2"}) {
			t.Errorf("Prefix(foo) = %v, want [foo1 foo2]", got)
		}

		// Early break: stop after the first pair.
		var n int
		for range ks.All() {
			n++
			break
		}
		if n != 1 {
			t.Errorf("early break visited %d, want 1", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

// TestSetKeyspaceByteIterators covers SetKeyspace.All/Range/Prefix: each
// (key, value) member pair yields separately, in (key, value) order.
func TestSetKeyspaceByteIterators(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Update(ctx, func(tx *Tx) error {
		sks, err := tx.CreateSetKeyspace("sks", nil)
		if err != nil {
			return err
		}
		members := []struct{ k, v string }{
			{"k1", "a"}, {"k1", "b"}, {"k2", "c"}, {"pfx1", "x"}, {"pfx2", "y"},
		}
		for _, m := range members {
			if _, err := sks.Put([]byte(m.k), []byte(m.v)); err != nil {
				return err
			}
		}

		// All — every (key, value) pair, separately, in order.
		if got := collectPairs(sks.All()); !slices.Equal(got, []string{"k1=a", "k1=b", "k2=c", "pfx1=x", "pfx2=y"}) {
			t.Errorf("All = %v", got)
		}
		// Range [k1, k2) → both k1 members only.
		if got := collectPairs(sks.Range([]byte("k1"), []byte("k2"))); !slices.Equal(got, []string{"k1=a", "k1=b"}) {
			t.Errorf("Range(k1,k2) = %v, want [k1=a k1=b]", got)
		}
		// Prefix("pfx") → the two pfx* members.
		if got := collectPairs(sks.Prefix([]byte("pfx"))); !slices.Equal(got, []string{"pfx1=x", "pfx2=y"}) {
			t.Errorf("Prefix(pfx) = %v, want [pfx1=x pfx2=y]", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func collectKeys(seq func(func([]byte, []byte) bool)) []string {
	var out []string
	for kb := range seq {
		out = append(out, string(kb))
	}
	return out
}

func collectPairs(seq func(func([]byte, []byte) bool)) []string {
	var out []string
	for kb, vb := range seq {
		out = append(out, string(kb)+"="+string(vb))
	}
	return out
}

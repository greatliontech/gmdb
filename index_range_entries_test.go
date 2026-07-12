package gmdb

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
)

// RangeEntries contract (api-surface.md §Index Lookup API): raw
// stored entries as (decoded column tuple + PK, verbatim value
// bytes) — no back-lookup, no covering interpretation, prefix
// bounds and IterOption shared with Range, Keyspace indexes only.

func newIndexedKeyspaceForEntries(t *testing.T, unique bool, covering []IndexCoveringColumn) (*Keyspace, *Tx, func()) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		db.Close()
		t.Fatalf("Begin: %v", err)
	}
	decl := &IndexDecl{
		Name:     "ix",
		Columns:  []IndexColumn{{Name: "a"}, {Name: "b"}},
		Unique:   unique,
		Covering: covering,
		Extract: func(k, v []byte) []IndexEntry {
			// Column a = the value's first byte run up to '|',
			// column b = the rest; covering (when declared) carries
			// the key reversed — distinguishable from every input.
			i := bytes.IndexByte(v, '|')
			e := IndexEntry{Cols: [][]byte{v[:i], v[i+1:]}}
			if len(covering) > 0 {
				r := make([]byte, len(k))
				for j := range k {
					r[j] = k[len(k)-1-j]
				}
				e.Cover = [][]byte{r}
			}
			return []IndexEntry{e}
		},
	}
	ks, err := tx.CreateKeyspace("k", decl)
	if err != nil {
		tx.Rollback()
		db.Close()
		t.Fatalf("CreateKeyspace: %v", err)
	}
	return ks, tx, func() { tx.Rollback(); db.Close() }
}

func TestIndexRangeEntriesYieldsSplitEntries(t *testing.T) {
	for _, unique := range []bool{false, true} {
		name := "non-unique"
		if unique {
			name = "unique"
		}
		t.Run(name, func(t *testing.T) {
			ks, _, cleanup := newIndexedKeyspaceForEntries(t, unique, nil)
			defer cleanup()
			// Values with embedded 0x00 exercise the NUL-escape
			// round-trip through the entry-key decode.
			rows := map[string]string{
				"k1": "a\x00x|b1",
				"k2": "a\x00x|b2",
				"k3": "z|c",
			}
			for k, v := range rows {
				if err := ks.Put([]byte(k), []byte(v)); err != nil {
					t.Fatalf("Put: %v", err)
				}
			}
			idx, err := ks.Index("ix")
			if err != nil {
				t.Fatalf("Index: %v", err)
			}
			type got struct{ a, b, pk, val string }
			var all []got
			for ek, vb := range idx.RangeEntries(nil, nil) {
				if len(ek.Cols) != 2 {
					t.Fatalf("entry has %d cols, want 2", len(ek.Cols))
				}
				all = append(all, got{string(ek.Cols[0]), string(ek.Cols[1]), string(ek.PK), string(vb)})
			}
			if err := idx.Err(); err != nil {
				t.Fatalf("Err: %v", err)
			}
			want := []got{
				{"a\x00x", "b1", "k1", ""},
				{"a\x00x", "b2", "k2", ""},
				{"z", "c", "k3", ""},
			}
			if !slices.Equal(all, want) {
				t.Fatalf("entries = %v, want %v", all, want)
			}

			// Prefix bound restricts to the first column group; the
			// value stays verbatim-empty for a non-covering decl.
			var pks []string
			for ek := range idx.RangeEntries([][]byte{[]byte("a\x00x")}, [][]byte{[]byte("a\x00x\x00")}) {
				pks = append(pks, string(ek.PK))
			}
			if idx.Err() != nil || !slices.Equal(pks, []string{"k1", "k2"}) {
				t.Fatalf("prefix-bounded entries = %v (err %v), want [k1 k2]", pks, idx.Err())
			}

			// Reverse yields the same set, element-wise reversed.
			var rev []string
			for ek := range idx.RangeEntries(nil, nil, Reverse()) {
				rev = append(rev, string(ek.PK))
			}
			if idx.Err() != nil || !slices.Equal(rev, []string{"k3", "k2", "k1"}) {
				t.Fatalf("reverse entries = %v (err %v), want [k3 k2 k1]", rev, idx.Err())
			}
		})
	}
}

func TestIndexRangeEntriesCoveringValueVerbatim(t *testing.T) {
	ks, _, cleanup := newIndexedKeyspaceForEntries(t, false, []IndexCoveringColumn{{Name: "c"}})
	defer cleanup()
	if err := ks.Put([]byte("key"), []byte("a|b")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := ks.Index("ix")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	n := 0
	for ek, vb := range idx.RangeEntries(nil, nil) {
		n++
		// The value is the encoded covering tuple VERBATIM — decode
		// via the public tuple decoder, never a row read.
		cols, err := DecodeCoveringTuple(vb)
		if err != nil {
			t.Fatalf("DecodeCoveringTuple: %v", err)
		}
		if len(cols) != 1 || string(cols[0]) != "yek" {
			t.Fatalf("covering slots = %q, want [yek]", cols)
		}
		if string(ek.PK) != "key" {
			t.Fatalf("pk = %q, want key", ek.PK)
		}
	}
	if n != 1 || idx.Err() != nil {
		t.Fatalf("yielded %d entries (err %v), want 1", n, idx.Err())
	}
}

func TestIndexRangeEntriesValidation(t *testing.T) {
	ks, _, cleanup := newIndexedKeyspaceForEntries(t, false, nil)
	defer cleanup()
	idx, err := ks.Index("ix")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	// More bound columns than declared → ErrInvalidOptions, no yield.
	for range idx.RangeEntries([][]byte{{1}, {2}, {3}}, nil) {
		t.Fatal("over-wide start bound yielded")
	}
	if !errors.Is(idx.Err(), ErrInvalidOptions) {
		t.Fatalf("Err = %v, want ErrInvalidOptions", idx.Err())
	}
}

func TestIndexRangeEntriesSetKeyspaceRejected(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 128})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tx2, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx2.Rollback()
	decl := &IndexDecl{
		Name:    "ix",
		Columns: []IndexColumn{{Name: "a"}},
		Extract: func(k, v []byte) []IndexEntry {
			return []IndexEntry{{Cols: [][]byte{v}}}
		},
	}
	s, err := tx2.CreateSetKeyspace("s", nil, decl)
	if err != nil {
		t.Fatalf("CreateSetKeyspace: %v", err)
	}
	if _, err := s.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	idx, err := s.Index("ix")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	for range idx.RangeEntries(nil, nil) {
		t.Fatal("SetKeyspace RangeEntries yielded")
	}
	if !errors.Is(idx.Err(), ErrInvalidOptions) {
		t.Fatalf("Err = %v, want ErrInvalidOptions", idx.Err())
	}
}

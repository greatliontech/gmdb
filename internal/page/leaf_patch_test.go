package page

import (
	"bytes"
	"testing"
)

// PatchRefs must rewrite exactly the overflow/nested page-reference
// u64s and nothing else: every other byte of the page — keys, values,
// TotalLen/NestedCount, group structure, tables — stays identical.
func TestLeafPatchRefs_RewritesOnlyRefs(t *testing.T) {
	nestedFlags := CellFlagMultiValue | CellFlagNestedTree
	subpage, err := EncodeSubpage([][]byte{[]byte("m1"), []byte("m2")}, 0)
	if err != nil {
		t.Fatalf("EncodeSubpage: %v", err)
	}
	// Eight entries so RGT=3 places ref-bearing cells at restart AND
	// delta positions in BOTH trailer forms (overflow at idx 1 delta /
	// idx 4 restart? — group layout is builder-owned; the visited-set
	// assertion below pins the exact ref cells either way), plus a
	// subpage cell (MultiValue without NestedTree) pinning the
	// no-trailer skip guard.
	entries := []LeafEntry{
		{Key: []byte("shared-aaa"), Value: []byte("v1")},
		{Flags: CellFlagOverflow, Key: []byte("shared-bbb"), OverflowPage: 100, TotalLen: 9999},
		{Flags: CellFlagMultiValue, Key: []byte("shared-bzz"), Value: subpage},
		{Flags: nestedFlags, Key: []byte("shared-ddd"), NestedRoot: 200, NestedCount: 7},
		{Flags: CellFlagOverflow, Key: []byte("shared-eee"), OverflowPage: 300, TotalLen: 12345},
		{Key: []byte("shared-fff"), Value: []byte("v6")},
		{Flags: nestedFlags, Key: []byte("shared-ggg"), NestedRoot: 400, NestedCount: 11},
		{Key: []byte("shared-hhh"), Value: []byte("v8")},
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		// RGT=3 puts refs at restart AND delta positions.
		{"compressed", Config{PageSize: 4096, RestartGroupTarget: 3, PageChecksum: false}},
		{"uncompressed", Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, tc.cfg.PageSize)
			b := NewLeafBuilder(buf, tc.cfg)
			for _, e := range entries {
				if !b.AddEntry(e) {
					t.Fatalf("AddEntry(%q) full", e.Key)
				}
			}
			b.Finish()
			orig := bytes.Clone(buf)

			r := NewLeafReader(buf, tc.cfg)
			if err := r.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			var visited []int
			r.PatchRefs(func(idx int, e LeafEntry) uint64 {
				visited = append(visited, idx)
				if !bytes.Equal(e.Key, entries[idx].Key) {
					t.Fatalf("PatchRefs idx=%d key=%q want %q", idx, e.Key, entries[idx].Key)
				}
				if e.IsOverflow() {
					if e.OverflowPage != entries[idx].OverflowPage || e.TotalLen != entries[idx].TotalLen {
						t.Fatalf("idx=%d decoded ovfl %d/%d want %d/%d", idx, e.OverflowPage, e.TotalLen, entries[idx].OverflowPage, entries[idx].TotalLen)
					}
					return e.OverflowPage + 1000
				}
				if e.NestedRoot != entries[idx].NestedRoot || e.NestedCount != entries[idx].NestedCount {
					t.Fatalf("idx=%d decoded nested %d/%d want %d/%d", idx, e.NestedRoot, e.NestedCount, entries[idx].NestedRoot, entries[idx].NestedCount)
				}
				return e.NestedRoot + 1000
			})
			if want := []int{1, 3, 4, 6}; len(visited) != len(want) || visited[0] != 1 || visited[1] != 3 || visited[2] != 4 || visited[3] != 6 {
				t.Fatalf("visited=%v want %v", visited, want)
			}

			// Re-decode: refs moved by +1000, second u64 and all keys/values intact.
			r2 := NewLeafReader(buf, tc.cfg)
			if err := r2.Validate(); err != nil {
				t.Fatalf("post-patch Validate: %v", err)
			}
			it := r2.IterForReuse(nil, nil, nil)
			i := 0
			for e, ok := it.Next(); ok; e, ok = it.Next() {
				want := entries[i]
				if !bytes.Equal(e.Key, want.Key) {
					t.Fatalf("entry %d key=%q want %q", i, e.Key, want.Key)
				}
				switch {
				case want.IsOverflow():
					if e.OverflowPage != want.OverflowPage+1000 || e.TotalLen != want.TotalLen {
						t.Fatalf("entry %d ovfl=%d/%d want %d/%d", i, e.OverflowPage, e.TotalLen, want.OverflowPage+1000, want.TotalLen)
					}
				case want.IsNestedTree():
					if e.NestedRoot != want.NestedRoot+1000 || e.NestedCount != want.NestedCount {
						t.Fatalf("entry %d nested=%d/%d want %d/%d", i, e.NestedRoot, e.NestedCount, want.NestedRoot+1000, want.NestedCount)
					}
				default:
					if !bytes.Equal(e.Value, want.Value) {
						t.Fatalf("entry %d value=%q want %q", i, e.Value, want.Value)
					}
				}
				i++
			}
			if i != len(entries) {
				t.Fatalf("iterated %d entries want %d", i, len(entries))
			}

			// Byte-level: exactly 4 u64s differ (16 changed bytes at
			// most per ref is 8; 4 refs → 32 bytes), everything else
			// identical.
			diff := 0
			for j := range buf {
				if buf[j] != orig[j] {
					diff++
				}
			}
			if diff == 0 || diff > 4*8 {
				t.Fatalf("changed bytes = %d, want in (0, 32]", diff)
			}
		})
	}
}

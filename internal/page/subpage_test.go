package page

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// --- EncodeSubpage round-trip ---

func TestEncodeSubpageEmpty(t *testing.T) {
	for _, fvs := range []uint16{0, 1, 8, 64} {
		t.Run(fmt.Sprintf("fvs=%d", fvs), func(t *testing.T) {
			buf, err := EncodeSubpage(nil, fvs)
			if err != nil {
				t.Fatalf("EncodeSubpage(nil): %v", err)
			}
			if len(buf) != SubpageHeaderSize {
				t.Fatalf("empty subpage len=%d, want %d", len(buf), SubpageHeaderSize)
			}
			r := NewSubpageReader(buf, fvs)
			if r.Count() != 0 || r.DataSize() != 0 {
				t.Fatalf("empty subpage Count=%d DataSize=%d, want 0/0", r.Count(), r.DataSize())
			}
			if err := r.Validate(); err != nil {
				t.Fatalf("empty subpage Validate: %v", err)
			}
		})
	}
}

func TestEncodeSubpageVariableRoundTrip(t *testing.T) {
	values := [][]byte{
		[]byte("alpha"),
		[]byte("beta"),
		[]byte("delta"),
		[]byte("gamma"),
	}
	buf, err := EncodeSubpage(values, 0)
	if err != nil {
		t.Fatalf("EncodeSubpage: %v", err)
	}
	r := NewSubpageReader(buf, 0)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r.Count() != len(values) {
		t.Fatalf("Count=%d, want %d", r.Count(), len(values))
	}
	for i, want := range values {
		got := r.ValueAt(i)
		if !bytes.Equal(got, want) {
			t.Errorf("ValueAt(%d)=%q, want %q", i, got, want)
		}
	}
	// AllValues yields in order.
	var seen [][]byte
	r.AllValues(func(v []byte) bool {
		dup := append([]byte(nil), v...)
		seen = append(seen, dup)
		return true
	})
	if len(seen) != len(values) {
		t.Fatalf("AllValues yielded %d, want %d", len(seen), len(values))
	}
	for i, want := range values {
		if !bytes.Equal(seen[i], want) {
			t.Errorf("AllValues[%d]=%q, want %q", i, seen[i], want)
		}
	}
}

func TestEncodeSubpageFixedRoundTrip(t *testing.T) {
	const fvs uint16 = 4
	values := [][]byte{
		{0, 0, 0, 1},
		{0, 0, 0, 2},
		{0, 0, 0, 3},
		{0, 0, 1, 0},
		{1, 0, 0, 0},
	}
	buf, err := EncodeSubpage(values, fvs)
	if err != nil {
		t.Fatalf("EncodeSubpage: %v", err)
	}
	r := NewSubpageReader(buf, fvs)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r.DataSize() != len(values)*int(fvs) {
		t.Fatalf("DataSize=%d, want %d", r.DataSize(), len(values)*int(fvs))
	}
	for i, want := range values {
		got := r.ValueAt(i)
		if !bytes.Equal(got, want) {
			t.Errorf("ValueAt(%d)=%x, want %x", i, got, want)
		}
	}
}

func TestEncodeSubpageZeroLengthValuesAllowed(t *testing.T) {
	// Variable-size: a single zero-length value is a valid entry.
	values := [][]byte{{}}
	buf, err := EncodeSubpage(values, 0)
	if err != nil {
		t.Fatalf("EncodeSubpage: %v", err)
	}
	r := NewSubpageReader(buf, 0)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r.Count() != 1 {
		t.Fatalf("Count=%d, want 1", r.Count())
	}
	if v := r.ValueAt(0); len(v) != 0 {
		t.Fatalf("ValueAt(0) len=%d, want 0", len(v))
	}
}

func TestEncodeSubpageRejectsOutOfOrder(t *testing.T) {
	values := [][]byte{[]byte("beta"), []byte("alpha")}
	_, err := EncodeSubpage(values, 0)
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("err=%v, want ErrSubpageCorrupted", err)
	}
}

func TestEncodeSubpageRejectsDuplicate(t *testing.T) {
	values := [][]byte{[]byte("alpha"), []byte("alpha")}
	_, err := EncodeSubpage(values, 0)
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("err=%v, want ErrSubpageCorrupted", err)
	}
}

func TestEncodeSubpageRejectsFixedSizeMismatch(t *testing.T) {
	values := [][]byte{{0, 0, 0, 1}, {0, 0, 1}}
	_, err := EncodeSubpage(values, 4)
	if !errors.Is(err, ErrSubpageValueSize) {
		t.Fatalf("err=%v, want ErrSubpageValueSize", err)
	}
}

// --- Search ---

func TestSearchVariableSizeHits(t *testing.T) {
	values := [][]byte{[]byte("alpha"), []byte("beta"), []byte("gamma")}
	buf, err := EncodeSubpage(values, 0)
	if err != nil {
		t.Fatalf("EncodeSubpage: %v", err)
	}
	r := NewSubpageReader(buf, 0)
	for i, v := range values {
		idx, found := r.Search(v)
		if !found || idx != i {
			t.Errorf("Search(%q)=(%d,%v), want (%d,true)", v, idx, found, i)
		}
	}
}

func TestSearchVariableSizeMissReturnsInsertionPoint(t *testing.T) {
	values := [][]byte{[]byte("alpha"), []byte("gamma"), []byte("kappa")}
	buf, _ := EncodeSubpage(values, 0)
	r := NewSubpageReader(buf, 0)
	cases := []struct {
		target  string
		wantIdx int
	}{
		{"aaa", 0},     // before all
		{"beta", 1},    // between alpha and gamma
		{"foo", 1},     // between alpha and gamma (alphabetically)
		{"hotel", 2},   // between gamma and kappa
		{"zulu", 3},    // after all
	}
	for _, tc := range cases {
		idx, found := r.Search([]byte(tc.target))
		if found {
			t.Errorf("Search(%q)=(%d,true), want (%d,false)", tc.target, idx, tc.wantIdx)
			continue
		}
		if idx != tc.wantIdx {
			t.Errorf("Search(%q)=(%d,false), want (%d,false)", tc.target, idx, tc.wantIdx)
		}
	}
}

func TestSearchFixedSizeBinarySearch(t *testing.T) {
	const fvs uint16 = 2
	values := [][]byte{
		{0, 1}, {0, 5}, {1, 0}, {1, 3}, {2, 9}, {3, 0}, {7, 7},
	}
	buf, _ := EncodeSubpage(values, fvs)
	r := NewSubpageReader(buf, fvs)
	for i, v := range values {
		idx, found := r.Search(v)
		if !found || idx != i {
			t.Errorf("Search(%x)=(%d,%v), want (%d,true)", v, idx, found, i)
		}
	}
	// Miss: target between (1,3) and (2,9) → insertion at 4.
	idx, found := r.Search([]byte{2, 0})
	if found || idx != 4 {
		t.Errorf("Search(0x0200)=(%d,%v), want (4,false)", idx, found)
	}
	// Miss before all.
	idx, found = r.Search([]byte{0, 0})
	if found || idx != 0 {
		t.Errorf("Search(0x0000)=(%d,%v), want (0,false)", idx, found)
	}
	// Miss after all.
	idx, found = r.Search([]byte{9, 9})
	if found || idx != len(values) {
		t.Errorf("Search(0x0909)=(%d,%v), want (%d,false)", idx, found, len(values))
	}
}

func TestSearchEmptySubpage(t *testing.T) {
	buf, _ := EncodeSubpage(nil, 0)
	r := NewSubpageReader(buf, 0)
	idx, found := r.Search([]byte("anything"))
	if found || idx != 0 {
		t.Errorf("Search on empty=(%d,%v), want (0,false)", idx, found)
	}
}

func TestSearchFixedSizeWrongLengthTargetNoMatch(t *testing.T) {
	// Wrong-length targets are not pre-rejected by Search — they
	// just necessarily mismatch every stored entry (the SetKeyspace
	// surface enforces length at a higher layer).
	const fvs uint16 = 4
	values := [][]byte{{0, 0, 0, 1}, {0, 0, 0, 2}}
	buf, _ := EncodeSubpage(values, fvs)
	r := NewSubpageReader(buf, fvs)
	// 3-byte target — bytes.Compare against 4-byte entries always
	// mismatches; the insertion point is well-defined.
	_, found := r.Search([]byte{0, 0, 0})
	if found {
		t.Errorf("Search(short)=found, want not-found")
	}
}

// --- Insert ---

func TestInsertVariableSizeIntoEmpty(t *testing.T) {
	buf, _ := EncodeSubpage(nil, 0)
	r := NewSubpageReader(buf, 0)
	newBuf, added, err := r.Insert([]byte("alpha"))
	if err != nil || !added {
		t.Fatalf("Insert: added=%v err=%v, want true/nil", added, err)
	}
	r2 := NewSubpageReader(newBuf, 0)
	if err := r2.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r2.Count() != 1 || !bytes.Equal(r2.ValueAt(0), []byte("alpha")) {
		t.Fatalf("post-insert: Count=%d ValueAt(0)=%q", r2.Count(), r2.ValueAt(0))
	}
}

func TestInsertVariableSizeMaintainsSorted(t *testing.T) {
	values := [][]byte{[]byte("beta"), []byte("delta")}
	buf, _ := EncodeSubpage(values, 0)
	r := NewSubpageReader(buf, 0)
	// Insert "alpha" at head, "charlie" in middle, "epsilon" at end.
	for _, insert := range []string{"alpha", "charlie", "epsilon"} {
		newBuf, added, err := r.Insert([]byte(insert))
		if err != nil || !added {
			t.Fatalf("Insert(%q): added=%v err=%v", insert, added, err)
		}
		r = NewSubpageReader(newBuf, 0)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := []string{"alpha", "beta", "charlie", "delta", "epsilon"}
	if r.Count() != len(want) {
		t.Fatalf("Count=%d, want %d", r.Count(), len(want))
	}
	for i, w := range want {
		if got := r.ValueAt(i); !bytes.Equal(got, []byte(w)) {
			t.Errorf("ValueAt(%d)=%q, want %q", i, got, w)
		}
	}
}

func TestInsertDuplicateIsNoop(t *testing.T) {
	values := [][]byte{[]byte("alpha"), []byte("beta")}
	buf, _ := EncodeSubpage(values, 0)
	r := NewSubpageReader(buf, 0)
	newBuf, added, err := r.Insert([]byte("alpha"))
	if err != nil {
		t.Fatalf("Insert(dup): err=%v", err)
	}
	if added {
		t.Errorf("Insert(dup): added=true, want false")
	}
	// Contract: no-op duplicate returns content-identical bytes (the
	// original subpage view). Test by content equality rather than
	// pointer identity so a future refactor that returns a defensive
	// copy with the same content stays green — what callers care
	// about is "the no-op produces the pre-insert subpage."
	if !bytes.Equal(newBuf, buf[:r.SizeBytes()]) {
		t.Errorf("Insert(dup) bytes diverged from original; want identical content")
	}
}

func TestInsertFixedSizeRejectsWrongLength(t *testing.T) {
	const fvs uint16 = 4
	buf, _ := EncodeSubpage(nil, fvs)
	r := NewSubpageReader(buf, fvs)
	_, _, err := r.Insert([]byte{1, 2, 3})
	if !errors.Is(err, ErrSubpageValueSize) {
		t.Fatalf("Insert(short): err=%v, want ErrSubpageValueSize", err)
	}
	_, _, err = r.Insert([]byte{1, 2, 3, 4, 5})
	if !errors.Is(err, ErrSubpageValueSize) {
		t.Fatalf("Insert(long): err=%v, want ErrSubpageValueSize", err)
	}
}

func TestInsertFixedSizeSequence(t *testing.T) {
	const fvs uint16 = 2
	buf, _ := EncodeSubpage(nil, fvs)
	r := NewSubpageReader(buf, fvs)
	inserts := [][]byte{{0, 5}, {0, 1}, {1, 0}, {0, 3}, {2, 9}}
	for _, v := range inserts {
		newBuf, added, err := r.Insert(v)
		if err != nil || !added {
			t.Fatalf("Insert(%x): added=%v err=%v", v, added, err)
		}
		r = NewSubpageReader(newBuf, fvs)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	want := [][]byte{{0, 1}, {0, 3}, {0, 5}, {1, 0}, {2, 9}}
	for i, w := range want {
		if got := r.ValueAt(i); !bytes.Equal(got, w) {
			t.Errorf("ValueAt(%d)=%x, want %x", i, got, w)
		}
	}
}

// --- Delete ---

func TestDeleteVariableSize(t *testing.T) {
	values := [][]byte{[]byte("alpha"), []byte("beta"), []byte("delta"), []byte("gamma")}
	buf, _ := EncodeSubpage(values, 0)
	r := NewSubpageReader(buf, 0)
	newBuf, deleted, err := r.Delete([]byte("beta"))
	if err != nil || !deleted {
		t.Fatalf("Delete(beta): deleted=%v err=%v", deleted, err)
	}
	r2 := NewSubpageReader(newBuf, 0)
	if err := r2.Validate(); err != nil {
		t.Fatalf("Validate post-delete: %v", err)
	}
	want := []string{"alpha", "delta", "gamma"}
	if r2.Count() != len(want) {
		t.Fatalf("Count=%d, want %d", r2.Count(), len(want))
	}
	for i, w := range want {
		if got := r2.ValueAt(i); !bytes.Equal(got, []byte(w)) {
			t.Errorf("ValueAt(%d)=%q, want %q", i, got, w)
		}
	}
}

func TestDeleteAbsentIsNoop(t *testing.T) {
	values := [][]byte{[]byte("alpha"), []byte("beta")}
	buf, _ := EncodeSubpage(values, 0)
	r := NewSubpageReader(buf, 0)
	newBuf, deleted, err := r.Delete([]byte("zulu"))
	if err != nil {
		t.Fatalf("Delete(absent): err=%v", err)
	}
	if deleted {
		t.Errorf("Delete(absent): deleted=true, want false")
	}
	// Contract symmetric to Insert(dup): no-op returns content-identical
	// bytes. Test by content equality rather than pointer identity so a
	// future refactor that returns a defensive copy on the no-op path
	// stays green.
	if !bytes.Equal(newBuf, buf[:r.SizeBytes()]) {
		t.Errorf("Delete(absent) bytes diverged from original; want identical content")
	}
}

func TestDeleteToEmpty(t *testing.T) {
	values := [][]byte{[]byte("only")}
	buf, _ := EncodeSubpage(values, 0)
	r := NewSubpageReader(buf, 0)
	newBuf, deleted, err := r.Delete([]byte("only"))
	if err != nil || !deleted {
		t.Fatalf("Delete: deleted=%v err=%v", deleted, err)
	}
	r2 := NewSubpageReader(newBuf, 0)
	if err := r2.Validate(); err != nil {
		t.Fatalf("Validate empty: %v", err)
	}
	if r2.Count() != 0 || r2.DataSize() != 0 {
		t.Fatalf("post-delete-to-empty Count=%d DataSize=%d, want 0/0", r2.Count(), r2.DataSize())
	}
	if len(newBuf) != SubpageHeaderSize {
		t.Fatalf("post-delete-to-empty buf len=%d, want %d", len(newBuf), SubpageHeaderSize)
	}
}

func TestDeleteFixedSizeRejectsWrongLength(t *testing.T) {
	const fvs uint16 = 4
	values := [][]byte{{0, 0, 0, 1}, {0, 0, 0, 2}}
	buf, _ := EncodeSubpage(values, fvs)
	r := NewSubpageReader(buf, fvs)
	_, _, err := r.Delete([]byte{0, 0, 1})
	if !errors.Is(err, ErrSubpageValueSize) {
		t.Fatalf("Delete(short): err=%v, want ErrSubpageValueSize", err)
	}
}

// --- Validate ---

func TestValidateRejectsCountDataSizeMismatch(t *testing.T) {
	// Construct a Count=2 / DataSize=0 subpage manually.
	buf := make([]byte, SubpageHeaderSize)
	le.PutUint16(buf[subOffCount:], 2)
	le.PutUint16(buf[subOffDataSize:], 0)
	r := NewSubpageReader(buf, 0)
	err := r.Validate()
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("Validate=%v, want ErrSubpageCorrupted", err)
	}
}

func TestValidateRejectsCount0WithNonZeroDataSize(t *testing.T) {
	buf := make([]byte, SubpageHeaderSize+8)
	le.PutUint16(buf[subOffCount:], 0)
	le.PutUint16(buf[subOffDataSize:], 8)
	r := NewSubpageReader(buf, 0)
	err := r.Validate()
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("Validate=%v, want ErrSubpageCorrupted", err)
	}
}

func TestValidateRejectsFixedSizeStrideMismatch(t *testing.T) {
	// Count=3, DataSize=8, fvs=4 — should be Count*fvs=12 but we say 8.
	buf := make([]byte, SubpageHeaderSize+8)
	le.PutUint16(buf[subOffCount:], 3)
	le.PutUint16(buf[subOffDataSize:], 8)
	r := NewSubpageReader(buf, 4)
	err := r.Validate()
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("Validate=%v, want ErrSubpageCorrupted", err)
	}
}

func TestValidateRejectsOutOfOrderVariable(t *testing.T) {
	// Hand-craft a Count=2 variable-size subpage with entries in
	// reverse order.
	buf := make([]byte, SubpageHeaderSize+2+1+2+1)
	le.PutUint16(buf[subOffCount:], 2)
	le.PutUint16(buf[subOffDataSize:], 6)
	off := SubpageHeaderSize
	le.PutUint16(buf[off:], 1)
	buf[off+2] = 'b'
	off += 3
	le.PutUint16(buf[off:], 1)
	buf[off+2] = 'a'
	r := NewSubpageReader(buf, 0)
	err := r.Validate()
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("Validate=%v, want ErrSubpageCorrupted", err)
	}
}

func TestValidateRejectsValueLenOverrun(t *testing.T) {
	// Variable-size: DataSize=4 but the lone entry declares ValueLen=10.
	buf := make([]byte, SubpageHeaderSize+4)
	le.PutUint16(buf[subOffCount:], 1)
	le.PutUint16(buf[subOffDataSize:], 4)
	off := SubpageHeaderSize
	le.PutUint16(buf[off:], 10) // bogus ValueLen
	// Filler bytes — Validate should catch the ValueLen overrun.
	r := NewSubpageReader(buf, 0)
	err := r.Validate()
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("Validate=%v, want ErrSubpageCorrupted", err)
	}
}

func TestValidateRejectsDuplicateFixed(t *testing.T) {
	buf := make([]byte, SubpageHeaderSize+8)
	le.PutUint16(buf[subOffCount:], 2)
	le.PutUint16(buf[subOffDataSize:], 8)
	off := SubpageHeaderSize
	copy(buf[off:], []byte{0, 0, 0, 1})
	copy(buf[off+4:], []byte{0, 0, 0, 1}) // duplicate
	r := NewSubpageReader(buf, 4)
	err := r.Validate()
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("Validate=%v, want ErrSubpageCorrupted", err)
	}
}

// --- Edge: ValueAt, AllValues ---

func TestValueAtPanicsOutOfRange(t *testing.T) {
	buf, _ := EncodeSubpage([][]byte{[]byte("a")}, 0)
	r := NewSubpageReader(buf, 0)
	defer func() {
		if recover() == nil {
			t.Errorf("ValueAt(1) on Count=1 did not panic")
		}
	}()
	r.ValueAt(1)
}

func TestAllValuesEarlyTermination(t *testing.T) {
	values := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")}
	buf, _ := EncodeSubpage(values, 0)
	r := NewSubpageReader(buf, 0)
	var seen int
	r.AllValues(func(v []byte) bool {
		seen++
		return seen < 2 // stop after 2nd
	})
	if seen != 2 {
		t.Errorf("AllValues iterated %d, want 2", seen)
	}
}

// --- Insert/Delete sequence stress ---

func TestInsertDeleteSequenceMaintainsInvariants(t *testing.T) {
	buf, _ := EncodeSubpage(nil, 0)
	r := NewSubpageReader(buf, 0)
	// Insert in scrambled order.
	inserts := []string{"mango", "apple", "kiwi", "banana", "fig", "date", "cherry"}
	for _, v := range inserts {
		nb, added, err := r.Insert([]byte(v))
		if err != nil || !added {
			t.Fatalf("Insert(%q): added=%v err=%v", v, added, err)
		}
		r = NewSubpageReader(nb, 0)
		if err := r.Validate(); err != nil {
			t.Fatalf("Validate after insert %q: %v", v, err)
		}
	}
	if r.Count() != len(inserts) {
		t.Fatalf("Count=%d, want %d", r.Count(), len(inserts))
	}
	// Verify sorted-order Inv-2.
	for i := 1; i < r.Count(); i++ {
		prev := r.ValueAt(i - 1)
		cur := r.ValueAt(i)
		if bytes.Compare(prev, cur) >= 0 {
			t.Errorf("not sorted: ValueAt(%d)=%q >= ValueAt(%d)=%q", i-1, prev, i, cur)
		}
	}
	// Delete in different order.
	deletes := []string{"kiwi", "apple", "mango", "cherry", "fig", "date", "banana"}
	for _, v := range deletes {
		nb, deleted, err := r.Delete([]byte(v))
		if err != nil || !deleted {
			t.Fatalf("Delete(%q): deleted=%v err=%v", v, deleted, err)
		}
		r = NewSubpageReader(nb, 0)
		if err := r.Validate(); err != nil {
			t.Fatalf("Validate after delete %q: %v", v, err)
		}
	}
	if r.Count() != 0 || r.DataSize() != 0 {
		t.Fatalf("post-all-deletes Count=%d DataSize=%d, want 0/0", r.Count(), r.DataSize())
	}
}

// --- Bounded growth: insert near DataSize uint16 cap ---

func TestEncodeRejectsDataSizeOverflow(t *testing.T) {
	// 200 entries of 350 bytes each = 70000 bytes data — exceeds uint16 cap (65535).
	values := make([][]byte, 0, 200)
	for i := range 200 {
		v := make([]byte, 350)
		// Distinct prefix per entry so the sorted-order check passes.
		v[0] = byte(i / 256)
		v[1] = byte(i % 256)
		values = append(values, v)
	}
	_, err := EncodeSubpage(values, 0)
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("EncodeSubpage huge: err=%v, want ErrSubpageCorrupted (DataSize overflow)", err)
	}
}

// --- Insert ordering does not aliase the original buf ---

func TestInsertProducesIndependentBuffer(t *testing.T) {
	values := [][]byte{[]byte("alpha"), []byte("gamma")}
	buf, _ := EncodeSubpage(values, 0)
	r := NewSubpageReader(buf, 0)
	newBuf, added, err := r.Insert([]byte("beta"))
	if err != nil || !added {
		t.Fatalf("Insert: added=%v err=%v", added, err)
	}
	// Mutate buf — newBuf must be unaffected.
	for i := range buf {
		buf[i] = 0xFF
	}
	r2 := NewSubpageReader(newBuf, 0)
	if err := r2.Validate(); err != nil {
		t.Fatalf("Validate post-buf-mutation: %v", err)
	}
	got := []string{}
	r2.AllValues(func(v []byte) bool {
		got = append(got, string(v))
		return true
	})
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestDeleteProducesIndependentBuffer(t *testing.T) {
	values := [][]byte{[]byte("alpha"), []byte("beta"), []byte("gamma")}
	buf, _ := EncodeSubpage(values, 0)
	r := NewSubpageReader(buf, 0)
	newBuf, deleted, err := r.Delete([]byte("beta"))
	if err != nil || !deleted {
		t.Fatalf("Delete: deleted=%v err=%v", deleted, err)
	}
	// Mutate buf — newBuf must be unaffected.
	for i := range buf {
		buf[i] = 0xFF
	}
	r2 := NewSubpageReader(newBuf, 0)
	if err := r2.Validate(); err != nil {
		t.Fatalf("Validate post-buf-mutation: %v", err)
	}
	got := []string{}
	r2.AllValues(func(v []byte) bool {
		got = append(got, string(v))
		return true
	})
	want := []string{"alpha", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

// Regression test for M-1 (introduced overflow guard on Insert).
// A misbehaving caller — one that has bypassed the 6.4 50%-of-leaf
// promotion-threshold check, or has constructed a hostile
// SubpageReader directly — must not be able to push Count or
// DataSize past the uint16 cap; the codec's contract requires
// ErrSubpageCorrupted on overflow rather than silent truncation
// of the encoded header.
func TestInsertRejectsDataSizeOverflow(t *testing.T) {
	// Build a subpage whose existing DataSize is near the uint16
	// cap (variable-size). Adding one more entry should overflow.
	const big = 65000 // body bytes
	v0 := make([]byte, big)
	for i := range v0 {
		v0[i] = 'a'
	}
	buf, err := EncodeSubpage([][]byte{v0}, 0)
	if err != nil {
		t.Fatalf("seed EncodeSubpage: %v", err)
	}
	r := NewSubpageReader(buf, 0)
	// Sanity: seed subpage is valid.
	if err := r.Validate(); err != nil {
		t.Fatalf("seed Validate: %v", err)
	}
	// Insert a value that would push DataSize past MaxSubpageDataSize.
	v1 := make([]byte, 1000) // 1000 > MaxSubpageDataSize - 65000 - 2
	for i := range v1 {
		v1[i] = 'b' // sorts after v0 (all 'a'), so insertion is valid logically
	}
	_, added, err := r.Insert(v1)
	if !errors.Is(err, ErrSubpageCorrupted) {
		t.Fatalf("Insert(overflow): err=%v added=%v, want ErrSubpageCorrupted", err, added)
	}
	if added {
		t.Errorf("Insert(overflow): added=true, want false")
	}
}

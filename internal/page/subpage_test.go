package page

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestSubpageVariableRoundTrip(t *testing.T) {
	values := [][]byte{
		[]byte("alpha"),
		[]byte("beta"),
		[]byte("gamma"),
		[]byte("delta"),
	}
	// Sort for correctness.
	// Already sorted lexicographically.

	size := SubpageSize(values, 0)
	buf := make([]byte, size)
	b := NewSubpageBuilder(buf, 0)
	for _, v := range values {
		if !b.AddValue(v) {
			t.Fatalf("AddValue(%q) failed", v)
		}
	}
	total := b.Finish()
	if total != size {
		t.Fatalf("Finish() = %d, want %d", total, size)
	}

	r := NewSubpageReader(buf[:total], 0)
	if r.Count() != len(values) {
		t.Fatalf("Count() = %d, want %d", r.Count(), len(values))
	}
	for i, want := range values {
		got := r.Value(i)
		if !bytes.Equal(got, want) {
			t.Errorf("Value(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestSubpageFixedRoundTrip(t *testing.T) {
	fixedSize := 8
	values := make([][]byte, 5)
	for i := range values {
		v := make([]byte, fixedSize)
		binary.BigEndian.PutUint64(v, uint64((i+1)*100))
		values[i] = v
	}

	size := SubpageSize(values, fixedSize)
	buf := make([]byte, size)
	b := NewSubpageBuilder(buf, fixedSize)
	for _, v := range values {
		if !b.AddValue(v) {
			t.Fatalf("AddValue failed")
		}
	}
	total := b.Finish()

	r := NewSubpageReader(buf[:total], fixedSize)
	if r.Count() != len(values) {
		t.Fatalf("Count() = %d, want %d", r.Count(), len(values))
	}
	for i, want := range values {
		got := r.Value(i)
		if !bytes.Equal(got, want) {
			t.Errorf("Value(%d) mismatch", i)
		}
	}
}

func TestSubpageSearchVariable(t *testing.T) {
	values := [][]byte{
		[]byte("apple"),
		[]byte("banana"),
		[]byte("cherry"),
		[]byte("date"),
		[]byte("elderberry"),
	}

	size := SubpageSize(values, 0)
	buf := make([]byte, size)
	b := NewSubpageBuilder(buf, 0)
	for _, v := range values {
		b.AddValue(v)
	}
	b.Finish()

	r := NewSubpageReader(buf[:size], 0)

	// Exact matches.
	for i, v := range values {
		idx, found := r.Search(v)
		if !found {
			t.Errorf("Search(%q): not found", v)
		}
		if idx != i {
			t.Errorf("Search(%q): index = %d, want %d", v, idx, i)
		}
	}

	// Not found — insertion point (coconut > cherry, so insertion at 3).
	idx, found := r.Search([]byte("coconut"))
	if found {
		t.Error("Search(coconut): unexpectedly found")
	}
	if idx != 3 {
		t.Errorf("Search(coconut): index = %d, want 3", idx)
	}

	// Before first.
	idx, found = r.Search([]byte("aaa"))
	if found {
		t.Error("Search(aaa): unexpectedly found")
	}
	if idx != 0 {
		t.Errorf("Search(aaa): index = %d, want 0", idx)
	}

	// After last.
	idx, found = r.Search([]byte("zebra"))
	if found {
		t.Error("Search(zebra): unexpectedly found")
	}
	if idx != 5 {
		t.Errorf("Search(zebra): index = %d, want 5", idx)
	}
}

func TestSubpageSearchFixed(t *testing.T) {
	fixedSize := 4
	values := make([][]byte, 10)
	for i := range values {
		v := make([]byte, fixedSize)
		binary.BigEndian.PutUint32(v, uint32((i+1)*10))
		values[i] = v
	}

	size := SubpageSize(values, fixedSize)
	buf := make([]byte, size)
	b := NewSubpageBuilder(buf, fixedSize)
	for _, v := range values {
		b.AddValue(v)
	}
	b.Finish()

	r := NewSubpageReader(buf[:size], fixedSize)

	for i, v := range values {
		idx, found := r.Search(v)
		if !found || idx != i {
			t.Errorf("Search(value[%d]): idx=%d found=%v, want %d true", i, idx, found, i)
		}
	}

	// Not found.
	target := make([]byte, fixedSize)
	binary.BigEndian.PutUint32(target, 15) // between 10 and 20
	idx, found := r.Search(target)
	if found {
		t.Error("Search(15): unexpectedly found")
	}
	if idx != 1 {
		t.Errorf("Search(15): index = %d, want 1", idx)
	}
}

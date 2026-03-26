package page

import (
	"bytes"
	"fmt"
	"testing"
)

func TestBranchRoundTrip(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewBranchBuilder(buf, cfg)
	b.SetPtr0(100)

	keys := [][]byte{
		[]byte("delta"),
		[]byte("hotel"),
		[]byte("lima"),
		[]byte("papa"),
		[]byte("tango"),
	}
	childPtrs := []uint64{200, 300, 400, 500, 600}

	for i, key := range keys {
		if !b.AddCell(key, childPtrs[i]) {
			t.Fatalf("AddCell(%q) failed", key)
		}
	}
	count := b.Finish()
	if count != uint16(len(keys)) {
		t.Fatalf("Finish() = %d, want %d", count, len(keys))
	}

	// Verify header.
	typ, flags, hcount, additional := ReadHeader(buf)
	if typ != TypeBranch {
		t.Errorf("Type = %d, want %d", typ, TypeBranch)
	}
	if flags != 0 {
		t.Errorf("Flags = %d, want 0", flags)
	}
	if hcount != uint16(len(keys)) {
		t.Errorf("Count = %d, want %d", hcount, len(keys))
	}
	if additional != 0 {
		t.Errorf("AdditionalPages = %d, want 0", additional)
	}

	// Read back.
	r := NewBranchReader(buf)
	if r.Count() != len(keys) {
		t.Fatalf("Count() = %d, want %d", r.Count(), len(keys))
	}
	if r.Ptr0() != 100 {
		t.Errorf("Ptr0() = %d, want 100", r.Ptr0())
	}
	for i, want := range keys {
		got := r.Key(i)
		if !bytes.Equal(got, want) {
			t.Errorf("Key(%d) = %q, want %q", i, got, want)
		}
		if r.ChildPtr(i) != childPtrs[i] {
			t.Errorf("ChildPtr(%d) = %d, want %d", i, r.ChildPtr(i), childPtrs[i])
		}
	}
}

func TestBranchSearch(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewBranchBuilder(buf, cfg)
	b.SetPtr0(10)
	// Separators: "f", "k", "p", "u"
	// Children:   10  | 20 | 30 | 40 | 50
	b.AddCell([]byte("f"), 20)
	b.AddCell([]byte("k"), 30)
	b.AddCell([]byte("p"), 40)
	b.AddCell([]byte("u"), 50)
	b.Finish()

	r := NewBranchReader(buf)

	tests := []struct {
		target    string
		wantChild uint64
		wantIdx   int
	}{
		{"a", 10, -1},  // before "f" → Ptr0
		{"e", 10, -1},  // before "f" → Ptr0
		{"f", 20, 0},   // == "f" → right child of "f"
		{"g", 20, 0},   // between "f" and "k"
		{"k", 30, 1},   // == "k" → right child of "k"
		{"m", 30, 1},   // between "k" and "p"
		{"p", 40, 2},   // == "p" → right child of "p"
		{"u", 50, 3},   // == "u" → right child of "u"
		{"z", 50, 3},   // after all → rightmost child
	}
	for _, tt := range tests {
		child, idx := r.Search([]byte(tt.target))
		if child != tt.wantChild || idx != tt.wantIdx {
			t.Errorf("Search(%q) = (%d, %d), want (%d, %d)",
				tt.target, child, idx, tt.wantChild, tt.wantIdx)
		}
	}
}

func TestBranchFull(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewBranchBuilder(buf, cfg)
	b.SetPtr0(1)

	count := 0
	for {
		key := []byte(fmt.Sprintf("key-%04d", count))
		if !b.AddCell(key, uint64(count+2)) {
			break
		}
		count++
	}
	b.Finish()

	r := NewBranchReader(buf)
	if r.Count() != count {
		t.Errorf("Count() = %d, want %d", r.Count(), count)
	}
	if count == 0 {
		t.Fatal("expected at least one cell to fit")
	}
}

func TestBranchWithChecksum(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: true}
	buf := make([]byte, cfg.PageSize)

	b := NewBranchBuilder(buf, cfg)
	b.SetPtr0(1)
	b.AddCell([]byte("key1"), 2)
	b.AddCell([]byte("key2"), 3)
	b.Finish()

	// Write checksum.
	WriteCRC32C(buf)
	if !VerifyCRC32C(buf) {
		t.Fatal("checksum verification failed")
	}

	// Read back.
	r := NewBranchReader(buf)
	if r.Count() != 2 {
		t.Fatalf("Count() = %d, want 2", r.Count())
	}
}

func TestBranchEmpty(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewBranchBuilder(buf, cfg)
	b.SetPtr0(42)
	b.Finish()

	r := NewBranchReader(buf)
	if r.Count() != 0 {
		t.Errorf("Count() = %d, want 0", r.Count())
	}
	if r.Ptr0() != 42 {
		t.Errorf("Ptr0() = %d, want 42", r.Ptr0())
	}

	// Search with 0 cells should return Ptr0.
	child, idx := r.Search([]byte("anything"))
	if child != 42 || idx != -1 {
		t.Errorf("Search on empty branch: child=%d idx=%d, want 42 -1", child, idx)
	}
}

func TestBranchBuilderCount(t *testing.T) {
	cfg := PageConfig{PageSize: 4096, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)

	b := NewBranchBuilder(buf, cfg)
	b.SetPtr0(1)
	if b.Count() != 0 {
		t.Errorf("Count() = %d, want 0", b.Count())
	}
	b.AddCell([]byte("a"), 2)
	if b.Count() != 1 {
		t.Errorf("Count() = %d, want 1", b.Count())
	}
	b.AddCell([]byte("b"), 3)
	if b.Count() != 2 {
		t.Errorf("Count() = %d, want 2", b.Count())
	}
}

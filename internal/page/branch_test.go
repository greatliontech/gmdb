package page

import (
	"bytes"
	"testing"
)

func newBranchPage(t *testing.T, cfg Config) []byte {
	t.Helper()
	return make([]byte, cfg.PageSize)
}

func TestBranchEmptyRoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newBranchPage(t, cfg)
	EncodeBranchEmpty(buf, cfg, 42)
	if got := BranchLeftmostChild(buf); got != 42 {
		t.Errorf("leftmost = %d, want 42", got)
	}
	if got := BranchCellCount(buf); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	lm, cells := DecodeBranch(buf, cfg)
	if lm != 42 || len(cells) != 0 {
		t.Errorf("decode empty: leftmost=%d cells=%v", lm, cells)
	}
}

func TestBranchEncodeDecodeRoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newBranchPage(t, cfg)
	cells := []BranchCell{
		{Key: []byte("apple"), Child: 10},
		{Key: []byte("banana"), Child: 20},
		{Key: []byte("cherry"), Child: 30},
	}
	if err := EncodeBranch(buf, cfg, 7, cells); err != nil {
		t.Fatalf("EncodeBranch: %v", err)
	}
	lm, got := DecodeBranch(buf, cfg)
	if lm != 7 {
		t.Errorf("leftmost = %d, want 7", lm)
	}
	if len(got) != len(cells) {
		t.Fatalf("decoded %d cells, want %d", len(got), len(cells))
	}
	for i, c := range cells {
		if !bytes.Equal(got[i].Key, c.Key) {
			t.Errorf("cell %d Key: got %q, want %q", i, got[i].Key, c.Key)
		}
		if got[i].Child != c.Child {
			t.Errorf("cell %d Child: got %d, want %d", i, got[i].Child, c.Child)
		}
	}
}

func TestBranchEncodeRejectsUnsorted(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newBranchPage(t, cfg)
	cells := []BranchCell{
		{Key: []byte("b"), Child: 1},
		{Key: []byte("a"), Child: 2},
	}
	if err := EncodeBranch(buf, cfg, 0, cells); err == nil {
		t.Error("expected error on unsorted cells")
	}
}

func TestBranchSearchDescendIndex(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newBranchPage(t, cfg)
	cells := []BranchCell{
		{Key: []byte("c"), Child: 10},
		{Key: []byte("f"), Child: 20},
		{Key: []byte("k"), Child: 30},
	}
	if err := EncodeBranch(buf, cfg, 5, cells); err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Per page-formats.md: BranchSearch returns the smallest i
	// with target < Key[i]. ChildAt(i) picks the right child:
	//   target=b ⇒ search returns 0 (b<c), descend leftmost=5
	//   target=c ⇒ returns 1 (c<f), descend cell 0's child=10
	//   target=d ⇒ returns 1, descend cell 0's child=10
	//   target=f ⇒ returns 2 (f<k), descend cell 1's child=20
	//   target=z ⇒ returns 3, descend cell 2's child=30
	cases := []struct {
		target string
		want   uint64
	}{
		{"b", 5}, {"c", 10}, {"d", 10}, {"e", 10},
		{"f", 20}, {"g", 20}, {"j", 20},
		{"k", 30}, {"l", 30}, {"z", 30},
	}
	for _, c := range cases {
		idx, _ := BranchSearch(buf, cfg, []byte(c.target), NoExtentTail)
		got := BranchChildAt(buf, cfg, idx)
		if got != c.want {
			t.Errorf("search %q: idx=%d child=%d, want child=%d", c.target, idx, got, c.want)
		}
	}
}

func TestBranchEncodedSizeFits(t *testing.T) {
	cfg := Config{PageSize: 4096}
	cells := []BranchCell{
		{Key: bytes.Repeat([]byte("k"), 100), Child: 1},
		{Key: bytes.Repeat([]byte("l"), 100), Child: 2},
	}
	want := branchHeaderEnd + 2*branchDirEntrySize + 2*(100+branchChildPtrSize)
	if got := BranchEncodedSize(cfg, cells); got != want {
		t.Errorf("BranchEncodedSize = %d, want %d", got, want)
	}
}

func TestBranchEncodeRejectsOversized(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newBranchPage(t, cfg)
	// One enormous cell that can't fit.
	cells := []BranchCell{
		{Key: bytes.Repeat([]byte("x"), 5000), Child: 1},
	}
	if err := EncodeBranch(buf, cfg, 0, cells); err == nil {
		t.Error("expected error on oversize cells")
	}
}

func TestShortestSeparatorBoundaryCases(t *testing.T) {
	// Spec inv 1 (page-formats.md §Prefix-Truncated Branch Keys):
	// the separator S satisfies max(L) < S <= min(R).
	cases := []struct {
		left, right, want string
	}{
		{"abc", "abd", "abd"},   // divergence at i=2; sep = right[:3]
		{"abc", "abz", "abz"},   // divergence at i=2
		{"a", "b", "b"},         // divergence at i=0
		{"abc", "abcd", "abcd"}, // left is strict prefix of right; sep extends by one
		{"a", "ab", "ab"},       // same
		{"", "x", "x"},          // empty left, single-byte right
	}
	for _, c := range cases {
		got := ShortestSeparator([]byte(c.left), []byte(c.right))
		if string(got) != c.want {
			t.Errorf("ShortestSeparator(%q, %q) = %q, want %q", c.left, c.right, got, c.want)
		}
		// Spec invariant: left < S <= right.
		if bytes.Compare([]byte(c.left), got) >= 0 {
			t.Errorf("invariant violated: %q !< %q", c.left, got)
		}
		if bytes.Compare(got, []byte(c.right)) > 0 {
			t.Errorf("invariant violated: %q > %q", got, c.right)
		}
	}
}

func TestShortestSeparatorPanicsOnInvalidInput(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on left >= right")
		}
	}()
	ShortestSeparator([]byte("b"), []byte("a"))
}

func TestBranchChecksumPageRoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096, PageChecksum: true}
	buf := newBranchPage(t, cfg)
	cells := []BranchCell{
		{Key: []byte("k"), Child: 99},
	}
	if err := EncodeBranch(buf, cfg, 1, cells); err != nil {
		t.Fatalf("encode: %v", err)
	}
	WritePageFooter(buf, cfg.PageSize)
	if !VerifyPageFooter(buf, cfg.PageSize) {
		t.Error("footer verify failed")
	}
	// Decode survives footer-write region (we don't write past ContentEnd).
	_, got := DecodeBranch(buf, cfg)
	if len(got) != 1 || got[0].Child != 99 {
		t.Errorf("decoded %v after footer", got)
	}
}

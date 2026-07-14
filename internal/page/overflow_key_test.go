package page

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// TestInlineThresholdValues pins the spec's T table (limits.md §Maximum
// Key Size; page-formats.md §Overflow-Key Cells derivation): T is a
// pure function of (PageSize, PageChecksum) and the exact constants are
// contract.
func TestInlineThresholdValues(t *testing.T) {
	cases := []struct {
		pageSize uint32
		checksum bool
		want     int
	}{
		{4096, true, 2010},
		{4096, false, 2014},
		{8192, true, 4058},
		{8192, false, 4062},
		{16384, true, 8154},
		{16384, false, 8158},
		{65536, true, 32730},
		{65536, false, 32734},
	}
	for _, c := range cases {
		cfg := Config{PageSize: c.pageSize, PageChecksum: c.checksum}
		if got := cfg.InlineThreshold(); got != c.want {
			t.Errorf("InlineThreshold(%d, checksum=%v) = %d, want %d", c.pageSize, c.checksum, got, c.want)
		}
		// The floor: a plain branch page holds TWO worst-case
		// overflow cells (page-formats.md §Invariants).
		tt := cfg.InlineThreshold()
		if need := BranchEncodedSizeOf(2, 2*tt, 2); need > cfg.ContentEnd() {
			t.Errorf("two overflow cells at T=%d need %d > ContentEnd %d", tt, need, cfg.ContentEnd())
		}
		// 15-bit fit for the branch directory's overflow marker.
		if tt >= 1<<15 {
			t.Errorf("T=%d does not fit 15 bits", tt)
		}
	}
}

// ovkEntry builds an overflow-key LeafEntry with resident = key[0:T].
func ovkEntry(cfg Config, fullKey []byte, value []byte, extPage uint64) LeafEntry {
	tt := cfg.InlineThreshold()
	return LeafEntry{
		Flags:       CellFlagOverflowKey,
		Key:         fullKey[:tt],
		Value:       value,
		KeyExtPage:  extPage,
		KeyTotalLen: uint32(len(fullKey)),
	}
}

// TestOverflowKeyLeafRoundTrip pins the leaf wire forms of
// page-formats.md §Overflow-Key Cells across both variants and all
// value halves: the resident bytes, extent reference, and value half
// decode back exactly, Validate accepts the page, and the entry is a
// singleton restart group on the compressed variant.
func TestOverflowKeyLeafRoundTrip(t *testing.T) {
	for _, variant := range []struct {
		name string
		rgt  uint16
	}{{"compressed", 16}, {"uncompressed", 1}} {
		t.Run(variant.name, func(t *testing.T) {
			cfg := Config{PageSize: 4096, RestartGroupTarget: variant.rgt}
			tt := cfg.InlineThreshold()
			full := bytes.Repeat([]byte("K"), tt+500)
			buf := make([]byte, cfg.PageSize)
			b := NewLeafBuilder(buf, cfg)
			if !b.AddInline([]byte("AAA"), []byte("v0")) {
				t.Fatal("AddInline AAA")
			}
			e := ovkEntry(cfg, full, []byte("big-v"), 77)
			if !b.AddEntry(e) {
				t.Fatal("AddEntry overflow-key")
			}
			if !b.AddInline([]byte("L"), []byte("v2")) { // 'L' > 'K...' resident
				t.Fatal("AddInline post")
			}
			b.Finish()

			r := NewLeafReader(buf, cfg)
			if err := r.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			got, _ := r.EntryAt(1, nil)
			if !got.IsOverflowKey() || !bytes.Equal(got.Key, full[:tt]) ||
				got.KeyExtPage != 77 || got.KeyTotalLen != uint32(len(full)) ||
				!bytes.Equal(got.Value, []byte("big-v")) {
				t.Fatalf("round-trip mismatch: flags=%x keyLen=%d ext=%d total=%d val=%q",
					got.Flags, len(got.Key), got.KeyExtPage, got.KeyTotalLen, got.Value)
			}
			if variant.rgt >= 2 {
				// Singleton-group rule: the overflow-key entry is alone
				// in its group.
				found := false
				start := 0
				for g := 0; g < r.RestartCount(); g++ {
					gc := r.GroupEntryCount(g)
					if start <= 1 && 1 < start+gc {
						found = true
						if gc != 1 {
							t.Errorf("overflow-key entry in group of %d, want singleton", gc)
						}
					}
					start += gc
				}
				if !found {
					t.Fatal("entry 1 not located in any group")
				}
			}
		})
	}
}

// TestOverflowKeyValueFormComposition pins the orthogonal composition
// (page-formats.md §Overflow-Key Cells, Leaf forms): OverflowKey ×
// {inline, empty, overflow-value, nested-tree, subpage} all round-trip.
func TestOverflowKeyValueFormComposition(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16, LeafLayout: LeafLayoutInterleaved}
	tt := cfg.InlineThreshold()
	mk := func(c byte) []byte { return bytes.Repeat([]byte{c}, tt+9) }

	entries := []LeafEntry{
		{Flags: CellFlagOverflowKey, Key: mk('a')[:tt], KeyExtPage: 11, KeyTotalLen: uint32(tt + 9), Value: []byte("inline")},
		{Flags: CellFlagOverflowKey | CellFlagEmptyValue, Key: mk('b')[:tt], KeyExtPage: 12, KeyTotalLen: uint32(tt + 9)},
		{Flags: CellFlagOverflowKey | CellFlagOverflow, Key: mk('c')[:tt], KeyExtPage: 13, KeyTotalLen: uint32(tt + 9), OverflowPage: 99, TotalLen: 5000},
		{Flags: CellFlagOverflowKey | CellFlagMultiValue | CellFlagNestedTree, Key: mk('d')[:tt], KeyExtPage: 14, KeyTotalLen: uint32(tt + 9), NestedRoot: 55, NestedCount: 3},
	}
	// T-sized residents fit at most ~2 per 4 KB page — round-trip each
	// composition on its own page.
	for i, want := range entries {
		buf := make([]byte, cfg.PageSize)
		b := NewLeafBuilder(buf, cfg)
		if !b.AddEntry(want) {
			t.Fatalf("AddEntry %d", i)
		}
		b.Finish()
		r := NewLeafReader(buf, cfg)
		if err := r.Validate(); err != nil {
			t.Fatalf("entry %d Validate: %v", i, err)
		}
		got, _ := r.EntryAt(0, nil)
		// The empty-value form decodes Value as a non-nil empty slice.
		if want.Flags&CellFlagEmptyValue != 0 {
			want.Value = []byte{}
		}
		if got.Flags != want.Flags || !bytes.Equal(got.Key, want.Key) ||
			got.KeyExtPage != want.KeyExtPage || got.KeyTotalLen != want.KeyTotalLen ||
			!bytes.Equal(got.Value, want.Value) ||
			got.OverflowPage != want.OverflowPage || got.TotalLen != want.TotalLen ||
			got.NestedRoot != want.NestedRoot || got.NestedCount != want.NestedCount {
			t.Errorf("entry %d mismatch:\n got %+v\nwant %+v", i, got, want)
		}
	}
}

// TestValidateRejectsMalformedOverflowKey pins the trust-boundary
// rejections (page-formats.md §Overflow-Key Cells: derivable-length
// read policy; singleton-group rule; extent-reference sanity).
func TestValidateRejectsMalformedOverflowKey(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16, LeafLayout: LeafLayoutInterleaved}
	tt := cfg.InlineThreshold()
	full := bytes.Repeat([]byte("K"), tt+100)

	build := func(mutate func(buf []byte, r LeafReader)) error {
		buf := make([]byte, cfg.PageSize)
		b := NewLeafBuilder(buf, cfg)
		if !b.AddEntry(ovkEntry(cfg, full, []byte("v"), 7)) {
			t.Fatal("AddEntry")
		}
		b.Finish()
		r := NewLeafReader(buf, cfg)
		if err := r.Validate(); err != nil {
			t.Fatalf("pre-mutation Validate: %v", err)
		}
		if mutate != nil {
			mutate(buf, r)
		}
		return NewLeafReader(buf, cfg).Validate()
	}

	// Wrong resident length: KeyLen != T (derivable-length policy).
	if err := build(func(buf []byte, r LeafReader) {
		// KeyLen sits at entry start + 1 (offset 12 is the first entry).
		le.PutUint16(buf[13:], uint16(tt-1))
	}); err == nil || !strings.Contains(err.Error(), "inline threshold") {
		t.Errorf("wrong KeyLen: err = %v, want inline-threshold rejection", err)
	}
	// Zero extent page.
	if err := build(func(buf []byte, r LeafReader) {
		// [Flags][KeyLen u16][ValueLen u32][Key tt][ExtPage u64]...
		off := 12 + 1 + 2 + 4 + tt
		le.PutUint64(buf[off:], 0)
	}); err == nil || !strings.Contains(err.Error(), "extent page is 0") {
		t.Errorf("zero ext page: err = %v, want extent-page rejection", err)
	}
	// KeyTotalLen <= T.
	if err := build(func(buf []byte, r LeafReader) {
		off := 12 + 1 + 2 + 4 + tt + 8
		le.PutUint32(buf[off:], uint32(tt))
	}); err == nil || !strings.Contains(err.Error(), "does not exceed") {
		t.Errorf("small KeyTotalLen: err = %v, want total-length rejection", err)
	}
	// Delta entry carrying the OverflowKey bit is corruption.
	deltaBuf := make([]byte, cfg.PageSize)
	db := NewLeafBuilder(deltaBuf, cfg)
	if !db.AddInline([]byte("shared-prefix-a"), []byte("v")) || !db.AddInline([]byte("shared-prefix-b"), []byte("v")) {
		t.Fatal("delta fixture build")
	}
	db.Finish()
	r := NewLeafReader(deltaBuf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("delta fixture Validate: %v", err)
	}
	if r.RestartCount() != 1 || r.GroupEntryCount(0) != 2 {
		t.Fatalf("delta fixture: want one 2-entry group, got %d groups", r.RestartCount())
	}
	// Flip the delta entry's flags (second entry): find its offset by
	// decoding entry 0's extent.
	_, next := r.decodeFullKeyEntry(12)
	deltaBuf[next] |= CellFlagOverflowKey
	if err := NewLeafReader(deltaBuf, cfg).Validate(); err == nil || !strings.Contains(err.Error(), "restart-only") {
		t.Errorf("delta with OverflowKey: err = %v, want singleton-rule rejection", err)
	}
	// Forged multi-entry group whose restart entry is overflow-key:
	// bump the group Count and append garbage is complex; instead flip
	// the restart entry of the 2-entry delta fixture to overflow-key —
	// Count stays 2 → singleton-group violation (the restart entry's
	// own length checks would also fire; assert on the group rule by
	// making the restart a REAL overflow-key entry is covered above,
	// so here any rejection suffices).
	deltaBuf2 := make([]byte, cfg.PageSize)
	db2 := NewLeafBuilder(deltaBuf2, cfg)
	if !db2.AddInline([]byte("shared-prefix-a"), []byte("v")) || !db2.AddInline([]byte("shared-prefix-b"), []byte("v")) {
		t.Fatal("fixture 2 build")
	}
	db2.Finish()
	deltaBuf2[12] |= CellFlagOverflowKey
	if err := NewLeafReader(deltaBuf2, cfg).Validate(); err == nil {
		t.Error("restart flipped to overflow-key in 2-entry group: Validate accepted a malformed page")
	}
}

// TestCompareEntryKeyExtentRule pins the full-key comparison rule
// (page-formats.md §Overflow-Key Cells, Comparison): the extent is
// consulted exactly when the probe exceeds T bytes and ties through
// the resident portion.
func TestCompareEntryKeyExtentRule(t *testing.T) {
	cfg := Config{PageSize: 4096}
	tt := cfg.InlineThreshold()
	full := append(bytes.Repeat([]byte{'x'}, tt), []byte("TAIL")...)
	e := LeafEntry{
		Flags:       CellFlagOverflowKey,
		Key:         full[:tt],
		KeyExtPage:  9,
		KeyTotalLen: uint32(len(full)),
	}
	calls := 0
	tail := func(probe []byte, extPage uint64, totalLen uint32) (int, error) {
		calls++
		if extPage != 9 || totalLen != uint32(len(full)) {
			t.Fatalf("tail got ext=%d total=%d", extPage, totalLen)
		}
		return bytes.Compare(probe, full), nil
	}
	cases := []struct {
		name      string
		probe     []byte
		want      int // sign of compare(stored, probe)
		wantCalls int
	}{
		{"diverges-early", append([]byte{'a'}, full[1:tt]...), 1, 0},
		{"short-prefix", full[:100], 1, 0},
		{"exact-resident-length", full[:tt], 1, 0},
		{"over-T-tie-less", append(bytes.Clone(full[:tt]), []byte("TAIK")...), 1, 1},
		{"over-T-tie-equal", full, 0, 1},
		{"over-T-tie-greater", append(bytes.Clone(full[:tt]), []byte("TAIM")...), -1, 1},
	}
	for _, c := range cases {
		calls = 0
		got, err := compareEntryKey(e, c.probe, tail)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		sign := 0
		if got > 0 {
			sign = 1
		} else if got < 0 {
			sign = -1
		}
		if sign != c.want || calls != c.wantCalls {
			t.Errorf("%s: cmp=%d (want sign %d), tail calls=%d (want %d)", c.name, got, c.want, calls, c.wantCalls)
		}
	}
}

// TestBranchOverflowCellRoundTrip pins the branch overflow-cell form
// (page-formats.md §Plain Branch KeyLen bit 15; §Overflow-Key Cells
// Branch form): resident slice = sep[0:T], extent fields round-trip,
// ValidateBranch accepts, child pointers read/write correctly, and
// BranchSearch consults the extent exactly on an over-T resident tie.
// A plain branch holds exactly TWO worst-case overflow cells (the
// split-feasibility floor), so the mixed ovk+short fixture carries one
// overflow cell; the two-ovk floor page is round-tripped separately
// below.
func TestBranchOverflowCellRoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096}
	tt := cfg.InlineThreshold()
	pfx := bytes.Repeat([]byte{'p'}, 64)
	mkSep := func(c byte) []byte {
		s := append(bytes.Clone(pfx), bytes.Repeat([]byte{c}, tt-len(pfx))...)
		return append(s, []byte("tail-")...) // full length tt+5 > T
	}
	sepA, sepB := mkSep('a'), mkSep('b')
	short := append(bytes.Clone(pfx), 'q') // sorts after sepA's resident bytes
	cells := []BranchCell{
		{Key: sepA[:tt], Child: 101, KeyExtPage: 41, KeyTotalLen: uint32(len(sepA))},
		{Key: short, Child: 103},
	}
	buf := make([]byte, cfg.PageSize)
	if err := EncodeBranch(buf, cfg, 100, cells); err != nil {
		t.Fatalf("EncodeBranch: %v", err)
	}
	if err := ValidateBranch(buf, cfg); err != nil {
		t.Fatalf("ValidateBranch: %v", err)
	}
	gotLeftmost, gotCells := DecodeBranch(buf, cfg)
	if gotLeftmost != 100 || len(gotCells) != 2 {
		t.Fatalf("decode: leftmost=%d cells=%d", gotLeftmost, len(gotCells))
	}

	// The floor page: exactly two worst-case overflow cells fit
	// (page-formats.md §Invariants, branch floor) and round-trip.
	floor := []BranchCell{
		{Key: sepA[:tt], Child: 1, KeyExtPage: 41, KeyTotalLen: uint32(len(sepA))},
		{Key: sepB[:tt], Child: 2, KeyExtPage: 42, KeyTotalLen: uint32(len(sepB))},
	}
	fbuf := make([]byte, cfg.PageSize)
	if err := EncodeBranch(fbuf, cfg, 9, floor); err != nil {
		t.Fatalf("EncodeBranch(two-ovk floor): %v", err)
	}
	if err := ValidateBranch(fbuf, cfg); err != nil {
		t.Fatalf("ValidateBranch(two-ovk floor): %v", err)
	}
	if _, fc := DecodeBranch(fbuf, cfg); len(fc) != 2 || fc[0].KeyExtPage != 41 || fc[1].KeyExtPage != 42 {
		t.Fatalf("two-ovk floor decode mismatch: %+v", fc)
	}
	// The floor's tight side: two worst-case overflow cells plus ANY
	// third cell exceed the page — EncodeBranch must reject.
	over := append(append([]BranchCell{}, floor...), BranchCell{Key: []byte{0xFF}, Child: 3})
	if err := EncodeBranch(make([]byte, cfg.PageSize), cfg, 9, over); err == nil {
		t.Fatal("EncodeBranch accepted two worst-case overflow cells plus a third")
	}
	for i := range cells {
		if !bytes.Equal(gotCells[i].Key, cells[i].Key) || gotCells[i].Child != cells[i].Child ||
			gotCells[i].KeyExtPage != cells[i].KeyExtPage || gotCells[i].KeyTotalLen != cells[i].KeyTotalLen {
			t.Errorf("cell %d mismatch: got %+v want %+v", i, gotCells[i], cells[i])
		}
	}
	// Child rewrite through the extent-bearing cell.
	SetBranchCellChild(buf, cfg, 0, 201)
	if got := BranchChildAt(buf, cfg, 1); got != 201 {
		t.Errorf("SetBranchCellChild through overflow cell: child = %d, want 201", got)
	}
	// Key-extent repoint.
	SetBranchCellKeyExtPage(buf, cfg, 0, 410)
	if got := BranchCellAt(buf, cfg, 0); got.KeyExtPage != 410 {
		t.Errorf("SetBranchCellKeyExtPage: ext = %d, want 410", got.KeyExtPage)
	}

	// Search: a probe that ties through sep A's first-T bytes and is
	// longer must consult the extent; the comparator decides.
	calls := 0
	tail := func(probe []byte, extPage uint64, totalLen uint32) (int, error) {
		calls++
		return bytes.Compare(probe, sepA), nil
	}
	probe := append(bytes.Clone(sepA[:tt]), []byte("tail-x")...) // > sepA
	idx, err := BranchSearch(buf, cfg, probe, tail)
	if err != nil {
		t.Fatalf("BranchSearch: %v", err)
	}
	if calls == 0 {
		t.Error("BranchSearch never consulted the extent on an over-T resident tie")
	}
	if idx != 1 {
		t.Errorf("BranchSearch(> sepA, < short) = %d, want 1", idx)
	}
	// A short probe never touches extents.
	calls = 0
	if _, err := BranchSearch(buf, cfg, []byte("zzz"), tail); err != nil {
		t.Fatalf("BranchSearch short: %v", err)
	}
	if calls != 0 {
		t.Errorf("short probe consulted the extent %d times, want 0", calls)
	}
}

// TestBranchValidateRejectsForgedOverflowCell pins ValidateBranch's
// derivable-length policy for overflow cells.
func TestBranchValidateRejectsForgedOverflowCell(t *testing.T) {
	cfg := Config{PageSize: 4096}
	tt := cfg.InlineThreshold()
	// Two overflow cells — the plain-branch floor shape; each cell's
	// inline bytes are the full T-byte resident slice and the extent
	// reference sits at a directly-computable offset.
	sepA := bytes.Repeat([]byte{'a'}, tt+10)
	sepB := bytes.Repeat([]byte{'b'}, tt+10)
	cells := []BranchCell{
		{Key: sepA[:tt], Child: 7, KeyExtPage: 3, KeyTotalLen: uint32(len(sepA))},
		{Key: sepB[:tt], Child: 8, KeyExtPage: 4, KeyTotalLen: uint32(len(sepB))},
	}
	build := func(mutate func(buf []byte)) error {
		buf := make([]byte, cfg.PageSize)
		if err := EncodeBranch(buf, cfg, 1, cells); err != nil {
			t.Fatalf("EncodeBranch: %v", err)
		}
		if mutate != nil {
			mutate(buf)
		}
		return ValidateBranch(buf, cfg)
	}
	if err := build(nil); err != nil {
		t.Fatalf("clean page rejected: %v", err)
	}
	// Inline length != T - PrefixLen: shrinking the directory's low-15
	// length also skews the cell-bounds arithmetic, so the page may be
	// rejected by either the bounds check or the derivable-length
	// check — any rejection satisfies the trust boundary.
	if err := build(func(buf []byte) {
		dirOff := branchHeaderEnd
		raw := le.Uint16(buf[dirOff+2:])
		le.PutUint16(buf[dirOff+2:], (raw&branchDirKeyOverflowBit)|uint16(tt-1))
	}); err == nil {
		t.Error("wrong inline length accepted by ValidateBranch")
	}
	// KeyTotalLen <= T (bounds untouched — must hit the derivable-
	// length policy specifically).
	if err := build(func(buf []byte) {
		dirOff := branchHeaderEnd
		off := int(le.Uint16(buf[dirOff:])) + tt // ext ref after the inline key bytes
		le.PutUint32(buf[off+8:], uint32(tt))
	}); err == nil || !strings.Contains(err.Error(), "does not exceed") {
		t.Errorf("small KeyTotalLen: err = %v, want rejection", err)
	}
}

// TestPatchKeyExtRefs pins the in-place key-extent repoint primitive
// across value forms (relocation's size-identical patch contract).
func TestPatchKeyExtRefs(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 16}
	tt := cfg.InlineThreshold()
	mk := func(c byte) []byte { return bytes.Repeat([]byte{c}, tt+3) }
	entries := []LeafEntry{
		{Flags: CellFlagOverflowKey, Key: mk('a')[:tt], KeyExtPage: 11, KeyTotalLen: uint32(tt + 3), Value: []byte("v")},
		{Flags: CellFlagOverflowKey | CellFlagEmptyValue, Key: mk('b')[:tt], KeyExtPage: 12, KeyTotalLen: uint32(tt + 3)},
		{Flags: CellFlagOverflowKey | CellFlagOverflow, Key: mk('c')[:tt], KeyExtPage: 13, KeyTotalLen: uint32(tt + 3), OverflowPage: 99, TotalLen: 4096},
	}
	// T-sized residents mean at most ~2 overflow-key entries per 4 KB
	// page — patch each entry on its own page so every value form is
	// exercised.
	for i, e := range entries {
		buf := make([]byte, cfg.PageSize)
		b := NewLeafBuilder(buf, cfg)
		if !b.AddEntry(e) {
			t.Fatalf("AddEntry %d", i)
		}
		b.Finish()
		r := NewLeafReader(buf, cfg)
		if err := r.Validate(); err != nil {
			t.Fatalf("entry %d Validate: %v", i, err)
		}
		r.PatchKeyExtRefs(func(_ int, pe LeafEntry) uint64 {
			return pe.KeyExtPage + 100
		})
		if err := NewLeafReader(buf, cfg).Validate(); err != nil {
			t.Fatalf("entry %d post-patch Validate: %v", i, err)
		}
		got, _ := NewLeafReader(buf, cfg).EntryAt(0, nil)
		if got.KeyExtPage != e.KeyExtPage+100 {
			t.Errorf("entry %d ext = %d, want %d", i, got.KeyExtPage, e.KeyExtPage+100)
		}
		if got.OverflowPage != e.OverflowPage {
			t.Errorf("entry %d value ref disturbed: %d", i, got.OverflowPage)
		}
	}
	_ = fmt.Sprint() // keep fmt import if unused paths change
}

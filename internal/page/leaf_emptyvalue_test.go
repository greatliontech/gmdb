package page

import (
	"bytes"
	"fmt"
	"testing"
)

// The empty-value cell form (CellFlags bit 3): plain cells with empty
// values encode without the 4-byte ValueLen field (page-formats.md
// §Leaf Page, empty-value cell). Encoders emit it unconditionally for
// empty plain values; decoders accept BOTH it and the legacy
// zero-ValueLen inline form.

func emptyValEntries(n int) []LeafEntry {
	es := make([]LeafEntry, 0, n)
	for i := range n {
		es = append(es, LeafEntry{Key: fmt.Appendf(nil, "member-%04d", i)})
	}
	return es
}

// Round-trip in both variants: build → Validate → iterate → search;
// then splice-insert and splice-delete around empty-value cells.
func TestEmptyValueCellRoundTripAndSplice(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"compressed", Config{PageSize: 4096, RestartGroupTarget: 4, PageChecksum: false, LeafLayout: LeafLayoutInterleaved}},
		{"uncompressed", Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := emptyValEntries(40)
			buf := make([]byte, tc.cfg.PageSize)
			b := NewLeafBuilder(buf, tc.cfg)
			for _, e := range es {
				if !b.AddEntry(e) {
					t.Fatalf("AddEntry(%q) full", e.Key)
				}
			}
			b.Finish()
			r := NewLeafReader(buf, tc.cfg)
			if err := r.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			i := 0
			it := r.IterForReuse(nil, nil, nil)
			for e, ok := it.Next(); ok; e, ok = it.Next() {
				if !bytes.Equal(e.Key, es[i].Key) {
					t.Fatalf("entry %d key=%q want %q", i, e.Key, es[i].Key)
				}
				if e.Value == nil || len(e.Value) != 0 {
					t.Fatalf("entry %d value=%v, want non-nil empty", i, e.Value)
				}
				if e.Flags&CellFlagEmptyValue == 0 {
					t.Fatalf("entry %d flags=%#x, want EmptyValue set", i, e.Flags)
				}
				i++
			}
			if i != len(es) {
				t.Fatalf("iterated %d, want %d", i, len(es))
			}
			// Splice-insert a new empty-value member mid-page, then
			// splice-delete an existing one; the page must stay
			// Validate-clean with correct membership.
			if !TryInsertAt(buf, tc.cfg, 7, LeafEntry{Key: []byte("member-0006x")}) {
				t.Fatal("TryInsertAt declined")
			}
			if !TryDeleteAt(buf, tc.cfg, 20) {
				t.Fatal("TryDeleteAt declined")
			}
			r2 := NewLeafReader(buf, tc.cfg)
			if err := r2.Validate(); err != nil {
				t.Fatalf("post-splice Validate: %v", err)
			}
			if idx, _, found, _ := r2.SearchLeaf([]byte("member-0006x"), NoExtentTail); !found {
				t.Fatal("spliced-in member not found")
			} else if idx != 7 {
				t.Fatalf("spliced-in member at %d, want 7", idx)
			}
		})
	}
}

// Density: the compact form saves exactly 4 bytes per empty-value
// cell — DataEnd equals the sum of the per-entry key halves with NO
// ValueLen contribution. Pins that the encoders actually emit the
// compact form (a legacy-form emitter regresses DataEnd by 4×N).
func TestEmptyValueCellDensityExact(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}
	const n = 50
	es := emptyValEntries(n)
	buf := make([]byte, cfg.PageSize)
	b := NewLeafBuilder(buf, cfg)
	for _, e := range es {
		if !b.AddEntry(e) {
			t.Fatalf("AddEntry(%q) full", e.Key)
		}
	}
	b.Finish()
	want := 0
	for _, e := range es {
		want += 1 + 2 + len(e.Key) // Flags + KeyLen + key; no ValueLen
	}
	r := NewLeafReader(buf, cfg)
	got := r.DataEnd() - 12
	if got != want {
		t.Fatalf("entry-data bytes = %d, want %d (compact form saves 4/cell; legacy would be %d)",
			got, want, want+4*n)
	}
}

// Legacy acceptance: a hand-encoded zero-ValueLen inline cell (the
// pre-compact form) must still Validate and decode as an empty value —
// mixed-form pages are valid per the spec.
func TestEmptyValueLegacyZeroValueLenStillDecodes(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)
	// Hand-encode one legacy inline cell: [Flags=0][KeyLen][ValueLen=0][Key].
	key := []byte("legacy-key")
	off := 12
	buf[off] = 0
	off++
	le.PutUint16(buf[off:], uint16(len(key)))
	off += 2
	le.PutUint32(buf[off:], 0)
	off += 4
	copy(buf[off:], key)
	off += len(key)
	// Header: uncompressed leaf, count 1, DataEnd, offset table.
	WriteHeader(buf, TypeLeafUncompressed, 1, 0)
	le.PutUint16(buf[ucLeafOffDataEnd:], uint16(off))
	le.PutUint16(buf[cfg.ContentEnd()-2:], 12)

	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("legacy form must stay valid: %v", err)
	}
	idx, e, found, _ := r.SearchLeaf(key, NoExtentTail)
	if !found || idx != 0 {
		t.Fatalf("legacy cell not found (idx=%d found=%v)", idx, found)
	}
	if len(e.Value) != 0 {
		t.Fatalf("legacy cell value len=%d, want 0", len(e.Value))
	}
}

// The exclusivity clause (page-formats.md: EmptyValue is exclusive
// with every other bit) is enforced at the Validate boundary: any
// combination of bit 3 with another flag is structural corruption —
// the hot decoders would otherwise misread the cell under one of the
// other forms.
func TestEmptyValueExclusivityRejectedByValidate(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}
	for _, flags := range []uint8{
		CellFlagEmptyValue | CellFlagOverflow,
		CellFlagEmptyValue | CellFlagMultiValue,
		CellFlagEmptyValue | CellFlagMultiValue | CellFlagNestedTree,
	} {
		buf := make([]byte, cfg.PageSize)
		key := []byte("k")
		off := 12
		buf[off] = flags
		off++
		le.PutUint16(buf[off:], uint16(len(key)))
		off += 2
		copy(buf[off:], key)
		off += len(key)
		// Pad a plausible trailer so bounds aren't the failure cause.
		off += 16
		WriteHeader(buf, TypeLeafUncompressed, 1, 0)
		le.PutUint16(buf[ucLeafOffDataEnd:], uint16(off))
		le.PutUint16(buf[cfg.ContentEnd()-2:], 12)
		r := NewLeafReader(buf, cfg)
		if err := r.Validate(); err == nil {
			t.Fatalf("flags %#x: Validate accepted an illegal EmptyValue combination", flags)
		}
	}
}

// Mixed-form pages are valid: a legacy zero-ValueLen cell and a
// compact empty-value cell coexisting on one page both decode as
// empty values.
func TestEmptyValueMixedFormPage(t *testing.T) {
	cfg := Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}
	buf := make([]byte, cfg.PageSize)
	off := 12
	off0 := off
	// Legacy cell: [0][KeyLen][ValueLen=0][Key].
	k0 := []byte("aaa")
	buf[off] = 0
	off++
	le.PutUint16(buf[off:], uint16(len(k0)))
	off += 2
	le.PutUint32(buf[off:], 0)
	off += 4
	copy(buf[off:], k0)
	off += len(k0)
	off1 := off
	// Compact cell: [EmptyValue][KeyLen][Key].
	k1 := []byte("bbb")
	buf[off] = CellFlagEmptyValue
	off++
	le.PutUint16(buf[off:], uint16(len(k1)))
	off += 2
	copy(buf[off:], k1)
	off += len(k1)
	WriteHeader(buf, TypeLeafUncompressed, 2, 0)
	le.PutUint16(buf[ucLeafOffDataEnd:], uint16(off))
	le.PutUint16(buf[cfg.ContentEnd()-4:], uint16(off0))
	le.PutUint16(buf[cfg.ContentEnd()-2:], uint16(off1))

	r := NewLeafReader(buf, cfg)
	if err := r.Validate(); err != nil {
		t.Fatalf("mixed-form page must validate: %v", err)
	}
	for i, want := range [][]byte{k0, k1} {
		e, _ := r.EntryAt(i, nil)
		if !bytes.Equal(e.Key, want) || len(e.Value) != 0 {
			t.Fatalf("entry %d: key=%q valueLen=%d, want %q/0", i, e.Key, len(e.Value), want)
		}
	}
}

// FirstKey / LastKey use manual skip math rather than the decoders —
// they must handle the empty-value form (no ValueLen to skip).
// Previously both returned the key shifted by four bytes on such
// cells.
func TestEmptyValueFirstLastKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"compressed", Config{PageSize: 4096, RestartGroupTarget: 4, PageChecksum: false, LeafLayout: LeafLayoutInterleaved}},
		{"uncompressed", Config{PageSize: 4096, RestartGroupTarget: 1, PageChecksum: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			es := emptyValEntries(9)
			buf := make([]byte, tc.cfg.PageSize)
			b := NewLeafBuilder(buf, tc.cfg)
			for _, e := range es {
				b.AddEntry(e)
			}
			b.Finish()
			r := NewLeafReader(buf, tc.cfg)
			if got := r.FirstKey(); !bytes.Equal(got, es[0].Key) {
				t.Fatalf("FirstKey = %q, want %q", got, es[0].Key)
			}
			if got, _ := r.LastKey(nil); !bytes.Equal(got, es[len(es)-1].Key) {
				t.Fatalf("LastKey = %q, want %q", got, es[len(es)-1].Key)
			}
		})
	}
}

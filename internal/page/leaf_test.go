package page

import (
	"bytes"
	"fmt"
	"testing"
)

func newLeafPage(t *testing.T, cfg Config) []byte {
	t.Helper()
	return make([]byte, cfg.PageSize)
}

func TestLeafEmptyRoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	if err := EncodeLeaf(buf, cfg, 16, nil); err != nil {
		t.Fatalf("EncodeLeaf empty: %v", err)
	}
	if got := LeafEntryCount(buf); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	if got := LeafRestartInterval(buf); got != 16 {
		t.Errorf("interval = %d, want 16", got)
	}
	if got := LeafRestartCount(buf); got != 0 {
		t.Errorf("restart count = %d, want 0", got)
	}
	got, err := DecodeLeaf(buf, cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("decoded %d entries, want 0", len(got))
	}
}

func TestLeafSingleEntryRoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	entries := []EncodedEntry{
		{Key: []byte("apple"), Value: []byte("fruit-A")},
	}
	if err := EncodeLeaf(buf, cfg, 16, entries); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := LeafRestartCount(buf); got != 1 {
		t.Errorf("restart count = %d, want 1 (single entry is its own restart)", got)
	}
	decoded, err := DecodeLeaf(buf, cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("decoded %d entries, want 1", len(decoded))
	}
	if !bytes.Equal(decoded[0].Key, []byte("apple")) {
		t.Errorf("Key: got %q, want apple", decoded[0].Key)
	}
	if !bytes.Equal(decoded[0].Value, []byte("fruit-A")) {
		t.Errorf("Value: got %q", decoded[0].Value)
	}
}

func TestLeafDeltaCompressionRoundTrip(t *testing.T) {
	// Spec invariant 2 (page-formats.md §Leaf Page): a delta entry's
	// SharedLen counts leading bytes shared with the PREVIOUS entry
	// in the same restart group. The reconstructed key must equal
	// previous[0:SharedLen] || UnsharedKey. Pin with a high-prefix
	// workload at interval=4 so two restart groups exist.
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	entries := []EncodedEntry{
		{Key: []byte("aaaa-000"), Value: []byte("v0")},
		{Key: []byte("aaaa-001"), Value: []byte("v1")},
		{Key: []byte("aaaa-002"), Value: []byte("v2")},
		{Key: []byte("aaaa-003"), Value: []byte("v3")},
		{Key: []byte("bbbb-000"), Value: []byte("v4")}, // new restart at i=4
		{Key: []byte("bbbb-001"), Value: []byte("v5")},
	}
	if err := EncodeLeaf(buf, cfg, 4, entries); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := LeafRestartCount(buf); got != 2 {
		t.Errorf("restart count = %d, want 2 (entries at i=0 + i=4)", got)
	}
	decoded, err := DecodeLeaf(buf, cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != len(entries) {
		t.Fatalf("decoded %d entries, want %d", len(decoded), len(entries))
	}
	for i, e := range entries {
		if !bytes.Equal(decoded[i].Key, e.Key) {
			t.Errorf("entry %d Key: got %q, want %q", i, decoded[i].Key, e.Key)
		}
		if !bytes.Equal(decoded[i].Value, e.Value) {
			t.Errorf("entry %d Value: got %q, want %q", i, decoded[i].Value, e.Value)
		}
	}
	// Spec inv 2 strengthening (chunk-4.2 close-out finding L1):
	// the test name promises "delta compression" — assert
	// compression actually fired by comparing the encoded size
	// against the no-compression alternative (interval=1, every
	// entry its own restart with full key). With a 4-byte shared
	// prefix on every delta, interval=4 must produce a strictly
	// smaller page than interval=1.
	compressed := LeafEncodedSize(cfg, 4, entries)
	uncompressed := LeafEncodedSize(cfg, 1, entries)
	if compressed >= uncompressed {
		t.Errorf("compression didn't shrink page: interval=4 size=%d, interval=1 size=%d (want < )",
			compressed, uncompressed)
	}
}

func TestLeafRestartTablePosition(t *testing.T) {
	// Spec invariant 3 (page-formats.md §Leaf Page): "A leaf's
	// RestartCount × 2 bytes immediately before the optional 8-byte
	// xxhash64 footer constitute the restart table." Pin by reading
	// the restart entries' offsets via the public accessor and
	// verifying they point to where entries actually live.
	for _, withChecksum := range []bool{false, true} {
		t.Run(fmt.Sprintf("checksum=%v", withChecksum), func(t *testing.T) {
			cfg := Config{PageSize: 4096, PageChecksum: withChecksum}
			buf := newLeafPage(t, cfg)
			entries := []EncodedEntry{
				{Key: []byte("a"), Value: []byte("1")},
				{Key: []byte("b"), Value: []byte("2")},
				{Key: []byte("c"), Value: []byte("3")},
				{Key: []byte("d"), Value: []byte("4")},
				{Key: []byte("e"), Value: []byte("5")},
				{Key: []byte("f"), Value: []byte("6")},
			}
			if err := EncodeLeaf(buf, cfg, 3, entries); err != nil {
				t.Fatalf("encode: %v", err)
			}
			// Restart count = ceil(6/3) = 2 (entries at i=0, i=3).
			rc := LeafRestartCount(buf)
			if rc != 2 {
				t.Fatalf("restart count = %d, want 2", rc)
			}
			tableStart := leafRestartTableStart(cfg, rc)
			wantTableStart := cfg.ContentEnd() - 2*2 // rc * 2 bytes/entry
			if tableStart != wantTableStart {
				t.Errorf("table start = %d, want %d", tableStart, wantTableStart)
			}
			// Restart entry 0 should be at leafHeaderEnd (=12).
			if got := LeafRestartOffset(buf, cfg, 0); got != leafHeaderEnd {
				t.Errorf("restart[0] offset = %d, want %d", got, leafHeaderEnd)
			}
			// Restart entry 1's offset must point at the
			// first entry of the second group (i=3, "d"). Decode
			// at that offset and verify it's a restart-format
			// entry with the full key — pins that the restart
			// table contents agree with the encoded layout, not
			// just the table's position.
			r1 := LeafRestartOffset(buf, cfg, 1)
			if r1 <= leafHeaderEnd {
				t.Errorf("restart[1] offset = %d, expected > %d", r1, leafHeaderEnd)
			}
			entry, _, err := decodeLeafEntry(buf, int(r1), cfg.ContentEnd(), true, nil)
			if err != nil {
				t.Fatalf("decode entry at restart[1] offset %d: %v", r1, err)
			}
			if !bytes.Equal(entry.Key, []byte("d")) {
				t.Errorf("restart[1] entry Key = %q, want %q", entry.Key, "d")
			}
			// Footer-aware: with checksum, applying WritePageFooter
			// shouldn't clobber the restart table (it lives below
			// the footer).
			if withChecksum {
				WritePageFooter(buf, cfg.PageSize)
				if got := LeafRestartCount(buf); got != rc {
					t.Errorf("restart count after footer: got %d, want %d", got, rc)
				}
				decoded, err := DecodeLeaf(buf, cfg)
				if err != nil || len(decoded) != len(entries) {
					t.Errorf("decode after footer failed: %v decoded=%d", err, len(decoded))
				}
			}
		})
	}
}

func TestLeafOverflowEntryRoundTrip(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	// Order: "big" < "small" lexicographically.
	entries := []EncodedEntry{
		{
			Key:          []byte("big"),
			Flags:        CellFlagOverflow,
			OverflowPage: 12345,
			TotalLen:     999999,
		},
		{Key: []byte("small"), Value: []byte("inline")},
	}
	if err := EncodeLeaf(buf, cfg, 16, entries); err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeLeaf(buf, cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded[0].IsOverflow() || decoded[0].OverflowPage != 12345 || decoded[0].TotalLen != 999999 {
		t.Errorf("overflow entry round-trip: %+v", decoded[0])
	}
	if decoded[1].IsOverflow() {
		t.Errorf("inline entry IsOverflow incorrectly set")
	}
	if !bytes.Equal(decoded[1].Value, []byte("inline")) {
		t.Errorf("inline value: %q", decoded[1].Value)
	}
}

func TestLeafRejectsUnsortedEntries(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	entries := []EncodedEntry{
		{Key: []byte("b"), Value: []byte("1")},
		{Key: []byte("a"), Value: []byte("2")},
	}
	if err := EncodeLeaf(buf, cfg, 16, entries); err == nil {
		t.Error("expected error on unsorted entries")
	}
}

func TestLeafRejectsDuplicateKeys(t *testing.T) {
	// EncodeLeaf rejects non-strictly-ascending entries: equal keys
	// surface as the same error path as out-of-order.
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	entries := []EncodedEntry{
		{Key: []byte("k"), Value: []byte("1")},
		{Key: []byte("k"), Value: []byte("2")},
	}
	if err := EncodeLeaf(buf, cfg, 16, entries); err == nil {
		t.Error("expected error on duplicate keys")
	}
}

func TestLeafRejectsUnknownCellFlags(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	entries := []EncodedEntry{
		{Key: []byte("k"), Value: []byte("v"), Flags: 1 << 5}, // reserved bit
	}
	if err := EncodeLeaf(buf, cfg, 16, entries); err == nil {
		t.Error("expected error on unknown CellFlags bits")
	}
}

func TestLeafRejectsOverflowWithValue(t *testing.T) {
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	entries := []EncodedEntry{
		{
			Key:          []byte("k"),
			Flags:        CellFlagOverflow,
			Value:        []byte("nope"),
			OverflowPage: 1,
			TotalLen:     100,
		},
	}
	if err := EncodeLeaf(buf, cfg, 16, entries); err == nil {
		t.Error("expected error: Overflow flag with non-empty Value")
	}
}

func TestDecodeLeafRejectsCorruptedKeyLen(t *testing.T) {
	// Decoder robustness contract (M1 from the 4.2 adversarial
	// review): a leaf page with a forged restart-entry KeyLen
	// must surface as an error from DecodeLeaf, NOT a slice-
	// out-of-bounds panic. Without this property, Check() —
	// which calls DecodeLeaf on potentially-corrupt on-disk pages
	// (PageChecksum=false is a supported configuration) — would
	// crash instead of reporting the bad page.
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	entries := []EncodedEntry{
		{Key: []byte("k"), Value: []byte("v")},
	}
	if err := EncodeLeaf(buf, cfg, 16, entries); err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Restart entry at offset 12: [Flags u8][KeyLen u16][ValueLen u32][Key][Val].
	// KeyLen lives at offset 13..14. Forge to 0xFFFF.
	le.PutUint16(buf[13:], 0xFFFF)
	_, err := DecodeLeaf(buf, cfg)
	if err == nil {
		t.Error("DecodeLeaf accepted forged KeyLen 0xFFFF; expected error")
	}
}

func TestDecodeLeafRejectsForgedRestartCount(t *testing.T) {
	// Decoder robustness contract (M3 from the 4.2 adversarial
	// review + spec invariant 3): DecodeLeaf cross-checks the
	// per-page RestartCount against ceil(N/K). A forged value
	// mislocates the restart table and would let
	// LeafRestartOffset return offsets into the entries region,
	// producing silently-wrong lookup results.
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	entries := []EncodedEntry{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
		{Key: []byte("c"), Value: []byte("3")},
		{Key: []byte("d"), Value: []byte("4")},
	}
	if err := EncodeLeaf(buf, cfg, 2, entries); err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Correct rc = ceil(4/2) = 2. Forge to 1.
	le.PutUint16(buf[leafRestartCountOff:], 1)
	_, err := DecodeLeaf(buf, cfg)
	if err == nil {
		t.Error("DecodeLeaf accepted forged RestartCount; expected error")
	}
}

func TestLeafEncodedSizeMatchesEncodedConsumption(t *testing.T) {
	// LeafEncodedSize predicts the bytes a leaf will consume
	// (header + per-entry bytes + restart-table bytes). Verify the
	// prediction against the ACTUAL on-disk encoding by walking
	// decodeLeafEntry's returned `next` offsets — that exercises
	// a different code path (the decoder) than the predictor
	// (which uses leafEntrySize). A bug in either surfaces as a
	// mismatch.
	cfg := Config{PageSize: 4096}
	buf := newLeafPage(t, cfg)
	entries := []EncodedEntry{
		{Key: []byte("aaa-1"), Value: []byte("v1")},
		{Key: []byte("aaa-2"), Value: []byte("v2")},
		{Key: []byte("aaa-3"), Value: []byte("v3")},
		{Key: []byte("aaa-4"), Value: []byte("v4")},
		{Key: []byte("aaa-5"), Value: []byte("v5")},
	}
	predicted := LeafEncodedSize(cfg, 16, entries)
	if err := EncodeLeaf(buf, cfg, 16, entries); err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Walk the actually-encoded entries via the decoder; the
	// final `pos` is the byte offset directly after the last
	// entry — i.e., the start of free space.
	rc := LeafRestartCount(buf)
	tableStart := leafRestartTableStart(cfg, rc)
	pos := leafHeaderEnd
	var prev []byte
	for i := range entries {
		isRestart := i%16 == 0
		var p []byte
		if !isRestart {
			p = prev
		}
		entry, next, err := decodeLeafEntry(buf, pos, tableStart, isRestart, p)
		if err != nil {
			t.Fatalf("decode entry %d at off=%d: %v", i, pos, err)
		}
		prev = entry.Key
		pos = next
	}
	encodedConsumed := pos + int(rc)*leafRestartTableEntrySize
	if predicted != encodedConsumed {
		t.Errorf("LeafEncodedSize=%d, on-disk consumption=%d", predicted, encodedConsumed)
	}
}

package page

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"
)

// contiguousRun concatenates an encoded run's pages into the committed
// on-disk image (runs are physically contiguous — page-formats.md
// §Overflow Page).
func contiguousRun(pages [][]byte) []byte {
	var run []byte
	for _, p := range pages {
		run = append(run, p...)
	}
	return run
}

func encodeRun(t *testing.T, cfg Config, value []byte) [][]byte {
	t.Helper()
	n := int(OverflowRunLength(cfg, uint64(len(value))))
	pages := make([][]byte, n)
	for i := range n {
		// Pre-fill with garbage: production slab buffers are pool-
		// recycled, so zero slack must come from the ENCODER (clear),
		// never from a coincidentally-fresh buffer.
		pages[i] = bytes.Repeat([]byte{0xFF}, int(cfg.PageSize))
	}
	if err := EncodeOverflowRun(pages, cfg, value); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return pages
}

func TestOverflowRunLengthBoundaries(t *testing.T) {
	for _, withChecksum := range []bool{false, true} {
		t.Run(fmt.Sprintf("checksum=%v", withChecksum), func(t *testing.T) {
			cfg := Config{PageSize: 4096, PageChecksum: withChecksum}
			firstCap := OverflowFirstPageCapacity(cfg) // 4096-8 plain, 4096-16 checksummed
			follower := OverflowFollowerCapacity(cfg)  // 4096 (no footer either way)
			wantFirst := 4096 - 8
			if withChecksum {
				wantFirst = 4096 - 16
			}
			if firstCap != wantFirst {
				t.Fatalf("first-page capacity = %d, want %d (page-formats.md §Overflow Page)", firstCap, wantFirst)
			}
			if follower != 4096 {
				t.Fatalf("follower capacity = %d, want %d (followers carry no header/footer)", follower, 4096)
			}
			cases := []struct {
				valLen uint64
				want   uint32
			}{
				{0, 1},
				{1, 1},
				{uint64(firstCap), 1},
				{uint64(firstCap) + 1, 2},
				{uint64(firstCap) + uint64(follower), 2},
				{uint64(firstCap) + uint64(follower) + 1, 3},
				{uint64(firstCap) + 2*uint64(follower), 3},
				{uint64(firstCap) + 2*uint64(follower) + 1, 4},
			}
			for _, c := range cases {
				got := OverflowRunLength(cfg, c.valLen)
				if got != c.want {
					t.Errorf("len=%d: got %d, want %d", c.valLen, got, c.want)
				}
			}
		})
	}
}

func TestOverflowRoundTripSinglePage(t *testing.T) {
	cfg := Config{PageSize: 4096}
	value := bytes.Repeat([]byte("x"), 100)
	pages := encodeRun(t, cfg, value)
	if len(pages) != 1 {
		t.Fatalf("expected 1-page run for 100-byte value, got %d", len(pages))
	}
	run := contiguousRun(pages)
	got := OverflowRunExtent(run, cfg)[:len(value)]
	if !bytes.Equal(got, value) {
		t.Errorf("round-trip mismatch")
	}
}

func TestOverflowRoundTripMultiPage(t *testing.T) {
	// Spec (page-formats.md §Overflow Page): an overflow run of 1+N
	// pages stores (PageSize-16) + N*PageSize extent bytes with
	// PageChecksum, (PageSize-8) + N*PageSize without — followers are
	// footer-free either way, the extent is one contiguous range, and
	// with checksums the whole-run digest verifies the image
	// standalone. Round-trip a value that crosses page boundaries to
	// pin the accounting.
	for _, withChecksum := range []bool{false, true} {
		t.Run(fmt.Sprintf("checksum=%v", withChecksum), func(t *testing.T) {
			cfg := Config{PageSize: 4096, PageChecksum: withChecksum}
			valLen := OverflowFirstPageCapacity(cfg) + OverflowFollowerCapacity(cfg) + 500
			value := make([]byte, valLen)
			if _, err := rand.Read(value); err != nil {
				t.Fatalf("rand: %v", err)
			}
			pages := encodeRun(t, cfg, value)
			if len(pages) != 3 {
				t.Fatalf("expected 3-page run, got %d", len(pages))
			}
			additional, err := DecodeOverflowFirstPage(pages[0])
			if err != nil {
				t.Fatalf("decode first: %v", err)
			}
			if int(additional) != 2 {
				t.Errorf("AdditionalPages = %d, want 2", additional)
			}
			run := contiguousRun(pages)
			if !VerifyOverflowRun(run, cfg) {
				t.Fatalf("freshly-encoded run fails whole-run digest verification")
			}
			extent := OverflowRunExtent(run, cfg)
			if !bytes.Equal(extent[:valLen], value) {
				t.Errorf("round-trip mismatch")
			}
			// Slack past the extent is zero on write — unconditionally
			// (page-formats.md §Overflow Page: a run image is a pure
			// function of its extent bytes).
			for i, b := range extent[valLen:] {
				if b != 0 {
					t.Fatalf("slack byte %d past extent is 0x%02x, want 0", i, b)
				}
			}
		})
	}
}

// TestOverflowRunDigestCoversFullContentRange (checksums.md
// §Overflow-Run Digest): the digest is computed over the FULL
// AdditionalPages-determined content range — a flipped bit anywhere in
// the extent, in a follower, or even in the slack past the extent
// length fails verification; the header and digest field themselves
// are outside the covered range.
func TestOverflowRunDigestCoversFullContentRange(t *testing.T) {
	cfg := Config{PageSize: 4096, PageChecksum: true}
	valLen := OverflowFirstPageCapacity(cfg) + 700 // 2-page run, follower half-slack
	value := make([]byte, valLen)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("rand: %v", err)
	}
	pages := encodeRun(t, cfg, value)
	base := contiguousRun(pages)
	if !VerifyOverflowRun(base, cfg) {
		t.Fatalf("clean run fails verification")
	}
	start := OverflowHeadContentStart(cfg)
	corrupt := []struct {
		name string
		off  int
	}{
		{"head extent first byte", start},
		{"head extent last byte", 4095},
		{"follower extent byte", 4096 + 100},
		{"follower slack byte", 4096 + 700 + 100}, // past the extent length
		{"run's final byte", 2*4096 - 1},
	}
	for _, c := range corrupt {
		run := bytes.Clone(base)
		run[c.off] ^= 0x01
		if VerifyOverflowRun(run, cfg) {
			t.Errorf("%s (offset %d): corruption not detected — digest must cover the full content range", c.name, c.off)
		}
	}
	// The stored digest field itself: flipping it must also fail (the
	// comparison is against the recomputed value).
	run := bytes.Clone(base)
	run[HeaderSize] ^= 0x01
	if VerifyOverflowRun(run, cfg) {
		t.Errorf("flipped stored digest not detected")
	}
}

// TestOverflowRunDigestAbsentWithoutChecksum (checksums.md
// §Overflow-Run Digest): with checksums disabled the digest field is
// absent — extent bytes start at head offset 8 and verification is an
// unconditional pass.
func TestOverflowRunDigestAbsentWithoutChecksum(t *testing.T) {
	cfg := Config{PageSize: 4096}
	if got := OverflowHeadContentStart(cfg); got != HeaderSize {
		t.Fatalf("content start = %d, want %d", got, HeaderSize)
	}
	value := bytes.Repeat([]byte("y"), 300)
	run := contiguousRun(encodeRun(t, cfg, value))
	// Byte 8 is the first extent byte, not a digest slot.
	if run[HeaderSize] != 'y' {
		t.Fatalf("extent does not start at offset 8 with checksums off")
	}
	run[HeaderSize+1] ^= 0x01
	if !VerifyOverflowRun(run, cfg) {
		t.Errorf("VerifyOverflowRun must pass unconditionally with checksums disabled")
	}
}

// TestOverflowStreamedDigestMatchesOneShot: SetOverflowRunDigest with a
// caller-streamed hash (the bulk-load slab-bypass writer) must be
// byte-identical to EncodeOverflowRun's output — one on-disk form, no
// writer drift.
func TestOverflowStreamedDigestMatchesOneShot(t *testing.T) {
	cfg := Config{PageSize: 4096, PageChecksum: true}
	valLen := OverflowFirstPageCapacity(cfg) + 2*OverflowFollowerCapacity(cfg) - 37
	value := make([]byte, valLen)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("rand: %v", err)
	}
	run := contiguousRun(encodeRun(t, cfg, value))
	if got, want := StoredOverflowRunDigest(run), OverflowRunDigest(run, cfg); got != want {
		t.Fatalf("stored digest %x != recomputed one-shot %x", got, want)
	}
}

func TestOverflowEncodeRejectsWrongPageCount(t *testing.T) {
	cfg := Config{PageSize: 4096}
	value := bytes.Repeat([]byte("x"), 100) // 1-page run
	pages := make([][]byte, 2)              // wrong: 2 supplied
	for i := range pages {
		pages[i] = make([]byte, cfg.PageSize)
	}
	if err := EncodeOverflowRun(pages, cfg, value); err == nil {
		t.Error("expected error on wrong page count")
	}
}

func TestOverflowFirstPageTypeIsOverflow(t *testing.T) {
	cfg := Config{PageSize: 4096}
	value := []byte("small")
	pages := [][]byte{make([]byte, cfg.PageSize)}
	if err := EncodeOverflowRun(pages, cfg, value); err != nil {
		t.Fatalf("encode: %v", err)
	}
	typ, _, _, _ := ReadHeader(pages[0])
	if typ != TypeOverflow {
		t.Errorf("type = %d, want %d (TypeOverflow)", typ, TypeOverflow)
	}
}

// TestOverflowRunLength64NoTruncation (Inv-RV4): the uint64 run length
// must not truncate the way OverflowRunLength (uint32) does — a forged
// on-disk TotalLen whose true run exceeds uint32 is exactly what made a
// naive run-vs-extent guard pass while the value allocation was enormous.
func TestOverflowRunLength64NoTruncation(t *testing.T) {
	cfg := Config{PageSize: 4096}
	// Small valid values: the two forms agree.
	for _, v := range []uint64{0, 1, 100, 4088, 4089, 100000} {
		if got, want := OverflowRunLength64(cfg, v), uint64(OverflowRunLength(cfg, v)); got != want {
			t.Errorf("OverflowRunLength64(%d) = %d, OverflowRunLength = %d, want equal", v, got, want)
		}
	}
	// A forged ~1 PB TotalLen: its run length exceeds uint32, so the
	// uint32 form truncates while the uint64 form does not, and the uint64
	// value equals ceil over the follower cap.
	const forged = uint64(1) << 50
	first := uint64(OverflowFirstPageCapacity(cfg))
	follower := uint64(OverflowFollowerCapacity(cfg))
	want := 1 + (forged-first+follower-1)/follower
	got := OverflowRunLength64(cfg, forged)
	if got != want {
		t.Errorf("OverflowRunLength64(%d) = %d, want %d", forged, got, want)
	}
	if got <= uint64(^uint32(0)) {
		t.Fatalf("premise broken: run %d fits uint32; cannot demonstrate truncation", got)
	}
	if uint64(OverflowRunLength(cfg, forged)) == got {
		t.Errorf("OverflowRunLength did not truncate (got %d == %d); the uint64 variant would be redundant",
			OverflowRunLength(cfg, forged), got)
	}
}

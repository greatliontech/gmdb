package page

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"
)

func TestOverflowRunLengthBoundaries(t *testing.T) {
	cfg := Config{PageSize: 4096}
	firstCap := OverflowFirstPageCapacity(cfg) // 4096 - 8 = 4088
	follower := OverflowFollowerCapacity(cfg)  // 4096
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
}

func TestOverflowRoundTripSinglePage(t *testing.T) {
	cfg := Config{PageSize: 4096}
	value := bytes.Repeat([]byte("x"), 100)
	n := int(OverflowRunLength(cfg, uint64(len(value))))
	if n != 1 {
		t.Fatalf("expected 1-page run for 100-byte value, got %d", n)
	}
	pages := make([][]byte, 1)
	pages[0] = make([]byte, cfg.PageSize)
	if err := EncodeOverflowRun(pages, cfg, value); err != nil {
		t.Fatalf("encode: %v", err)
	}
	dst := make([]byte, len(value))
	got, err := AssembleOverflowValue(pages, cfg, dst)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if got != len(value) || !bytes.Equal(dst, value) {
		t.Errorf("round-trip mismatch (got %d bytes)", got)
	}
}

func TestOverflowRoundTripMultiPage(t *testing.T) {
	// Spec invariant 4 (page-formats.md §Overflow Page): an overflow
	// run of 1+N pages stores (PageSize-8) + N*PageSize bytes of
	// value (with footers, subtract another 8 per page). Round-trip
	// a value that crosses page boundaries to pin the run-length
	// accounting.
	for _, withChecksum := range []bool{false, true} {
		t.Run(fmt.Sprintf("checksum=%v", withChecksum), func(t *testing.T) {
			cfg := Config{PageSize: 4096, PageChecksum: withChecksum}
			// Value that spans 3 pages.
			valLen := OverflowFirstPageCapacity(cfg) + OverflowFollowerCapacity(cfg) + 500
			value := make([]byte, valLen)
			if _, err := rand.Read(value); err != nil {
				t.Fatalf("rand: %v", err)
			}
			n := int(OverflowRunLength(cfg, uint64(valLen)))
			if n != 3 {
				t.Fatalf("expected 3-page run, got %d", n)
			}
			pages := make([][]byte, n)
			for i := range n {
				pages[i] = make([]byte, cfg.PageSize)
			}
			if err := EncodeOverflowRun(pages, cfg, value); err != nil {
				t.Fatalf("encode: %v", err)
			}
			// First-page header populated correctly.
			additional, err := DecodeOverflowFirstPage(pages[0])
			if err != nil {
				t.Fatalf("decode first: %v", err)
			}
			if int(additional) != n-1 {
				t.Errorf("AdditionalPages = %d, want %d", additional, n-1)
			}
			dst := make([]byte, valLen)
			got, err := AssembleOverflowValue(pages, cfg, dst)
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			if got != valLen || !bytes.Equal(dst, value) {
				t.Errorf("round-trip mismatch: got %d bytes", got)
			}
		})
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

func TestOverflowAssembleRejectsHeaderMismatch(t *testing.T) {
	cfg := Config{PageSize: 4096}
	value := bytes.Repeat([]byte("x"), OverflowFirstPageCapacity(cfg)+10) // 2-page run
	pages := make([][]byte, 2)
	for i := range pages {
		pages[i] = make([]byte, cfg.PageSize)
	}
	if err := EncodeOverflowRun(pages, cfg, value); err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Hand assemble only the first page — should error.
	_, err := AssembleOverflowValue(pages[:1], cfg, make([]byte, len(value)))
	if err == nil {
		t.Error("expected error on truncated pages slice")
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

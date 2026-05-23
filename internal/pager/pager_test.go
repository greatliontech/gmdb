package pager

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

const testPageSize = 4096

func makeFile(t *testing.T, pages int) (*os.File, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Pre-fill with deterministic content: byte i = byte(i & 0xFF) for
	// each page; page p has bytes [p%256, (p+1)%256, ...].
	buf := make([]byte, testPageSize)
	for p := range pages {
		for i := range buf {
			buf[i] = byte((p*7 + i) & 0xFF)
		}
		if _, err := f.WriteAt(buf, int64(p)*testPageSize); err != nil {
			t.Fatalf("write page %d: %v", p, err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return f, path
}

func expectedPageBytes(p int) []byte {
	buf := make([]byte, testPageSize)
	for i := range buf {
		buf[i] = byte((p*7 + i) & 0xFF)
	}
	return buf
}

func TestReaderResolvesFromMmap(t *testing.T) {
	f, _ := makeFile(t, 4)
	defer f.Close()

	cfg := page.Config{PageSize: testPageSize, PageChecksum: false}
	p, err := NewReader(f, cfg, 4*testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer p.Close()

	for i := range 4 {
		got := p.Page(uint64(i))
		want := expectedPageBytes(i)
		if !bytes.Equal(got, want) {
			t.Errorf("page %d mismatch", i)
		}
	}
}

func TestReaderRejectsMutations(t *testing.T) {
	f, _ := makeFile(t, 2)
	defer f.Close()
	p, err := NewReader(f, page.Config{PageSize: testPageSize}, 2*testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer p.Close()

	if _, err := p.CoW(0, 1); !errors.Is(err, ErrReadOnly) {
		t.Errorf("CoW: got %v, want ErrReadOnly", err)
	}
	if _, err := p.AllocSlab(1); !errors.Is(err, ErrReadOnly) {
		t.Errorf("AllocSlab: got %v, want ErrReadOnly", err)
	}
	if _, err := p.Mutate(0); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Mutate: got %v, want ErrReadOnly", err)
	}
	if p.IsDirty(0) {
		t.Error("IsDirty on read-only pager = true")
	}
	if p.DirtyBytes() != 0 {
		t.Errorf("DirtyBytes = %d, want 0", p.DirtyBytes())
	}
	if len(p.DirtyIDs()) != 0 {
		t.Errorf("DirtyIDs len = %d, want 0", len(p.DirtyIDs()))
	}
	// Discard is a silent no-op on read-only.
	p.Discard(0)
	// ReleaseAll same.
	p.ReleaseAll()
}

func TestWriterCoWAndMutate(t *testing.T) {
	f, _ := makeFile(t, 8)
	defer f.Close()
	pool := NewBufPool(testPageSize)
	cfg := page.Config{PageSize: testPageSize}

	p, err := NewWriter(f, cfg, 8*testPageSize, pool, 1<<20)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()

	// CoW page 2 to a fresh destination ID 5.
	buf, err := p.CoW(2, 5)
	if err != nil {
		t.Fatalf("CoW: %v", err)
	}
	want := expectedPageBytes(2)
	if !bytes.Equal(buf, want) {
		t.Fatal("CoW buffer does not match source content")
	}

	// Page(5) now resolves to the slab buffer.
	got := p.Page(5)
	if !bytes.Equal(got, want) {
		t.Fatal("Page(5) does not match CoW buffer")
	}
	if !p.IsDirty(5) {
		t.Fatal("IsDirty(5) = false after CoW")
	}

	// Mutate page 5 — write a sentinel byte.
	buf, err = p.Mutate(5)
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	buf[0] = 0xAB
	if p.Page(5)[0] != 0xAB {
		t.Fatal("mutation not visible via Page()")
	}

	// Page(2) still resolves via mmap, unchanged.
	got = p.Page(2)
	if !bytes.Equal(got, expectedPageBytes(2)) {
		t.Fatal("Page(2) mutated despite CoW going to id 5")
	}

	if p.DirtyBytes() != testPageSize {
		t.Errorf("DirtyBytes = %d, want %d", p.DirtyBytes(), testPageSize)
	}
	ids := p.DirtyIDs()
	if len(ids) != 1 || ids[0] != 5 {
		t.Errorf("DirtyIDs = %v, want [5]", ids)
	}
}

func TestWriterCoWIdempotent(t *testing.T) {
	f, _ := makeFile(t, 4)
	defer f.Close()
	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, page.Config{PageSize: testPageSize}, 4*testPageSize, pool, 1<<20)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()

	buf1, err := p.CoW(0, 2)
	if err != nil {
		t.Fatalf("first CoW: %v", err)
	}
	buf1[100] = 0x42
	buf2, err := p.CoW(0, 2)
	if err != nil {
		t.Fatalf("re-CoW: %v", err)
	}
	// Must return the same buffer — same backing memory — and dirtyBytes
	// must not double.
	if &buf1[0] != &buf2[0] {
		t.Fatal("re-CoW returned a different buffer")
	}
	if buf2[100] != 0x42 {
		t.Fatal("re-CoW lost prior mutation")
	}
	if p.DirtyBytes() != testPageSize {
		t.Errorf("DirtyBytes = %d, want %d (re-CoW must not double)", p.DirtyBytes(), testPageSize)
	}
}

func TestWriterMutateRejectsClean(t *testing.T) {
	f, _ := makeFile(t, 2)
	defer f.Close()
	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, page.Config{PageSize: testPageSize}, 2*testPageSize, pool, 1<<20)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()

	if _, err := p.Mutate(0); !errors.Is(err, ErrPageNotDirty) {
		t.Errorf("Mutate clean: got %v, want ErrPageNotDirty", err)
	}
}

func TestWriterMaxTxBufferBytes(t *testing.T) {
	f, _ := makeFile(t, 8)
	defer f.Close()
	pool := NewBufPool(testPageSize)
	cfg := page.Config{PageSize: testPageSize}

	// Budget = 2 pages.
	p, err := NewWriter(f, cfg, 8*testPageSize, pool, 2*testPageSize)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()

	if _, err := p.CoW(0, 4); err != nil {
		t.Fatalf("CoW 1: %v", err)
	}
	if _, err := p.CoW(1, 5); err != nil {
		t.Fatalf("CoW 2: %v", err)
	}
	if _, err := p.CoW(2, 6); !errors.Is(err, ErrTxTooLarge) {
		t.Errorf("CoW 3: got %v, want ErrTxTooLarge", err)
	}
	if got := p.DirtyBytes(); got != 2*testPageSize {
		t.Errorf("DirtyBytes after ErrTxTooLarge = %d, want %d", got, 2*testPageSize)
	}
}

func TestWriterAllocSlabBudget(t *testing.T) {
	f, _ := makeFile(t, 4)
	defer f.Close()
	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, page.Config{PageSize: testPageSize}, 4*testPageSize, pool, testPageSize)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()

	buf, err := p.AllocSlab(7)
	if err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	if len(buf) != testPageSize {
		t.Errorf("AllocSlab buf len = %d, want %d", len(buf), testPageSize)
	}
	// Buffer is zero-filled (from pool's clear-on-Put + fresh allocation).
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("AllocSlab buf[%d] = %d, want 0", i, b)
		}
	}
	// Second alloc would exceed budget.
	if _, err := p.AllocSlab(8); !errors.Is(err, ErrTxTooLarge) {
		t.Errorf("second AllocSlab: got %v, want ErrTxTooLarge", err)
	}
	// AllocSlab is idempotent on existing dirty IDs.
	buf2, err := p.AllocSlab(7)
	if err != nil {
		t.Fatalf("re-AllocSlab: %v", err)
	}
	if &buf2[0] != &buf[0] {
		t.Fatal("re-AllocSlab returned a different buffer")
	}
}

func TestWriterDiscard(t *testing.T) {
	f, _ := makeFile(t, 4)
	defer f.Close()
	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, page.Config{PageSize: testPageSize}, 4*testPageSize, pool, 4*testPageSize)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()

	if _, err := p.CoW(0, 2); err != nil {
		t.Fatalf("CoW: %v", err)
	}
	if p.DirtyBytes() != testPageSize {
		t.Fatalf("DirtyBytes = %d", p.DirtyBytes())
	}
	p.Discard(2)
	if p.IsDirty(2) {
		t.Fatal("IsDirty(2) after Discard")
	}
	if p.DirtyBytes() != 0 {
		t.Errorf("DirtyBytes after Discard = %d, want 0", p.DirtyBytes())
	}
	// Discarding a non-existent id is a no-op.
	p.Discard(99)
}

func TestWriterReleaseAll(t *testing.T) {
	f, _ := makeFile(t, 8)
	defer f.Close()
	pool := NewBufPool(testPageSize)
	p, err := NewWriter(f, page.Config{PageSize: testPageSize}, 8*testPageSize, pool, 4*testPageSize)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer p.Close()

	for i := uint64(0); i < 3; i++ {
		if _, err := p.CoW(i, 4+i); err != nil {
			t.Fatalf("CoW: %v", err)
		}
	}
	p.ReleaseAll()
	if p.DirtyBytes() != 0 {
		t.Errorf("DirtyBytes after ReleaseAll = %d, want 0", p.DirtyBytes())
	}
	if len(p.DirtyIDs()) != 0 {
		t.Error("DirtyIDs not empty after ReleaseAll")
	}
	// Idempotent.
	p.ReleaseAll()
}

func TestPageOutOfRangePanics(t *testing.T) {
	f, _ := makeFile(t, 2)
	defer f.Close()
	p, err := NewReader(f, page.Config{PageSize: testPageSize}, 2*testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer p.Close()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic")
		}
	}()
	_ = p.Page(100)
}

func TestNewReaderRejectsInvalidConfig(t *testing.T) {
	f, _ := makeFile(t, 1)
	defer f.Close()

	if _, err := NewReader(f, page.Config{PageSize: 3000}, testPageSize); err == nil {
		t.Error("NewReader accepted invalid PageSize")
	}
	if _, err := NewReader(f, page.Config{PageSize: testPageSize}, 0); err == nil {
		t.Error("NewReader accepted zero reservation")
	}
}

func TestNewWriterRejectsBadInputs(t *testing.T) {
	f, _ := makeFile(t, 1)
	defer f.Close()
	cfg := page.Config{PageSize: testPageSize}

	if _, err := NewWriter(f, cfg, testPageSize, nil, 1<<20); err == nil {
		t.Error("NewWriter accepted nil pool")
	}
	wrongPool := NewBufPool(8192)
	if _, err := NewWriter(f, cfg, testPageSize, wrongPool, 1<<20); err == nil {
		t.Error("NewWriter accepted pool with mismatched page size")
	}
	if _, err := NewWriter(f, cfg, testPageSize, NewBufPool(testPageSize), 0); err == nil {
		t.Error("NewWriter accepted zero maxBytes")
	}
}

func TestCloseIdempotent(t *testing.T) {
	f, _ := makeFile(t, 1)
	defer f.Close()
	p, err := NewReader(f, page.Config{PageSize: testPageSize}, testPageSize)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestBufPoolClearsOnPut(t *testing.T) {
	pool := NewBufPool(testPageSize)
	buf := pool.Get()
	(*buf)[0] = 0xAB
	pool.Put(buf)
	// sync.Pool may or may not return the same buffer; loop a few times.
	for range 10 {
		next := pool.Get()
		if (*next)[0] != 0 {
			t.Fatalf("recycled buffer not cleared: byte 0 = %d", (*next)[0])
		}
		pool.Put(next)
	}
}

func TestBufPoolRejectsWrongSize(t *testing.T) {
	pool := NewBufPool(testPageSize)
	wrong := make([]byte, 100)
	pool.Put(&wrong) // dropped silently
	got := pool.Get()
	if len(*got) != testPageSize {
		t.Errorf("Get returned len %d, want %d", len(*got), testPageSize)
	}
}

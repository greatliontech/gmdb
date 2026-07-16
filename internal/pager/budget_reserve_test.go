package pager

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greatliontech/gmdb/internal/bitmap"
	"github.com/greatliontech/gmdb/internal/page"
)

// setupWriterMaxBytes is setupWriter with a caller-chosen slab budget,
// for tests that exercise the budget admission math directly.
func setupWriterMaxBytes(t *testing.T, pages, maxBytes int) (*Pager, *bitmap.Bitmap, *os.File) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := f.Truncate(int64(pages) * int64(testPageSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	pool := NewBufPool(testPageSize)
	cfg := page.Config{PageSize: testPageSize}
	p, err := NewWriter(f, cfg, int64(pages)*int64(testPageSize), pool, maxBytes)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	bm := bitmap.New(make([]byte, testPageSize), testPageSize, 1, uint64(pages))
	p.AttachBitmap(bm)
	p.SetCommitState(bm.FirstDataPage(), uint64(pages), 0)
	return p, bm, f
}

// TestRetireBudgetGuard pins the FreePage retire-branch admission
// check: retiring prior-tx pages grows the commit-time RPL segment
// projection, and the retire that would make the transaction unable
// to afford its own commit fails ErrTxTooLarge instead of deferring
// the failure to Commit. With maxBytes = 2 pages and zero dirty
// bytes, the reserve may grow to exactly 2 segment pages; the retire
// opening a third segment must be rejected.
func TestRetireBudgetGuard(t *testing.T) {
	const maxPages = 2
	p, _, f := setupWriterMaxBytes(t, 32, maxPages*int(testPageSize))
	defer p.Close()
	defer f.Close()

	cfg := page.Config{PageSize: testPageSize}
	capPerSeg := RPLEntriesPerSegment(cfg)
	if capPerSeg <= 0 {
		t.Fatalf("RPLEntriesPerSegment = %d", capPerSeg)
	}

	// Prior-tx page IDs: anything not in dirty/pendingAllocs. IDs need
	// not be backed by file content — FreePage only does bookkeeping.
	next := uint64(1 << 20)
	retire := func() error {
		next++
		return p.FreePage(next)
	}

	// Two segments' worth of retires fit: reserve reaches exactly
	// maxBytes with dirtyBytes = 0.
	for i := 0; i < 2*capPerSeg; i++ {
		if err := retire(); err != nil {
			t.Fatalf("retire %d: %v (reserve %d)", i, err, p.CommitReserveBytes())
		}
	}
	if got, want := p.CommitReserveBytes(), maxPages*int(testPageSize); got != want {
		t.Fatalf("reserve after 2 segments = %d, want %d", got, want)
	}

	// The retire opening the third segment must fail, leaving the
	// retired set unchanged.
	before := len(p.RetiredPages())
	if err := retire(); !errors.Is(err, ErrTxTooLarge) {
		t.Fatalf("third-segment retire: err = %v, want ErrTxTooLarge", err)
	}
	if after := len(p.RetiredPages()); after != before {
		t.Fatalf("failed retire mutated retiredPages: %d -> %d", before, after)
	}

	// Ops-phase installs never budget-reject (spill-threshold
	// semantics): the alloc succeeds even with the reserve at the
	// full budget. Discard it so the commit-phase raw-cap assertions
	// below start from zero dirty bytes.
	if _, err := p.AllocSlab(3); err != nil {
		t.Fatalf("AllocSlab under full reserve: err = %v, want success (spill threshold)", err)
	}
	p.Discard(3)

	// Commit-phase admission draws from the reserve: the same
	// allocation succeeds with the commit flag set, up to the raw cap.
	p.SetCommitPhase(true)
	defer p.SetCommitPhase(false)
	if _, err := p.AllocSlab(3); err != nil {
		t.Fatalf("AllocSlab in commit phase: %v", err)
	}
	if _, err := p.AllocSlab(4); err != nil {
		t.Fatalf("second commit-phase AllocSlab: %v", err)
	}
	// Raw cap remains the backstop.
	if _, err := p.AllocSlab(5); !errors.Is(err, ErrTxTooLarge) {
		t.Fatalf("commit-phase AllocSlab past raw cap: err = %v, want ErrTxTooLarge", err)
	}
}

// TestRetireGuardIgnoresSpillableDirtyBytes: the retire guard bounds
// the RESERVE (RPL segments cannot spill) — live dirtyBytes must not
// be charged, because data pages spill at operation boundaries and
// freed buffers drop at step 0. A retire with the slab far past the
// threshold but a tiny reserve must succeed.
func TestRetireGuardIgnoresSpillableDirtyBytes(t *testing.T) {
	const maxPages = 2
	p, _, f := setupWriterMaxBytes(t, 32, maxPages*int(testPageSize))
	defer p.Close()
	defer f.Close()

	// Slab far past the threshold (no savepoints, so nothing spills).
	for i := range 6 {
		id, err := p.AllocPage()
		if err != nil {
			t.Fatalf("AllocPage %d: %v", i, err)
		}
		if _, err := p.AllocSlab(id); err != nil {
			t.Fatalf("AllocSlab %d: %v", i, err)
		}
	}
	if p.DirtyBytes() <= maxPages*int(testPageSize) {
		t.Fatal("fixture: dirty not past the threshold")
	}
	// A prior-tx retire opening the first RPL segment: reserve (1
	// page) + PageSize fits the 2-page budget regardless of dirty.
	if err := p.FreePage(1 << 20); err != nil {
		t.Fatalf("retire with spillable dirty past threshold: %v (dirtyBytes must not be charged)", err)
	}
}

// TestSpillRestoreAccounting: dirtyBytes is recomputed from the held
// buffer sets at savepoint restore — a spill inside a nested window
// legitimately shrinks the set below its Begin-time size, so a
// Begin-time scalar would over-count forever after.
func TestSpillRestoreAccounting(t *testing.T) {
	p, _, f := setupWriterMaxBytes(t, 64, 8*int(testPageSize))
	defer p.Close()
	defer f.Close()

	// Pre-window pages (spill-eligible), under the 8-page threshold
	// so the shallow release below does not spill yet.
	for range 4 {
		id, err := p.AllocPage()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.AllocSlab(id); err != nil {
			t.Fatal(err)
		}
	}
	// A detached buffer: free one page (loose), then loose-pop it via
	// a shallow window's alloc and re-CoW — the original buffer moves
	// to detachedBufs, where it must stay COUNTED by the accounting.
	firstID, err := p.AllocPage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.AllocSlab(firstID); err != nil {
		t.Fatal(err)
	}
	if err := p.FreePage(firstID); err != nil {
		t.Fatal(err)
	}
	ssp := p.BeginShallowSavepoint()
	popped, err := p.AllocPage() // loose-pop: detaches the buffer
	if err != nil {
		t.Fatal(err)
	}
	if popped != firstID {
		t.Fatalf("fixture: expected loose-pop of %d, got %d", firstID, popped)
	}
	if _, err := p.AllocSlab(popped); err != nil {
		t.Fatal(err)
	}
	p.ReleaseSavepoint(ssp)
	if p.HeldBufferCountForTest() == len(p.DirtyIDs()) {
		t.Fatal("fixture: no detached buffer present")
	}

	sp := p.BeginSavepoint() // nested window
	// In-window installs past the threshold, then the spill.
	before := p.DirtyBytes()
	for range 6 {
		id, err := p.AllocPage()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.AllocSlab(id); err != nil {
			t.Fatal(err)
		}
	}
	p.SpillExcess()
	if p.DirtyBytes() >= before+6*int(testPageSize) {
		t.Fatalf("spill did not run inside the nested window: dirty %d", p.DirtyBytes())
	}
	p.RestoreSavepoint(sp)
	// The invariant: dirtyBytes == held buffers × PageSize.
	want := p.HeldBufferCountForTest() * int(testPageSize)
	if got := p.DirtyBytes(); got != want {
		t.Fatalf("post-restore dirtyBytes = %d, want %d (held buffers × PageSize)", got, want)
	}
}

// TestSpillNeverPoolRecycles pins the byte-slice ownership invariant
// on both slab-exit paths the spill pass owns (pager-slab.md, the
// never-POOL-recycled invariant): a buffer leaving the slab via the
// SPILL pwrite or via the loose-DROP goes to the garbage collector —
// pool-recycling would zero-fill it under every borrowed []byte.
func TestSpillNeverPoolRecycles(t *testing.T) {
	p, _, f := setupWriterMaxBytes(t, 64, 2*int(testPageSize))
	defer p.Close()
	defer f.Close()

	sentinel := func(buf []byte, b byte) {
		for i := range buf[:64] {
			buf[i] = b
		}
	}
	// Loose buffer, pinned under a NESTED savepoint so the spill
	// (not the drop, not a loose-pop) must handle it.
	looseID, err := p.AllocPage()
	if err != nil {
		t.Fatal(err)
	}
	looseBuf, err := p.AllocSlab(looseID)
	if err != nil {
		t.Fatal(err)
	}
	sentinel(looseBuf, 0xA1)
	if err := p.FreePage(looseID); err != nil {
		t.Fatal(err)
	}
	sp := p.BeginSavepoint()
	// Live pages past the threshold force the spill.
	var liveBuf []byte
	for i := range 4 {
		id, err := p.AllocPage()
		if err != nil {
			t.Fatal(err)
		}
		buf, err := p.AllocSlab(id)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			liveBuf = buf
			sentinel(liveBuf, 0xB2)
		}
	}
	p.SpillExcess()
	if p.DirtyBytes() > 2*int(testPageSize) {
		t.Fatal("fixture: spill did not run")
	}
	if looseBuf[0] != 0xA1 {
		t.Fatal("loose buffer pool-recycled by the spill (zero-filled under a borrowed slice)")
	}
	if liveBuf[0] != 0xB2 {
		t.Fatal("live buffer pool-recycled by the spill (zero-filled under a borrowed slice)")
	}
	p.ReleaseSavepoint(sp)

	// The loose-DROP path (stack now empty). Allocate the threshold
	// pressure FIRST, then free — no allocation happens between the
	// free and the boundary, so the buffer stays loose (a later
	// AllocPage would loose-pop it into the detach path instead).
	for range 3 {
		id, err := p.AllocPage()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.AllocSlab(id); err != nil {
			t.Fatal(err)
		}
	}
	dropID, err := p.AllocPage()
	if err != nil {
		t.Fatal(err)
	}
	dropBuf, err := p.AllocSlab(dropID)
	if err != nil {
		t.Fatal(err)
	}
	sentinel(dropBuf, 0xC3)
	if err := p.FreePage(dropID); err != nil {
		t.Fatal(err)
	}
	if _, stillLoose := p.LoosePages()[dropID]; !stillLoose {
		t.Fatal("fixture: page not loose at the boundary")
	}
	p.SpillExcess()
	if _, inDirty := p.dirty[dropID]; inDirty {
		t.Fatal("fixture: drop did not process the loose buffer")
	}
	if dropBuf[0] != 0xC3 {
		t.Fatal("loose buffer pool-recycled by the drop (zero-filled under a borrowed slice)")
	}
}

// failingSpillOps fails every WriteAt — the degraded-spill fixture.
type failingSpillOps struct{ FileOps }

func (f failingSpillOps) WriteAt(p []byte, off int64) (int, error) {
	return 0, errors.New("injected spill EIO")
}

// TestDegradedSpillRestoresAdmissionCeiling: after a spill I/O
// failure the relief path is dead for the rest of the tx, so the
// hard admission ceiling returns as the OOM backstop — installs past
// the threshold fail ErrTxTooLarge naming the spill failure.
func TestDegradedSpillRestoresAdmissionCeiling(t *testing.T) {
	p, _, f := setupWriterMaxBytes(t, 64, 2*int(testPageSize))
	defer p.Close()
	defer f.Close()

	restore := p.SetFileOpsForTest(failingSpillOps{p.FileOpsForTest()})
	defer restore()

	// Fill past the threshold; the boundary spill fails and records
	// spillErr.
	for range 3 {
		id, err := p.AllocPage()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.AllocSlab(id); err != nil {
			t.Fatal(err)
		}
	}
	p.SpillExcess()
	if p.SpillError() == nil {
		t.Fatal("fixture: spill did not record its failure")
	}
	// Degraded mode: the next over-threshold install is rejected,
	// naming the spill failure.
	id, err := p.AllocPage()
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.AllocSlab(id)
	if !errors.Is(err, ErrTxTooLarge) {
		t.Fatalf("install in degraded mode = %v, want ErrTxTooLarge", err)
	}
	if !strings.Contains(err.Error(), "injected spill EIO") {
		t.Fatalf("degraded-mode error does not name the spill failure: %v", err)
	}
}

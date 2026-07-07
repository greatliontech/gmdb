package pager

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// initDB creates a fresh database file with chunk-1-friendly defaults and
// returns it (opened via Open + ready for tx work).
func initDB(t *testing.T, pageChecksum bool) (*os.File, *OpenedDB, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ip := InitParams{
		PageSize:        testPageSize,
		PageChecksum:    pageChecksum,
		MinSize:         16,
		MaxSize:         128,
		GrowStep:        4,
		ShrinkThreshold: 8,
		UUID:            [16]byte{0xAA, 0xBB, 0xCC, 0xDD},
	}
	if err := Init(f, ip); err != nil {
		t.Fatalf("Init: %v", err)
	}
	pool := NewBufPool(testPageSize)
	db := openAttachedForTest(t, f, OpenParams{Pool: pool, MaxTxBufferBytes: 16 << 20})
	cleanup := func() {
		_ = db.Pager.Close()
		_ = f.Close()
	}
	return f, db, cleanup
}

func TestInitOpenRoundTrip(t *testing.T) {
	for _, csum := range []bool{false, true} {
		t.Run("csum="+boolStr(csum), func(t *testing.T) {
			_, db, cleanup := initDB(t, csum)
			defer cleanup()

			if db.Meta.Magic != page.Magic {
				t.Errorf("Magic = 0x%x, want 0x%x", db.Meta.Magic, page.Magic)
			}
			if db.Meta.PageSize != testPageSize {
				t.Errorf("PageSize = %d, want %d", db.Meta.PageSize, testPageSize)
			}
			if db.Meta.TxnID != 0 {
				t.Errorf("initial TxnID = %d, want 0", db.Meta.TxnID)
			}
			if db.ActiveMetaIdx != 0 {
				t.Errorf("initial active meta = %d, want 0", db.ActiveMetaIdx)
			}
			// Check checksum flag.
			if csum {
				if !db.Meta.HasFlag(page.MetaFlagPageChecksum) {
					t.Error("PageChecksum flag not set")
				}
			} else {
				if db.Meta.HasFlag(page.MetaFlagPageChecksum) {
					t.Error("PageChecksum flag set despite csum=false")
				}
			}
		})
	}
}

func TestCommitRoundTrip(t *testing.T) {
	for _, csum := range []bool{false, true} {
		t.Run("csum="+boolStr(csum), func(t *testing.T) {
			f, db, cleanup := initDB(t, csum)
			defer cleanup()

			p := db.Pager
			// Allocate a fresh page and CoW some content into it. (For
			// the genesis commit there's nothing to CoW from; use
			// AllocSlab to get a fresh buffer.)
			id, err := p.AllocPage()
			if err != nil {
				t.Fatalf("AllocPage: %v", err)
			}
			buf, err := p.AllocSlab(id)
			if err != nil {
				t.Fatalf("AllocSlab: %v", err)
			}
			// Write a leaf-page-shaped header (Type/Count) plus payload.
			page.WriteHeader(buf, page.TypeLeaf, 1, 0)
			copy(buf[page.HeaderSize:], []byte("hello, gmdb!"))

			p.SetCurrentTxnID(1)
			result, err := p.Commit(CommitParams{
				NewTxnID: 1,
				Flags:    db.Meta.Flags, // preserve PageChecksum + Checkpoint
			}, db.Meta, db.ActiveMetaIdx)
			if err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if result.Meta.TxnID != 1 {
				t.Errorf("post-commit TxnID = %d, want 1", result.Meta.TxnID)
			}
			if result.ActiveMetaIdx != 1 {
				t.Errorf("active meta = %d, want 1", result.ActiveMetaIdx)
			}
			if result.Meta.HighWaterMark <= db.Meta.HighWaterMark {
				t.Errorf("HWM did not advance: %d -> %d", db.Meta.HighWaterMark, result.Meta.HighWaterMark)
			}

			// Re-open and verify the page is durable.
			_ = p.Close()
			pool := NewBufPool(testPageSize)
			db2 := openAttachedForTest(t, f, OpenParams{Pool: pool, MaxTxBufferBytes: 16 << 20})
			defer db2.Pager.Close()

			if db2.Meta.TxnID != 1 {
				t.Errorf("re-opened TxnID = %d, want 1", db2.Meta.TxnID)
			}
			if db2.ActiveMetaIdx != 1 {
				t.Errorf("re-opened active = %d, want 1", db2.ActiveMetaIdx)
			}
			gotPage := db2.Pager.pageRaw(id)
			typ, _, count, _ := page.ReadHeader(gotPage)
			if typ != page.TypeLeaf || count != 1 {
				t.Errorf("re-opened page header: typ=%d count=%d", typ, count)
			}
			if !bytes.HasPrefix(gotPage[page.HeaderSize:], []byte("hello, gmdb!")) {
				t.Errorf("page payload not durable")
			}
			// Re-allocate on the re-opened DB and verify it does NOT
			// return `id` (which is in-use). Catches a class of bug
			// where the bitmap pwrite is silently dropped or the
			// dirty-set tracking misses a Clear: in either case the
			// re-built bitmap would think `id` is free.
			db2.Pager.SetCurrentTxnID(result.Meta.TxnID + 1)
			next, err := db2.Pager.AllocPage()
			if err != nil {
				t.Fatalf("post-reopen AllocPage: %v", err)
			}
			if next == id {
				t.Errorf("post-reopen AllocPage returned in-use page %d — bitmap pwrite dropped or dirty-set missed it", id)
			}
		})
	}
}

func TestCommitRetiresPageToRPL(t *testing.T) {
	f, db, cleanup := initDB(t, true)
	defer cleanup()
	p := db.Pager

	// First commit: allocate page A and populate it.
	idA, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage A: %v", err)
	}
	bufA, err := p.AllocSlab(idA)
	if err != nil {
		t.Fatalf("AllocSlab A: %v", err)
	}
	page.WriteHeader(bufA, page.TypeLeaf, 1, 0)
	copy(bufA[page.HeaderSize:], []byte("ver1"))
	p.SetCurrentTxnID(1)
	r1, err := p.Commit(CommitParams{NewTxnID: 1, Flags: db.Meta.Flags}, db.Meta, db.ActiveMetaIdx)
	if err != nil {
		t.Fatalf("Commit 1: %v", err)
	}

	// Second commit: retire page A and allocate page B.
	p.SetCommitState(r1.Meta.HighWaterMark, r1.Meta.MaxSize, r1.Meta.TxnID)
	if err := p.FreePage(idA); err != nil {
		t.Fatalf("FreePage A: %v", err)
	}
	idB, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage B: %v", err)
	}
	bufB, err := p.AllocSlab(idB)
	if err != nil {
		t.Fatalf("AllocSlab B: %v", err)
	}
	page.WriteHeader(bufB, page.TypeLeaf, 1, 0)
	copy(bufB[page.HeaderSize:], []byte("ver2"))
	p.SetCurrentTxnID(2)
	r2, err := p.Commit(CommitParams{NewTxnID: 2, Flags: r1.Meta.Flags}, r1.Meta, r1.ActiveMetaIdx)
	if err != nil {
		t.Fatalf("Commit 2: %v", err)
	}
	if r2.Meta.RPLHeadPage == 0 {
		t.Error("RPL head not set after retire")
	}
	if r2.Meta.RPLEntryCount != 1 {
		t.Errorf("RPL entry count = %d, want 1", r2.Meta.RPLEntryCount)
	}

	// Re-open and verify RPL chain rebuilt correctly.
	_ = p.Close()
	pool := NewBufPool(testPageSize)
	db3 := openAttachedForTest(t, f, OpenParams{Pool: pool, MaxTxBufferBytes: 16 << 20})
	defer db3.Pager.Close()
	chain := db3.Pager.RPLChain()
	if len(chain) != 1 {
		t.Fatalf("RPL chain length = %d, want 1", len(chain))
	}
	if chain[0].TxnID != 2 {
		t.Errorf("RPL seg TxnID = %d, want 2", chain[0].TxnID)
	}
	if chain[0].Count != 1 {
		t.Errorf("RPL seg count = %d, want 1", chain[0].Count)
	}
}

func TestCommitRejectsNonStrictlyIncreasingTxnID(t *testing.T) {
	_, db, cleanup := initDB(t, false)
	defer cleanup()
	p := db.Pager

	// Allocate something so commit has work to do.
	_, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	p.SetCurrentTxnID(0) // same as prev — should be rejected
	// Genesis-commit special case: prev.TxnID=0 and NewTxnID=1 is OK.
	// Anything else where NewTxnID <= prev.TxnID is rejected.
	_, err = p.Commit(CommitParams{NewTxnID: 0, Flags: db.Meta.Flags}, db.Meta, db.ActiveMetaIdx)
	if err == nil {
		t.Fatal("Commit accepted NewTxnID == prev.TxnID")
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestCommitStep0DropsFreedPagesFromWriteSet pins the freed-page
// write-set sweep (pager-slab.md §Step 0, grounded in free-space.md's
// loose-page invariant): a page allocated and
// freed within the same transaction is unreachable from every meta —
// it bypasses the RPL and goes straight to free-in-bitmap — so its
// slab buffer must be dropped from p.dirty at commit assembly, not
// pwritten. The buffer deliberately survives in p.dirty until step 0
// (an open shallow savepoint could resurrect the op that freed it);
// step 0 runs with every savepoint resolved.
func TestCommitStep0DropsFreedPagesFromWriteSet(t *testing.T) {
	p, _, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()
	p.SetCurrentTxnID(1)

	// Three pages: `inner` freed below the surviving tail (the
	// loose→pendingFrees path), `kept` surviving, `tail` freed at the
	// tail (FreePage sets the free bit immediately, so step 0's
	// TailRefund refunds it past the new HWM).
	alloc := func(name string) uint64 {
		id, err := p.AllocPage()
		if err != nil {
			t.Fatalf("AllocPage(%s): %v", name, err)
		}
		if _, err := p.AllocSlab(id); err != nil {
			t.Fatalf("AllocSlab(%s): %v", name, err)
		}
		return id
	}
	inner := alloc("inner")
	kept := alloc("kept")
	tail := alloc("tail")
	if err := p.FreePage(inner); err != nil {
		t.Fatalf("FreePage(inner): %v", err)
	}
	if err := p.FreePage(tail); err != nil {
		t.Fatalf("FreePage(tail): %v", err)
	}

	if err := p.commitStep0(); err != nil {
		t.Fatalf("commitStep0: %v", err)
	}
	for name, id := range map[string]uint64{"inner": inner, "tail": tail} {
		if _, ok := p.dirty[id]; ok {
			t.Errorf("%s freed page %d still in the write set after step 0 (write amplification)", name, id)
		}
	}
	if _, ok := p.dirty[kept]; !ok {
		t.Errorf("kept page %d missing from the write set after step 0", kept)
	}
	if _, ok := p.pendingFrees[inner]; !ok {
		t.Errorf("inner freed page %d not in pendingFrees after step 0", inner)
	}
	if hwm := p.highWaterMark; tail < hwm {
		t.Errorf("tail page %d below post-refund HWM %d (fixture expected a refund)", tail, hwm)
	}
}

// openAttachedForTest is the test-side composition of the two-phase
// writable open: pager.Open (unattached) + AttachLatest. Production
// callers route the attach decision through the recovery-commit gate
// (db.Open) or hold the grant already (Compact); tests are
// single-process with no gate to consult.
func openAttachedForTest(t *testing.T, f *os.File, op OpenParams) *OpenedDB {
	t.Helper()
	od, err := Open(f, op)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m, idx, err := od.Pager.AttachLatest(f)
	if err != nil {
		t.Fatalf("AttachLatest: %v", err)
	}
	od.Meta, od.ActiveMetaIdx = m, idx
	return od
}

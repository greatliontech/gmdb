package pager

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thegrumpylion/gmdb/internal/page"
)

// fillPattern writes a recognisable per-page byte pattern into buf so a
// read-back can prove the exact page (not a neighbour) landed on disk.
func fillPattern(buf []byte, seed byte) {
	for i := range buf {
		buf[i] = seed ^ byte(i)
	}
}

// TestWriteDirectPersistsToFile verifies a directly-written page lands on
// disk at the page's byte offset, bypassing the slab (no dirty entry).
func TestWriteDirectPersistsToFile(t *testing.T) {
	p, _, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	buf := make([]byte, testPageSize)
	fillPattern(buf, 0x5A)

	if err := p.WriteDirect(id, buf); err != nil {
		t.Fatalf("WriteDirect: %v", err)
	}
	// The page must NOT be in the slab — that is the whole point of the
	// bypass (no MaxTxBufferBytes charge).
	if _, dirty := p.dirty[id]; dirty {
		t.Error("WriteDirect installed a slab buffer; bypass violated")
	}
	if p.dirtyBytes != 0 {
		t.Errorf("dirtyBytes = %d after WriteDirect, want 0 (slab bypass)", p.dirtyBytes)
	}

	// Read straight back from the file at the page offset.
	got := make([]byte, testPageSize)
	if _, err := f.ReadAt(got, int64(id)*int64(testPageSize)); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, buf) {
		t.Error("on-disk page bytes do not match the WriteDirect buffer")
	}
}

// TestWriteDirectWritesChecksumFooter verifies the xxhash64 footer is
// written in place when PageChecksum is enabled, matching commitStep1.
func TestWriteDirectWritesChecksumFooter(t *testing.T) {
	p, _, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()
	p.cfg.PageChecksum = true

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	buf := make([]byte, testPageSize)
	fillPattern(buf, 0x33)

	if err := p.WriteDirect(id, buf); err != nil {
		t.Fatalf("WriteDirect: %v", err)
	}

	got := make([]byte, testPageSize)
	if _, err := f.ReadAt(got, int64(id)*int64(testPageSize)); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !page.VerifyPageFooter(got, testPageSize) {
		t.Error("on-disk page has no valid checksum footer")
	}
	// The content region (everything but the footer) must equal the
	// pre-footer bytes the caller supplied.
	body := testPageSize - page.FooterSize
	if !bytes.Equal(got[:body], buf[:body]) {
		t.Error("content region altered beyond the footer")
	}
}

// TestWriteDirectRejectsUnallocatedPage verifies WriteDirect refuses an
// id that this transaction never allocated (Inv-WD: writing to an
// unreserved page could clobber a page reachable from the active meta).
func TestWriteDirectRejectsUnallocatedPage(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	id := bm.FirstDataPage() // never AllocPage'd
	buf := make([]byte, testPageSize)
	fillPattern(buf, 0x77)

	if err := p.WriteDirect(id, buf); err == nil {
		t.Fatal("WriteDirect to unallocated page returned nil, want error")
	}
	// Nothing must have been written.
	got := make([]byte, testPageSize)
	if _, err := f.ReadAt(got, int64(id)*int64(testPageSize)); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, make([]byte, testPageSize)) {
		t.Error("rejected WriteDirect still mutated the file")
	}
}

// TestWriteDirectRejectsSlabPage verifies WriteDirect refuses an id that
// is also in the slab (Inv-WD: commitStep1 would re-pwrite the slab
// buffer over the direct content). Wraps ErrCorrupted.
func TestWriteDirectRejectsSlabPage(t *testing.T) {
	p, _, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if _, err := p.AllocSlab(id); err != nil {
		t.Fatalf("AllocSlab: %v", err)
	}
	buf := make([]byte, testPageSize)
	err = p.WriteDirect(id, buf)
	if err == nil {
		t.Fatal("WriteDirect to a slab page returned nil, want error")
	}
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("WriteDirect slab-page error = %v, want wrapped ErrCorrupted", err)
	}
}

// TestWriteDirectRejectsWrongSize verifies a buf whose length is not
// exactly PageSize is rejected before any write.
func TestWriteDirectRejectsWrongSize(t *testing.T) {
	p, _, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if err := p.WriteDirect(id, make([]byte, testPageSize-1)); err == nil {
		t.Fatal("WriteDirect with short buf returned nil, want error")
	}
	if err := p.WriteDirect(id, make([]byte, testPageSize+1)); err == nil {
		t.Fatal("WriteDirect with long buf returned nil, want error")
	}
}

// TestWriteDirectReadOnlyReturnsErrReadOnly verifies a read-only pager
// refuses WriteDirect.
func TestWriteDirectReadOnlyReturnsErrReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ro.gmdb")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(4 * int64(testPageSize)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	rp, err := NewReader(f, page.Config{PageSize: testPageSize}, 4*int64(testPageSize))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer rp.Close()

	if err := rp.WriteDirect(0, make([]byte, testPageSize)); !errors.Is(err, ErrReadOnly) {
		t.Errorf("WriteDirect on read-only pager = %v, want ErrReadOnly", err)
	}
}

// TestWriteDirectAbortReversesAllocation verifies the bounded-leakage
// property at the pager layer: after AbortTx, the directly-written page's
// bitmap bit reverts to free (reusable). The on-disk content is harmless
// stale bytes in a page no recoverable meta references.
func TestWriteDirectAbortReversesAllocation(t *testing.T) {
	p, bm, f := setupWriter(t, 16)
	defer p.Close()
	defer f.Close()

	first := bm.FirstDataPage()
	bm.Set(first + 5) // mark free so AllocPage takes the bitmap path
	p.BeginTx(TxParams{HighWaterMark: first, MaxSize: 16})

	id, err := p.AllocPage()
	if err != nil {
		t.Fatalf("AllocPage: %v", err)
	}
	if id != first+5 {
		t.Fatalf("AllocPage = %d, want %d", id, first+5)
	}
	if bm.IsSet(id) {
		t.Fatal("bit still free after AllocPage")
	}
	buf := make([]byte, testPageSize)
	fillPattern(buf, 0x11)
	if err := p.WriteDirect(id, buf); err != nil {
		t.Fatalf("WriteDirect: %v", err)
	}

	p.AbortTx()

	if !bm.IsSet(id) {
		t.Error("after AbortTx the directly-written page's bit was not restored to free")
	}
	// And it is handed out again on the next allocation (reusable).
	bm.Set(first + 5)
	p.BeginTx(TxParams{HighWaterMark: first, MaxSize: 16})
	reID, err := p.AllocPage()
	if err != nil {
		t.Fatalf("post-abort AllocPage: %v", err)
	}
	if reID != id {
		t.Errorf("post-abort AllocPage = %d, want reuse of %d", reID, id)
	}
}

// TestWriteDirectSurvivesCommit verifies a directly-written page is
// durable after commit (made stable by the step-2 whole-file fdatasync,
// never double-written by commitStep1) and that its bitmap bit is
// committed as in-use so a re-opened DB will not re-hand-out the id.
func TestWriteDirectSurvivesCommit(t *testing.T) {
	for _, csum := range []bool{false, true} {
		t.Run("csum="+boolStr(csum), func(t *testing.T) {
			f, db, cleanup := initDB(t, csum)
			defer cleanup()

			p := db.Pager
			// Match production fidelity: every commit is preceded by
			// BeginTx (Commit's snapshot-discard path assumes it).
			p.BeginTx(TxParams{
				HighWaterMark: db.Meta.HighWaterMark,
				MaxSize:       db.Meta.MaxSize,
				GrowStep:      db.Meta.GrowStep,
				MinSize:       db.Meta.MinSize,
				TxnID:         1,
			})
			id, err := p.AllocPage()
			if err != nil {
				t.Fatalf("AllocPage: %v", err)
			}
			buf := make([]byte, testPageSize)
			// A leaf-shaped header so a checksum-on read is structurally
			// plausible; the payload is what we assert on.
			page.WriteHeader(buf, page.TypeLeaf, 1, 0)
			copy(buf[page.HeaderSize:], []byte("direct-write payload"))

			if err := p.WriteDirect(id, buf); err != nil {
				t.Fatalf("WriteDirect: %v", err)
			}
			if _, dirty := p.dirty[id]; dirty {
				t.Fatal("WriteDirect page leaked into the slab")
			}

			if _, err := p.Commit(CommitParams{NewTxnID: 1, Flags: db.Meta.Flags}, db.Meta, db.ActiveMetaIdx); err != nil {
				t.Fatalf("Commit: %v", err)
			}

			// Re-open and verify durability + bitmap state.
			_ = p.Close()
			pool := NewBufPool(testPageSize)
			db2, err := Open(f, OpenParams{Pool: pool, MaxTxBufferBytes: 16 << 20})
			if err != nil {
				t.Fatalf("re-Open: %v", err)
			}
			defer db2.Pager.Close()

			got := db2.Pager.pageRaw(id)
			if !bytes.HasPrefix(got[page.HeaderSize:], []byte("direct-write payload")) {
				t.Error("directly-written payload not durable after commit")
			}
			if csum && !page.VerifyPageFooter(got, testPageSize) {
				t.Error("directly-written page failed checksum verification after re-open")
			}
			db2.Pager.SetCurrentTxnID(2)
			next, err := db2.Pager.AllocPage()
			if err != nil {
				t.Fatalf("post-reopen AllocPage: %v", err)
			}
			if next == id {
				t.Errorf("post-reopen AllocPage returned in-use page %d — direct page's bitmap bit was not committed in-use", id)
			}
		})
	}
}

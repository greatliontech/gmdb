package gmdb

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestUpdatePanicReleasesGrant: a panic in Update's fn must still release the
// cross-process write grant (api-surface.md §Update panic safety). Pre-fix the
// panic unwound past tx.Commit/tx.Rollback, leaking the grant until GC
// finalized the *Tx — blocking every subsequent writer (this process and any
// peer) on AcquireWriter. Here a leaked grant makes the follow-up Update hang
// until the deadline.
func TestUpdatePanicReleasesGrant(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("expected the panic to propagate out of Update")
			}
		}()
		_ = db.Update(ctx, func(tx *Tx) error { panic("boom in fn") })
	}()

	// The grant must be free now; a follow-up Update must complete promptly.
	// If it leaked, AcquireWriter blocks until this deadline fires.
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := db.Update(ctx2, func(tx *Tx) error {
		ks, e := tx.CreateKeyspace("ks")
		if e != nil {
			return e
		}
		return ks.Put([]byte("k"), []byte("v"))
	}); err != nil {
		t.Fatalf("post-panic Update failed — write grant leaked: %v", err)
	}
}

// TestViewPanicReleasesReaderSlot: a panic in View's fn must release the
// reader-table slot (api-surface.md §View panic safety). With MaxReaders=1 a
// leaked slot makes the next View fail with ErrReadersFull (pre-fix, until GC);
// with the fix the slot is freed immediately.
func TestViewPanicReleasesReaderSlot(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, tmpPath(t), Options{PageSize: 4096, MinSize: 16, MaxSize: 256, MaxReaders: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("expected the panic to propagate out of View")
			}
		}()
		_ = db.View(ctx, func(rtx *ReadTx) error { panic("boom in fn") })
	}()

	// The single slot must be free again.
	if err := db.View(ctx, func(rtx *ReadTx) error { return nil }); err != nil {
		if errors.Is(err, ErrReadersFull) {
			t.Fatalf("post-panic View hit ErrReadersFull — reader slot leaked")
		}
		t.Fatalf("post-panic View failed: %v", err)
	}
}

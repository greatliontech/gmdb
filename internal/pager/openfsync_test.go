package pager

import (
	"errors"
	"os"
	"testing"
	"time"
)

// buildLazyImage produces an opened DB whose on-disk state is NOT
// self-durable (SyncNone commits carrying the genesis epoch forward)
// so the gated recovery path takes the recovery-commit arm.
func buildLazyImage(t *testing.T) (*OpenedDB, func()) {
	t.Helper()
	f, db, cleanup := initDB(t, false)
	_ = f
	p := db.Pager
	prev, prevActive := db.Meta, db.ActiveMetaIdx
	for txn := uint64(1); txn <= 2; txn++ {
		p.BeginTx(TxParams{
			HighWaterMark: prev.HighWaterMark, MaxSize: prev.MaxSize,
			GrowStep: prev.GrowStep, MinSize: prev.MinSize, TxnID: txn,
			ReclamationBound: func() uint64 { return 0 },
		})
		id, err := p.AllocPage()
		if err != nil {
			t.Fatalf("AllocPage: %v", err)
		}
		if _, err := p.AllocSlab(id); err != nil {
			t.Fatalf("AllocSlab: %v", err)
		}
		res, err := p.Commit(CommitParams{NewTxnID: txn, Flags: prev.Flags, Sync: SyncNone}, prev, prevActive)
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		prev, prevActive = res.Meta, res.ActiveMetaIdx
	}
	db.Meta, db.ActiveMetaIdx = prev, prevActive
	return db, cleanup
}

// TestRecoveryCommitFsyncFailurePropagatesAndRetries pins the
// recovery-commit fsync failure path (durability.md §Recovery step 5,
// idempotent-under-crash): a failing fsync surfaces the error with
// nothing anchored; a retried RecoverToDurable re-runs the whole
// sequence and succeeds.
func TestRecoveryCommitFsyncFailurePropagatesAndRetries(t *testing.T) {
	db, cleanup := buildLazyImage(t)
	defer cleanup()
	p := db.Pager
	file := p.file

	injected := errors.New("injected recovery-commit fsync failure")
	restore := SetOpenFsyncHookForTest(func(op string) error {
		if op == "recovery-commit" {
			return injected
		}
		return nil
	})
	_, _, _, err := p.RecoverToDurable(file)
	restore()
	if !errors.Is(err, injected) {
		t.Fatalf("RecoverToDurable error = %v, want the injected failure", err)
	}
	if got := p.AnchoredEpoch(); got != 0 {
		t.Fatalf("anchored epoch = %d after failed fsync, want 0 (nothing anchored)", got)
	}

	// Retry after the fault clears. In-process the failed attempt's
	// pwrite is already visible (page cache), so the re-read under
	// the grant selects the recovery meta and the retry takes the
	// ANCHOR arm — the equivalent end state (after a real crash the
	// pwrite is lost and the recovery-commit arm re-runs; both roads
	// end at the same durable state, which is the idempotence the
	// spec claims). Assert the STATE, not the arm.
	m, _, _, err := p.RecoverToDurable(file)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if m.TxnID != db.Meta.TxnID+1 || !m.SelfDurable() {
		t.Fatalf("retry: TxnID=%d SelfDurable=%v, want %d/true",
			m.TxnID, m.SelfDurable(), db.Meta.TxnID+1)
	}
	if got := p.AnchoredEpoch(); got != m.TxnID {
		t.Fatalf("anchored epoch = %d after retry, want %d", got, m.TxnID)
	}
}

// TestAnchorFsyncFailurePropagates pins the self-durable arm's anchor
// fsync failure: the error surfaces and the anchored epoch does not
// advance — a failed fsync must never anchor the assertion it was
// supposed to make disk-fast (durability.md §Anchoring).
func TestAnchorFsyncFailurePropagates(t *testing.T) {
	f, db, cleanup := initDB(t, false)
	_ = f
	defer cleanup()
	p := db.Pager
	// Genesis is self-durable at epoch 0 — the gated path takes the
	// anchor arm.
	injected := errors.New("injected anchor fsync failure")
	restore := SetOpenFsyncHookForTest(func(op string) error {
		if op == "anchor" {
			return injected
		}
		return nil
	})
	_, _, _, err := p.RecoverToDurable(p.file)
	restore()
	if !errors.Is(err, injected) {
		t.Fatalf("RecoverToDurable error = %v, want the injected failure", err)
	}

	// Retry succeeds and anchors epoch 0's assertion.
	m, _, recovered, err := p.RecoverToDurable(p.file)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if recovered || !m.SelfDurable() {
		t.Fatalf("retry: recovered=%v SelfDurable=%v, want false/true (anchor arm)", recovered, m.SelfDurable())
	}
}

// TestAnchorArmRewritesTheSlot is the mtime-sentinel pin of the anchor
// REWRITE (durability.md §Anchoring: the byte-identical meta pwrite
// before the anchor fsync is load-bearing — a prior failed fsync both
// consumes the kernel error and marks pages clean, so a bare fdatasync
// would anchor an assertion the disk never received). The rewrite has
// no data-observable effect (identical bytes), but POSIX mandates
// write(2) marks st_mtime for update while fdatasync does not: reset
// mtime to a sentinel epoch, run the self-durable arm, and a changed
// mtime proves the write executed.
func TestAnchorArmRewritesTheSlot(t *testing.T) {
	f, db, cleanup := initDB(t, false)
	defer cleanup()
	p := db.Pager

	sentinel := time.Unix(1000000, 0)
	if err := os.Chtimes(f.Name(), sentinel, sentinel); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if _, _, recovered, err := p.RecoverToDurable(p.file); err != nil || recovered {
		t.Fatalf("RecoverToDurable: recovered=%v err=%v, want anchor arm", recovered, err)
	}
	st, err := os.Stat(f.Name())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.ModTime().Equal(sentinel) {
		t.Fatal("mtime unchanged across the anchor arm — the load-bearing slot rewrite did not execute")
	}
}
